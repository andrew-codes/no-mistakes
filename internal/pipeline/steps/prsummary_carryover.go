package steps

import (
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A rerun that only refreshes a PR description skips the steps that already
// passed, and the description is then composed from this run's own rows: Test
// "skipped", Document "skipped", Lint "skipped". That throws away what the same
// head was already validated against - the accepted Test exception with its
// totals, the live-validation verdict, which steps passed.
//
// The description therefore shows, for each step this run skipped, the result
// the most recent earlier run on the same branch and the SAME head recorded
// for it, labelled as carried over. It is a rendering rule only. The run's rows
// and the pipeline attestation keep saying "skipped": the attestation is what
// other tooling decides on, and a step this run did not execute is not one it
// attests. A step whose head moved is simply reported as skipped, because the
// earlier run validated different code.

// carriedOverNote labels a result shown in place of a skipped step.
const carriedOverNote = "carried over from the prior run on the same head"

// carriedStep is an earlier run's recorded result for a step this run skipped.
type carriedStep struct {
	Step   *db.StepResult
	Rounds []*db.StepRound
}

// viewOf returns the step, rounds and label to render for sr: the carried
// result when there is one, and sr itself, unlabelled, otherwise. It is safe
// on a nil receiver, which is how a step with nothing carried reads.
func (c *carriedStep) viewOf(sr *db.StepResult, rounds []*db.StepRound) (*db.StepResult, []*db.StepRound, string) {
	if c == nil {
		return sr, rounds, ""
	}
	return c.Step, c.Rounds, carriedOverNote
}

// carriedView is steps with each carried step swapped for its earlier result,
// for the readings that describe the PR's validation rather than this run's
// execution.
func carriedView(steps []*db.StepResult, carried map[string]*carriedStep) []*db.StepResult {
	if len(carried) == 0 {
		return steps
	}
	view := make([]*db.StepResult, len(steps))
	for i, sr := range steps {
		view[i] = sr
		if c := carried[sr.ID]; c != nil {
			view[i] = c.Step
		}
	}
	return view
}

// earlierSameHeadRuns returns the runs on this run's repository, branch and
// head that were created before it, newest first. An error reads as no earlier
// run, so a caller can only fall back to reporting what this run did.
func earlierSameHeadRuns(sctx *pipeline.StepContext) []*db.Run {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.Repo == nil {
		return nil
	}
	head := strings.TrimSpace(sctx.Run.HeadSHA)
	if head == "" {
		return nil
	}
	same, err := sctx.DB.GetRunsByRepoHead(sctx.Repo.ID, sctx.Run.Branch, head)
	if err != nil {
		slog.Warn("failed to read earlier runs on this head; skipped steps stay reported as skipped", "run", sctx.Run.ID, "error", err)
		return nil
	}
	var earlier []*db.Run
	for _, r := range same {
		if r.ID == sctx.Run.ID {
			continue
		}
		if r.CreatedAt < sctx.Run.CreatedAt || (r.CreatedAt == sctx.Run.CreatedAt && r.ID < sctx.Run.ID) {
			earlier = append(earlier, r)
		}
	}
	return earlier
}

// carriedSkippedSteps finds, for every step this run skipped on request, the
// completed result the newest earlier run on the same head recorded. A step an
// earlier run also skipped, failed or never reached is looked for in the run
// before that, so a chain of refreshes keeps the original result. Nothing is
// carried for a step skipped for a recorded automatic reason.
func carriedSkippedSteps(sctx *pipeline.StepContext, steps []*db.StepResult) map[string]*carriedStep {
	wanted := map[types.StepName]*db.StepResult{}
	for _, sr := range steps {
		if sr == nil || sr.Status != types.StepStatusSkipped || shouldOmitPipelineStep(sr) {
			continue
		}
		if sr.SkipReason != nil && strings.TrimSpace(*sr.SkipReason) != "" {
			continue
		}
		wanted[sr.StepName] = sr
	}
	if len(wanted) == 0 {
		return nil
	}
	carried := map[string]*carriedStep{}
	for _, prior := range earlierSameHeadRuns(sctx) {
		if len(wanted) == 0 {
			break
		}
		priorSteps, err := sctx.DB.GetStepsByRun(prior.ID)
		if err != nil {
			slog.Warn("failed to read an earlier run's steps; skipped steps stay reported as skipped", "run", sctx.Run.ID, "error", err)
			return nil
		}
		for _, ps := range priorSteps {
			sr := wanted[ps.StepName]
			if sr == nil || ps.Status != types.StepStatusCompleted {
				continue
			}
			rounds, err := sctx.DB.GetRoundsByStep(ps.RoundsStepID())
			if err != nil {
				slog.Warn("failed to read an earlier run's rounds; skipped steps stay reported as skipped", "run", sctx.Run.ID, "step", ps.StepName, "error", err)
				return nil
			}
			copied := *ps
			carried[sr.ID] = &carriedStep{Step: &copied, Rounds: rounds}
			delete(wanted, ps.StepName)
		}
	}
	if len(carried) == 0 {
		return nil
	}
	return carried
}

// riskRank orders the review's risk levels. An unknown level ranks as none.
func riskRank(level string) int {
	switch level {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	}
	return 0
}

// reviewFindingsOf is a review step's last recorded findings: the step's own
// when readable, otherwise its latest round's. It reports false when neither
// is readable, so a caller never reasons from a guess.
func reviewFindingsOf(sr *db.StepResult, rounds []*db.StepRound) (types.Findings, bool) {
	if sr.FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
			return f, true
		}
		return types.Findings{}, false
	}
	if len(rounds) > 0 && rounds[len(rounds)-1].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[len(rounds)-1].FindingsJSON); err == nil {
			return f, true
		}
	}
	return types.Findings{}, false
}

