package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// findCoveringMergedPR looks up the branch's merged PRs and returns the newest
// one that already delivered this run's head to the run's base branch, or nil
// when the PR step must go on to create a PR.
//
// Host.FindPR lists open PRs only. A forge that merges a PR the moment its
// build is green can merge it before this step runs, and opening a second PR
// for the same head would duplicate work that is already on the base branch.
//
// A merged PR counts only when all of these hold:
//   - it targeted this run's base branch (a PR merged into another branch did
//     not deliver the work here);
//   - the head the forge recorded for it equals the run's head, or contains it
//     (the run's head is an ancestor of the merged head), so nothing the run
//     holds is missing from it;
//   - that head can be compared locally. A merged head this checkout does not
//     have, or a PR the forge did not report a head or target for, proves
//     nothing and falls through to a new PR, the same as a branch that gained
//     commits after the merge.
//
// Declined and closed PRs are never listed, so they never block a new PR. A
// host without MergedPRFinder keeps today's behavior. A lookup that fails or
// returns an unreadable answer is an error rather than "none": reading it as
// absence is what would create a duplicate.
func findCoveringMergedPR(sctx *pipeline.StepContext, host scm.Host, branch, baseBranch string) (*scm.PR, error) {
	finder, ok := host.(scm.MergedPRFinder)
	if !ok {
		return nil, nil
	}
	sctx.Log(fmt.Sprintf("checking for an already merged pull request on branch %s...", branch))
	merged, err := finder.FindMergedPRs(sctx.Ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("look up merged pull requests: %w", err)
	}
	runHead := strings.TrimSpace(sctx.Run.HeadSHA)
	for _, pr := range merged {
		if pr == nil {
			continue
		}
		if got := strings.TrimSpace(pr.BaseBranch); got != baseBranch {
			sctx.Log(fmt.Sprintf("merged pull request %s targeted %q, not %q; it does not cover this run", describePR(pr), got, baseBranch))
			continue
		}
		covers, reason := mergedHeadCovers(sctx, runHead, strings.TrimSpace(pr.HeadSHA))
		if !covers {
			sctx.Log(fmt.Sprintf("merged pull request %s does not cover this run: %s", describePR(pr), reason))
			continue
		}
		return pr, nil
	}
	return nil, nil
}

// mergedHeadCovers reports whether mergedHead equals or contains runHead. The
// reason explains a negative answer.
func mergedHeadCovers(sctx *pipeline.StepContext, runHead, mergedHead string) (bool, string) {
	if runHead == "" {
		return false, "the run has no head to compare"
	}
	if mergedHead == "" {
		return false, "the forge did not report the merged head"
	}
	if strings.EqualFold(mergedHead, runHead) {
		return true, ""
	}
	if _, err := stepGitRun(sctx, "cat-file", "-e", mergedHead+"^{commit}"); err != nil {
		return false, fmt.Sprintf("merged head %s is not available locally to compare", shortSHA(mergedHead))
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", runHead, mergedHead); err != nil {
		return false, fmt.Sprintf("the branch has commits that merged head %s lacks", shortSHA(mergedHead))
	}
	return true, ""
}

// adoptMergedPR records an already merged PR as the run's PR and finishes the
// PR step without creating or updating anything. The CI step then reads the
// PR as merged on its first poll and ends the run as a success.
func adoptMergedPR(sctx *pipeline.StepContext, merged *scm.PR) (*pipeline.StepOutcome, error) {
	if err := refuseSkipWithClosingIssues(sctx, fmt.Sprintf("pull request %s for this head is already merged", describePR(merged))); err != nil {
		return nil, err
	}
	sctx.Log(fmt.Sprintf("pull request already merged: %s; not creating or updating a pull request", describePR(merged)))
	if merged.URL == "" {
		return &pipeline.StepOutcome{}, nil
	}
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, merged.URL); err != nil {
		slog.Warn("failed to persist PR URL", "run", sctx.Run.ID, "url", merged.URL, "err", err)
	}
	return &pipeline.StepOutcome{PRURL: merged.URL}, nil
}
