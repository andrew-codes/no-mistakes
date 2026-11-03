package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A description refresh: the PR was already ready and the rerun pushed nothing the
// PR did not have, so the run must neither flip it to draft nor demote it when
// the run's own outcome is worse than a ready PR deserves.
func TestRerunAddingNothingKeepsAReadyPRReady(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]types.RunStatus{
		"passed run": types.RunCompleted,
		"failed run": types.RunFailed,
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
			f := newRefreshFixture(t, api, "")
			step := &PRStep{}

			if _, err := step.Execute(f.c); err != nil {
				t.Fatal(err)
			}
			if st := api.readState(t); st.Draft || st.DraftToggles != 0 {
				t.Fatalf("draft=%v toggles=%d after the PR step, want a ready PR left untouched", st.Draft, st.DraftToggles)
			}
			msg := "ci failed"
			f.c.Run.Error = &msg
			finishRun(t, f.c, status)
			if err := step.FinalizeRun(f.c); err != nil {
				t.Fatal(err)
			}
			if st := api.readState(t); st.Draft || st.DraftToggles != 0 {
				t.Fatalf("draft=%v toggles=%d after the run ended, want the PR still ready", st.Draft, st.DraftToggles)
			}
		})
	}
}

// A rerun that leaves a draft alone does not publish it either: publishing
// belongs to the run that opened the PR.
func TestRerunAddingNothingDoesNotPublishADraft(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)
	api.withDraft(true)
	f := newRefreshFixture(t, api, "")
	step := &PRStep{}

	if _, err := step.Execute(f.c); err != nil {
		t.Fatal(err)
	}
	finishRun(t, f.c, types.RunCompleted)
	if err := step.FinalizeRun(f.c); err != nil {
		t.Fatal(err)
	}
	if st := api.readState(t); !st.Draft || st.DraftToggles != 0 {
		t.Fatalf("draft=%v toggles=%d, want the owner's draft left exactly as found", st.Draft, st.DraftToggles)
	}
}

// New content restarts validation exactly as before: a run whose head the PR
// does not already have still holds the PR as a draft until its end decides.
func TestRerunWithANewHeadStillHoldsTheReadyPRAsDraft(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	f := newRefreshFixture(t, api, "")
	// The earlier run pushed a different head to this PR.
	other := "1111111111111111111111111111111111111111"
	if err := f.c.DB.UpdateRunPushBinding(f.earlier.ID, db.PushBinding{HeadSHA: other, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(f.c); err != nil {
		t.Fatal(err)
	}
	if st := api.readState(t); !st.Draft || st.DraftToggles != 1 {
		t.Fatalf("draft=%v toggles=%d, want one toggle to draft for a head the PR did not have", st.Draft, st.DraftToggles)
	}
}

// The earlier push has to be to THIS pull request.
func TestRerunAfterAPushToAnotherPRStillHoldsTheReadyPRAsDraft(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	f := newRefreshFixture(t, api, "")
	if err := f.c.DB.UpdateRunPRURL(f.earlier.ID, "https://bitbucket.org/test/repo/pull-requests/7"); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(f.c); err != nil {
		t.Fatal(err)
	}
	if st := api.readState(t); !st.Draft {
		t.Fatal("an earlier push to a different pull request is not evidence this one already has the head")
	}
}

// withDraft seeds the draft state the fake reports for the existing PR.
func (api *fakeBitbucketAPI) withDraft(draft bool) *fakeBitbucketAPI {
	api.updateState(func(s *bbFakeStateForTest) { s.Draft = draft })
	return api
}