// carriedRiskLine replaces the Risk line of a refresh run whose Review covered
// less of the PR than an earlier review of the same head did. A refresh's
// Review is cheap and often scoped to whatever the intent mentions, so its
// Risk read "the latest commit only" while a fuller review of the same code
// already stood. The earlier line is used only when this run skipped a step
// (it is a refresh), the earlier review certified every path this run's did
// and more, and its risk level is no lower, so the line is never softened.
func carriedRiskLine(sctx *pipeline.StepContext, steps []*db.StepResult, rounds map[string][]*db.StepRound, current string) string {
	if current == "" {
		return current
	}
	var review *db.StepResult
	skipped := false
	for _, sr := range steps {
		if sr == nil {
			continue
		}
		if sr.Status == types.StepStatusSkipped && !shouldOmitPipelineStep(sr) {
			skipped = true
		}
		if sr.StepName == types.StepReview {
			review = sr
		}
	}
	if !skipped || review == nil {
		return current
	}
	now, ok := reviewFindingsOf(review, rounds[review.ID])
	if !ok || now.RiskLevel == "" {
		return current
	}
	covered := map[string]bool{}
	for _, p := range now.ReviewedPaths {
		covered[p] = true
	}
	for _, prior := range earlierSameHeadRuns(sctx) {
		priorSteps, err := sctx.DB.GetStepsByRun(prior.ID)
		if err != nil {
			return current
		}
		for _, ps := range priorSteps {
			if ps.StepName != types.StepReview || ps.Status != types.StepStatusCompleted {
				continue
			}
			priorRounds, err := sctx.DB.GetRoundsByStep(ps.RoundsStepID())
			if err != nil {
				return current
			}
			then, ok := reviewFindingsOf(ps, priorRounds)
			if !ok || then.RiskLevel == "" || riskRank(then.RiskLevel) < riskRank(now.RiskLevel) {
				continue
			}
			if len(then.ReviewedPaths) <= len(now.ReviewedPaths) {
				continue
			}
			seen := map[string]bool{}
			for _, p := range then.ReviewedPaths {
				seen[p] = true
			}
			supersets := true
			for p := range covered {
				supersets = supersets && seen[p]
			}
			if !supersets {
				continue
			}
			line := extractRiskLine([]*db.StepResult{ps}, map[string][]*db.StepRound{ps.ID: priorRounds})
			if line == "" {
				continue
			}
			return line + " (" + carriedOverNote + ")"
		}
	}
	return current
}
