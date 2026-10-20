package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A PR is opened as a draft and is published as ready only when the run that
// owns it has actually finished the job: every step passed on the final head,
// the checks are green, no review comment is waiting for a reply, and the
// description was regenerated for that final state. Anything else, including
// an answer the forge could not give, leaves it a draft.
//
// Two places apply it. The PR step puts an existing PR back to draft when a
// new run starts validating it (a new push, a rerun). FinalizeRun marks it
// ready, or leaves it draft, when the run ends. In between, the CI step never
// publishes: a red check or unreplied feedback can only find a PR that is
// still a draft.

func draftController(host scm.Host) (scm.PRDraftController, bool) {
	controller, ok := host.(scm.PRDraftController)
	if !ok || !controller.DraftLifecycle() {
		return nil, false
	}
	return controller, true
}

// holdPRAsDraft puts the PR into draft. Best effort: the run's end converges
// the state again, so a forge hiccup here must not fail an otherwise good run.
func holdPRAsDraft(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) {
	controller, ok := draftController(host)
	if !ok || pr == nil {
		return
	}
	if err := setDraftState(sctx.Ctx, controller, pr, true); err != nil {
		slog.Warn("failed to hold pull request as draft", "run", sctx.Run.ID, "error", err)
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("could not mark the pull request as a draft: %v", err))
		}
	}
}

// setDraftState moves the PR to the wanted state, writing only when it differs.
func setDraftState(ctx context.Context, controller scm.PRDraftController, pr *scm.PR, draft bool) error {
	current, err := controller.GetPRDraft(ctx, pr)
	if err != nil {
		return err
	}
	if current == draft {
		return nil
	}
	return controller.SetPRDraft(ctx, pr, draft)
}

// prReadiness reports whether the finished run justifies a ready PR, and if
// not, why. An error answering any question is "not ready".
func prReadiness(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, steps []*db.StepResult, descriptionCurrent bool) (bool, string) {
	if sctx.Run.Status != types.RunCompleted {
		return false, fmt.Sprintf("run %s", sctx.Run.Status)
	}
	if !descriptionCurrent {
		return false, "description was not regenerated for the final state"
	}
	for _, sr := range steps {
		if sr.OverrideReason != nil && *sr.OverrideReason != "" {
			return false, fmt.Sprintf("%s was approved over an unresolved condition", stepDisplayName(sr.StepName))
		}
		switch sr.Status {
		case types.StepStatusCompleted, types.StepStatusSkipped:
		default:
			return false, fmt.Sprintf("%s is %s", stepDisplayName(sr.StepName), sr.Status)
		}
	}
	checks, err := host.GetChecks(sctx.Ctx, pr)
	if err != nil {
		return false, fmt.Sprintf("checks unreadable: %v", err)
	}
	for _, check := range checks {
		switch check.Bucket {
		case scm.CheckBucketPass, scm.CheckBucketSkip:
		default:
			return false, fmt.Sprintf("check %s is %s", check.Name, check.Bucket)
		}
	}
	if comments, ok := host.(scm.ReviewCommentsHost); ok && host.Capabilities().ReviewComments {
		open, err := comments.GetReviewComments(sctx.Ctx, pr)
		if err != nil {
			return false, fmt.Sprintf("review comments unreadable: %v", err)
		}
		if len(open) > 0 {
			return false, fmt.Sprintf("%d review comment(s) have no reply", len(open))
		}
	}
	return true, ""
}

// syncPRDraftState publishes the PR as ready when prReadiness allows it and
// holds it as a draft otherwise.
func syncPRDraftState(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, descriptionCurrent bool) error {
	controller, ok := draftController(host)
	if !ok {
		return nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return errors.Join(fmt.Errorf("read steps: %w", err), setDraftState(sctx.Ctx, controller, pr, true))
	}
	ready, why := prReadiness(sctx, host, pr, steps, descriptionCurrent)
	if !ready {
		sctx.Log(fmt.Sprintf("holding the pull request as a draft: %s", why))
	} else {
		sctx.Log("pull request is ready for review")
	}
	return setDraftState(sctx.Ctx, controller, pr, !ready)
}
