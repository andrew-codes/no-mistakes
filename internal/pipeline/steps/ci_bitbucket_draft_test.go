package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Bitbucket does not evaluate a draft PR's git mergeability check: twg lists it
// PENDING or leaves it out altogether, and it stays that way for as long as the
// PR is a draft. The draft lifecycle opens every PR as a draft and publishes it
// ready only at the end of the run, so a monitor that waits for a resolved
// mergeability reading before it will look at anything else waits for a
// reading that only the run's own ending can produce.
const (
	bbDraftPendingGitCheck = `[{"type":"git_mergeability_check","status":"PENDING","required":true,"blocking":true},{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"PASSED","blocking":false}]`
	bbDraftNoGitCheck      = `[{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"PASSED","blocking":false}]`
)

// bbCIDraftContext is a CI step context for a Bitbucket PR that is a draft (or
// a ready PR) under the draft lifecycle, with every check green.
func bbCIDraftContext(t *testing.T, api *fakeBitbucketAPI, draft bool) (*pipeline.StepContext, *[]string) {
	t.Helper()
	api.updateState(func(s *bbFakeStateForTest) { s.Draft = draft })
	dir, baseSHA, headSHA := setupGitRepo(t)
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.Providers.Bitbucket.DraftPullRequests = true
	logs := &[]string{}
	sctx.Log = func(s string) { *logs = append(*logs, s) }
	return sctx, logs
}

func bbDraftAPI(t *testing.T, mergeability string) *fakeBitbucketAPI {
	t.Helper()
	return newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`).
		withMergeability(mergeability)
}

// onePollThenCancel lets the monitor poll once and then end the step with the
// context error, so a monitor that would otherwise wait forever fails the test
// instead of hanging it.
func onePollThenCancel(sctx *pipeline.StepContext) *CIStep {
	ctx, cancel := context.WithCancel(sctx.Ctx)
	sctx.Ctx = ctx
	return &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}
}

// A green draft ends the monitor at the checks verdict: a draft cannot merge,
// and the run's end is what publishes it, so watching it any longer would wait
// on the step this wait holds back.
func TestCIStep_BitbucketGreenDraftPRReleasesTheRunDespiteUnreadableMergeability(t *testing.T) {
	t.Parallel()
	for name, mergeability := range map[string]string{
		"git check pending":     bbDraftPendingGitCheck,
		"git check not listed":  bbDraftNoGitCheck,
		"empty merge check set": `[]`,
		"git check passed":      `[{"type":"git_mergeability_check","status":"PASSED","blocking":false}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sctx, logs := bbCIDraftContext(t, bbDraftAPI(t, mergeability), true)
			step := onePollThenCancel(sctx)

			outcome, err := driveCI(t, step, sctx)
			joined := strings.Join(*logs, "\n")
			if err != nil {
				t.Fatalf("expected the green draft to release the run, got %v\nlogs:\n%s", err, joined)
			}
			if outcome == nil || outcome.NeedsApproval || outcome.AutoFixable || outcome.Findings != "" || outcome.CIReadyNoCI == nil {
				t.Fatalf("outcome = %#v, want a plain checks-passed success", outcome)
			}
			if strings.Contains(joined, "mergeable state still pending") {
				t.Fatalf("a draft's unreadable mergeability must not hold the monitor, logs:\n%s", joined)
			}
		})
	}
}

// Releasing publishes nothing by itself: a draft with feedback nobody has
// answered is not a verdict, it is a repair.
func TestCIStep_BitbucketGreenDraftPRWithUnrepliedCommentsIsNotReleased(t *testing.T) {
	t.Parallel()
	api := bbDraftAPI(t, bbDraftPendingGitCheck).withComments(bbReviewCommentsJSON)
	sctx, _ := bbCIDraftContext(t, api, true)
	step := onePollThenCancel(sctx)

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.CIReadyNoCI != nil || outcome.Findings == "" {
		t.Fatalf("outcome = %#v, want the review comments as findings and no release", outcome)
	}
}

// The draft lifecycle only applies to a PR that is a draft. Once it is ready,
// the monitor watches it until it is merged or closed, as before.
func TestCIStep_BitbucketReadyPRUnderTheDraftLifecycleKeepsMonitoring(t *testing.T) {
	t.Parallel()
	sctx, logs := bbCIDraftContext(t, bbDraftAPI(t, `[{"type":"git_mergeability_check","status":"PASSED","blocking":false}]`), false)
	step := onePollThenCancel(sctx)

	if _, err := driveCI(t, step, sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the monitor to keep watching a ready PR, got %v", err)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "all CI checks passed - still monitoring until merged or closed") {
		t.Fatalf("expected the checks-passed verdict while still monitoring, logs:\n%s", joined)
	}
}

// Only a draft is excused. A ready PR is evaluated by Bitbucket, so a reading
// that is still pending there is a genuine "not yet", and the monitor keeps
// waiting for it exactly as before.
func TestCIStep_BitbucketReadyPRWithPendingMergeabilityStaysPending(t *testing.T) {
	t.Parallel()
	sctx, logs := bbCIDraftContext(t, bbDraftAPI(t, bbDraftPendingGitCheck), false)
	step := onePollThenCancel(sctx)

	if _, err := driveCI(t, step, sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the monitor to keep waiting, got %v", err)
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "mergeable state still pending") {
		t.Fatalf("a ready PR's pending mergeability must still hold the monitor, logs:\n%s", joined)
	}
	if strings.Contains(joined, "all CI checks passed") {
		t.Fatalf("a ready PR with unresolved mergeability must not report checks-passed, logs:\n%s", joined)
	}
}

// A draft is not exempt from what Bitbucket does tell us: a failed merge check
// is still a conflict to repair, and a red check is still a failure.
func TestCIStep_BitbucketDraftPRStillRepairsWhatItCanProve(t *testing.T) {
	t.Parallel()
	t.Run("blocking commits-behind check", func(t *testing.T) {
		t.Parallel()
		api := bbDraftAPI(t, `[{"type":"git_mergeability_check","status":"PENDING","blocking":true},{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"FAILED","blocking":true}]`)
		sctx, _ := bbCIDraftContext(t, api, true)
		sctx.Config.AutoFix = config.AutoFix{CI: 0}
		step := onePollThenCancel(sctx)

		outcome, err := driveCI(t, step, sctx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil || !strings.Contains(outcome.Findings, "merge conflict") {
			t.Fatalf("expected a merge-conflict finding for a draft behind its base, got %#v", outcome)
		}
	})
	t.Run("failed check", func(t *testing.T) {
		t.Parallel()
		api := bbDraftAPI(t, bbDraftPendingGitCheck).withStatuses(`[{"name":"build","state":"FAILED"}]`)
		sctx, _ := bbCIDraftContext(t, api, true)
		step := onePollThenCancel(sctx)

		outcome, err := driveCI(t, step, sctx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil || !strings.Contains(outcome.Findings, "build") {
			t.Fatalf("expected the failed build to be reported for a draft, got %#v", outcome)
		}
	})
}
