package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
)

const bbBasePRURL = "https://bitbucket.org/test/repo/pull-requests/42"

// bbBaseContext is a PR step whose run already owns Bitbucket PR 42, with the
// per-run --base-branch override set to requested (empty for none).
func bbBaseContext(t *testing.T, api *fakeBitbucketAPI, requested string) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if requested != "" {
		ensureLocalBranch(t, dir, requested, baseSHA)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	owned := bbBasePRURL
	sctx.Run.PRURL = &owned
	if requested != "" {
		sctx.Run.PRBaseBranch = &requested
	}
	return sctx
}

// The scenario this exists for: a Bitbucket PR is already open on another
// base and the run asks for main. The step used to fail with "existing pull
// request has no readable base branch, and this provider cannot retarget it".
func TestPRStep_BitbucketRetargetsExistingPRToTheRequestedBase(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withDestination("develop")
	c := bbBaseContext(t, api, "main")

	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatalf("PR step failed on an existing Bitbucket PR with a requested base: %v", err)
	}
	if got := api.destination(t); got != "main" {
		t.Fatalf("PR destination = %q, want main", got)
	}
	if api.logCount("pull-requests update --pull-request 42 --dest main") != 1 {
		t.Fatalf("expected exactly one retarget call")
	}
	if api.logCount("pull-requests create") != 0 {
		t.Fatal("a retarget must never open a duplicate PR")
	}
	if api.puts(t) != 1 {
		t.Fatalf("title and body updates = %d, want the usual single update after the retarget", api.puts(t))
	}
}

func TestPRStep_BitbucketLeavesAnAlreadyMatchingBaseAlone(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withDestination("main")
	c := bbBaseContext(t, api, "main")

	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	if api.destUpdates(t) != 0 {
		t.Fatal("a PR already on the requested base must not be retargeted")
	}
}

func TestPRStep_BitbucketFailsClosedWhenRequestedBaseCannotBeVerified(t *testing.T) {
	t.Parallel()
	// No destination in the response: the base is unreadable AND one was
	// requested, so the step must not rewrite the PR against a base it could
	// not check.
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "main")

	_, err := (&PRStep{}).Execute(c)
	if err == nil || !strings.Contains(err.Error(), "no destination branch") {
		t.Fatalf("error = %v, want the unreadable destination to fail the step", err)
	}
	if api.puts(t) != 0 || api.destUpdates(t) != 0 {
		t.Fatal("nothing may be changed on a PR whose base could not be read")
	}
}

func TestPRStep_BitbucketSurfacesARejectedRetarget(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withDestination("develop")
	api.env["FAKE_CLI_BB_REJECT_DEST_ON_UPDATE"] = "destination branch does not exist"
	c := bbBaseContext(t, api, "main")
	c.Env = api.Env()

	_, err := (&PRStep{}).Execute(c)
	if err == nil || !strings.Contains(err.Error(), "retarget pull request to main") || !strings.Contains(err.Error(), "destination branch does not exist") {
		t.Fatalf("error = %v, want the retarget failure", err)
	}
	if api.puts(t) != 0 {
		t.Fatal("the title and body must not be rewritten after a failed retarget")
	}
}

func TestPRStep_BitbucketRefusesARetargetTheServerIgnored(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL).withDestination("develop")
	api.env["FAKE_CLI_BB_IGNORE_DEST_ON_UPDATE"] = "1"
	c := bbBaseContext(t, api, "main")
	c.Env = api.Env()

	_, err := (&PRStep{}).Execute(c)
	if err == nil || !strings.Contains(err.Error(), "still targets develop") {
		t.Fatalf("error = %v, want the ignored retarget to fail rather than read as success", err)
	}
}

// Nothing requested and the base unreadable only because the provider could
// not say: the PR step has no base to compare, so it must not fail.
func TestPRStep_BitbucketUnreadableBaseWithNothingRequestedStillUpdates(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	var logs []string
	c.Log = func(s string) { logs = append(logs, s) }

	if _, err := (&PRStep{}).Execute(c); err != nil {
		t.Fatalf("PR step failed although no base was requested: %v", err)
	}
	if api.puts(t) != 1 {
		t.Fatalf("PR updates = %d, want the existing PR updated in place", api.puts(t))
	}
	if api.destUpdates(t) != 0 {
		t.Fatal("no retarget may happen when no base was requested")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "could not read the base branch") {
		t.Fatalf("the skipped base read should be logged, got %v", logs)
	}
}

// failingBaseReaderHost is an open PR whose base the provider cannot read.
type failingBaseReaderHost struct {
	scm.Host
	err error
}

func (h failingBaseReaderHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return scm.PRStateOpen, nil
}

func (h failingBaseReaderHost) GetPRBaseBranch(context.Context, *scm.PR) (string, error) {
	return "", h.err
}

func TestBindExistingPR_UnreadableBaseOnlyFailsWhenABaseWasRequested(t *testing.T) {
	t.Parallel()
	owned := bbBasePRURL
	readErr := errors.New("twg bb pull-requests get: boom")
	for _, tc := range []struct {
		name      string
		requested *string
		err       error
		wantErr   bool
	}{
		{name: "nothing requested continues with the base unknown", err: readErr},
		{name: "a requested base cannot be compared, so it fails closed", requested: strptr("main"), err: readErr, wantErr: true},
		{name: "a broken plugin contract always fails", err: fmt.Errorf("status: %w", plugin.ErrProtocol), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned, PRBaseBranch: tc.requested}}
			discovered := &scm.PR{Number: "42", URL: bbBasePRURL}
			bound, err := bindExistingPR(sctx, failingBaseReaderHost{err: tc.err}, discovered)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("bindExistingPR = %+v, want an error", bound)
				}
				return
			}
			if err != nil || bound == nil || bound.BaseBranch != "" {
				t.Fatalf("bindExistingPR = %+v, %v, want the PR with an unknown base", bound, err)
			}
		})
	}
}
