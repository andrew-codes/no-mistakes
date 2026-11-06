package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// A Bitbucket PR that is already green keeps being watched until it merges or
// closes, and what goes wrong after green is still repaired: the destination
// advancing past the repository's maximum_commits_behind merge check, and a
// reviewer commenting. Both used to be read only on the poll that first saw the
// checks settle, so a PR that turned unmergeable or collected feedback later
// logged "still monitoring" forever and nobody corrected it.

const (
	bbMergeChecksPassing = `[{"type":"git_mergeability_check","status":"PASSED","blocking":false},{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"PASSED","blocking":false}]`
	bbMergeChecksTooFar  = `[{"type":"git_mergeability_check","status":"PASSED","blocking":false},{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"FAILED","blocking":true}]`
)

func TestCIStep_BitbucketRepairsAPRThatFallsTooFarBehindAfterGreen(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	mergeChecks := filepath.Join(t.TempDir(), "merge-checks.json")
	if err := os.WriteFile(mergeChecks, []byte(bbMergeChecksPassing), 0o644); err != nil {
		t.Fatal(err)
	}
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withSourceCommit(headSHA).
		withStatuses(bbGreenStatuses)
	api.env["FAKE_CLI_BB_MERGEABILITY_FILE"] = mergeChecks

	var repairPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			repairPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "rebased.txt"), []byte("rebased"), 0o644)
			return &agent.Result{}, nil
		},
	}
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI.RevalidateRepairs = true
	logs := &[]string{}
	sctx.Log = func(s string) { *logs = append(*logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		waits++
		switch {
		case waits == 1:
			// The PR was green; now the destination moves on and the repository's
			// maximum_commits_behind rule trips.
			if err := os.WriteFile(mergeChecks, []byte(bbMergeChecksTooFar), 0o644); err != nil {
				t.Fatal(err)
			}
		case waits >= 4:
			cancel()
			return ctx.Err()
		}
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	if waits < 1 || !strings.Contains(strings.Join(*logs, "\n"), "all CI checks passed - still monitoring") {
		t.Fatalf("expected a green observation before the merge checks changed (waits=%d)\nlogs:\n%s", waits, strings.Join(*logs, "\n"))
	}
	assertCIRestartsValidation(t, outcome, err)
	if !strings.Contains(repairPrompt, "Rebase onto the base branch and resolve the merge conflicts") {
		t.Fatalf("a PR that fell too far behind after green was not repaired; repair prompt = %q\nlogs:\n%s", repairPrompt, strings.Join(*logs, "\n"))
	}
}

func TestCIStep_BitbucketRaisesACommentThatArrivesAfterGreen(t *testing.T) {
	t.Parallel()
	api := bbDraftAPI(t, bbMergeChecksPassing).withComments(`[]`)
	sctx, logs := bbCIDraftContext(t, api, false)
	ctx, cancel := context.WithCancel(sctx.Ctx)
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		waits++
		switch {
		case waits == 1:
			// Green and quiet so far; a reviewer now leaves feedback.
			if err := os.WriteFile(api.commentsPath, []byte(bbOneRequestAndANotice), 0o644); err != nil {
				t.Fatal(err)
			}
		case waits >= 4:
			cancel()
			return ctx.Err()
		}
		return nil
	}}

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("CI step error = %v\nlogs:\n%s", err, strings.Join(*logs, "\n"))
	}
	if waits < 1 || outcome == nil || outcome.CIReadyNoCI != nil || outcome.Findings == "" {
		t.Fatalf("outcome = %#v (waits=%d), want the comment raised after green\nlogs:\n%s", outcome, waits, strings.Join(*logs, "\n"))
	}
	if ids := commentFindingIDs(t, outcome.Findings); len(ids) != 1 || ids[0] != "2002" {
		t.Fatalf("raised comments = %v, want only the request (2002)", ids)
	}
}
