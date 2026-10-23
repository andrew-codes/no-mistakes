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

// draftAwaitsPublication reports whether the PR is a draft that only this run's
// end can publish: it is a draft under the draft lifecycle. Bitbucket does not
// evaluate a draft's git mergeability check, so twg lists it PENDING (or omits
// it) until the PR is published, and the lifecycle publishes only after the CI
// step has ended. A monitor that waited for that reading, or for the draft to
// merge, would wait on the thing it is holding back. An unanswerable draft
// state counts as not a draft: not knowing is no reason to stop waiting.
func draftAwaitsPublication(ctx context.Context, host scm.Host, pr *scm.PR) bool {
	controller, ok := draftController(host)
	if !ok || pr == nil {
		return false
	}
	draft, err := controller.GetPRDraft(ctx, pr)
	return err == nil && draft
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
	// checks_passed is the opt-in release verdict (ci.release_after_checks_pass):
	// the run proved the same things and left the merge to a human.
	if sctx.Run.Status != types.RunCompleted && sctx.Run.Status != types.RunChecksPassed {
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
	// The CI monitor cannot read a draft's mergeability, so it could not rule a
	// conflict out. Once the forge does report one, publishing would put a PR
	// that cannot merge in front of a reviewer. A reading that is merely
	// pending is the draft's usual state and proves nothing either way.
	if host.Capabilities().MergeableState {
		state, err := host.GetMergeableState(sctx.Ctx, pr)
		if err != nil {
			return false, fmt.Sprintf("mergeability unreadable: %v", err)
		}
		if state.Conflict() {
			return false, "the pull request has a merge conflict or is too far behind its base"
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
