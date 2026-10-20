package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// draftContext is a Bitbucket run that keeps PRs draft until they are ready.
func draftContext(t *testing.T, api *fakeBitbucketAPI) *pipeline.StepContext {
	t.Helper()
	c := bbBaseContext(t, api, "")
	c.Config.Providers.Bitbucket.DraftPullRequests = true
	return c
}

func finishRun(t *testing.T, c *pipeline.StepContext, status types.RunStatus) {
	t.Helper()
	steps, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range steps {
		if status == types.RunCompleted && sr.Status != types.StepStatusCompleted {
			if err := c.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
	}
	if status == types.RunCompleted {
		if err := c.DB.UpdateRunStatus(c.Run.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	c.Run.Status = status
}

func TestPRStep_BitbucketOpensANewPRAsADraft(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	c := draftContext(t, api)
	c.Run.PRURL = nil

	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	if !api.readState(t).Draft {
		t.Fatal("a created PR must be a draft")
	}
}

// A new run on a branch whose PR an earlier run published as ready restarts
// validation, so the PR must stop claiming ready.
func TestPRStep_BitbucketPutsAReadyPRBackToDraftWhenValidationRestarts(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := draftContext(t, api)

	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	st := api.readState(t)
	if !st.Draft || st.DraftToggles != 1 {
		t.Fatalf("draft=%v toggles=%d, want one toggle to draft", st.Draft, st.DraftToggles)
	}
	// A second pass finds it already draft and does not write again.
	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	if got := api.readState(t).DraftToggles; got != 1 {
		t.Fatalf("toggles = %d, want no write for a PR that is already a draft", got)
	}
}

func TestPRStep_BitbucketDraftToggleFailureDoesNotFailTheStep(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	api.env["FAKE_CLI_BB_REJECT_DRAFT_TOGGLE"] = "draft toggle is down"
	c := draftContext(t, api)
	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatalf("a draft toggle failure must not fail the step (the run's end converges it): %v", err)
	}
}

func TestPRStep_BitbucketLeavesDraftStateAloneWhenOptedOut(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "") // draft_pull_requests: false
	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	if api.readState(t).DraftToggles != 0 {
		t.Fatal("draft_pull_requests: false must never toggle draft state")
	}
}

func publishedDraftFixture(t *testing.T, api *fakeBitbucketAPI) (*pipeline.StepContext, *PRStep) {
	t.Helper()
	c := draftContext(t, api)
	seedRefreshRun(t, c, types.StepStatusCompleted, &cleanRound)
	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	if !api.readState(t).Draft {
		t.Fatal("setup: the PR step must leave the PR a draft")
	}
	return c, step
}

func TestFinalizeRun_BitbucketPublishesAPassedRunAsReady(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
	c, step := publishedDraftFixture(t, api)
	finishRun(t, c, types.RunCompleted)

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if st := api.readState(t); st.Draft {
		t.Fatalf("a passed run with green checks and nothing unreplied must be ready, draft=%v", st.Draft)
	}
	if !strings.Contains(api.lastDescription(t), "**Run outcome:** ✅ pipeline passed") {
		t.Fatal("the PR must be published ready only together with its regenerated description")
	}
}

func TestFinalizeRun_BitbucketKeepsAFailedRunADraft(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
	c, step := publishedDraftFixture(t, api)
	msg := "ci failed"
	c.Run.Error = &msg
	finishRun(t, c, types.RunFailed)

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if st := api.readState(t); !st.Draft || st.DraftToggles != 1 {
		t.Fatalf("a failed run must stay draft with no extra toggle, draft=%v toggles=%d", st.Draft, st.DraftToggles)
	}
}

func TestFinalizeRun_BitbucketKeepsADraftWhileAnyCheckIsNotGreen(t *testing.T) {
	t.Parallel()
	for name, statuses := range map[string]string{
		"failed":  `[{"name":"build","key":"build","state":"FAILED"}]`,
		"pending": `[{"name":"build","state":"INPROGRESS"}]`,
	} {
		statuses := statuses
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(statuses)
			c, step := publishedDraftFixture(t, api)
			finishRun(t, c, types.RunCompleted)
			if err := step.FinalizeRun(c); err != nil {
				t.Fatal(err)
			}
			if !api.readState(t).Draft {
				t.Fatal("a PR with a non-green check must not be published as ready")
			}
		})
	}
}

func TestFinalizeRun_BitbucketKeepsADraftWhileAReviewCommentHasNoReply(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).
		withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`).
		withComments(bbReviewCommentsJSON)
	c, step := publishedDraftFixture(t, api)
	finishRun(t, c, types.RunCompleted)

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if !api.readState(t).Draft {
		t.Fatal("unreplied review feedback must keep the PR a draft")
	}
}

func TestFinalizeRun_BitbucketReturnsAReadyPRToDraftWhenFeedbackArrives(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
	c, step := publishedDraftFixture(t, api)
	finishRun(t, c, types.RunCompleted)
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if api.readState(t).Draft {
		t.Fatal("setup: the PR should be ready")
	}

	// New unreplied feedback and another finalization: back to draft.
	api.withComments(bbReviewCommentsJSON)
	c.Env = api.Env()
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if !api.readState(t).Draft {
		t.Fatal("a PR that stopped being ready must go back to draft")
	}
}

func TestFinalizeRun_BitbucketNeverPublishesAnOverriddenRunAsReady(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
	c, step := publishedDraftFixture(t, api)
	finishRun(t, c, types.RunCompleted)
	steps, _ := c.DB.GetStepsByRun(c.Run.ID)
	for _, sr := range steps {
		if sr.StepName == types.StepTest {
			if err := c.DB.SetStepOverrideReason(sr.ID, "approved over a failing test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if !api.readState(t).Draft {
		t.Fatal("an approval override is not a clean pass, so the PR must stay draft")
	}
}
