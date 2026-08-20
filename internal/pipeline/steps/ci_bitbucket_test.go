package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// bitbucketPRGetKey returns the twg pull-requests get invocation key for PR 42.
func bitbucketPRGetKey(withStatuses bool) string {
	args := []string{"bitbucket", "pull-requests", "get", "42"}
	if withStatuses {
		args = append(args, "--statuses")
	}
	args = append(args, "--workspace", "test", "--repo", "repo", "--output", "json")
	return twgArgsKey(args...)
}

// newFakeBitbucketCI wires a fake twg binary that answers the PR-state and PR-statuses
// calls the CI step polls every round.
func newFakeBitbucketCI(t *testing.T, prState, statusesJSON string) (env []string, logFile string) {
	t.Helper()
	doctorKey, doctorResp := twgDoctorOK()
	return fakeTwg(t, map[string]fakeTwgResponse{
		doctorKey: doctorResp,
		bitbucketPRGetKey(false): {
			Stdout: twgEnvelope(`{"id":42,"state":"` + prState + `"}`),
		},
		bitbucketPRGetKey(true): {
			Stdout: twgEnvelope(`{"id":42,"_statuses":` + statusesJSON + `}`),
		},
	})
}

func TestCIStep_BitbucketPassesWhenStatusesPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := newFakeBitbucketCI(t, "OPEN", `[{"name":"build","state":"SUCCESSFUL"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Bitbucket CI pass to keep monitoring while PR is open, got %v", err)
	}
	if countLogLines(t, logFile, "pull-requests get 42 --workspace") == 0 {
		t.Fatal("expected Bitbucket PR state endpoint to be called")
	}
	if countLogLines(t, logFile, "pull-requests get 42 --statuses") == 0 {
		t.Fatal("expected Bitbucket statuses endpoint to be called")
	}
	foundPassed := false
	for _, line := range logs {
		if strings.Contains(line, "ready to merge") {
			t.Fatalf("expected Bitbucket CI logs not to imply mergeability, got %v", logs)
		}
		if strings.Contains(line, "all CI checks passed - still monitoring until merged or closed") {
			foundPassed = true
		}
	}
	if !foundPassed {
		t.Fatalf("expected successful Bitbucket CI logs, got %v", logs)
	}
}

func TestCIStep_BitbucketUsesProcessEnvWhenStepEnvIsNil(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := newFakeBitbucketCI(t, "OPEN", `[{"name":"build","state":"SUCCESSFUL"}]`)
	setProcessEnvFromFakeCLIEnv(t, env)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Bitbucket CI pass to keep monitoring while PR is open, got %v", err)
	}
	if countLogLines(t, logFile, "pull-requests get 42 --workspace") == 0 || countLogLines(t, logFile, "pull-requests get 42 --statuses") == 0 {
		t.Fatal("expected Bitbucket CI endpoints to be called")
	}
}

func TestCIStep_BitbucketFailureNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := newFakeBitbucketCI(t, "OPEN", `[{"name":"build","state":"FAILED"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected Bitbucket CI failures to require approval when auto-fix is disabled")
	}
	if countLogLines(t, logFile, "pull-requests get 42 --workspace") == 0 || countLogLines(t, logFile, "pull-requests get 42 --statuses") == 0 {
		t.Fatal("expected Bitbucket CI endpoints to be called")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("expected failing Bitbucket check finding, got %+v", findings.Items)
	}
}

// A stopped Bitbucket pipeline is terminal in the same way a cancelled GitHub
// check is: Bitbucket will not move it to SUCCESSFUL or FAILED on its own. It
// is not a job failure either, so it parks for a decision rather than entering
// the fix loop - and never by continuing to poll a result that has stopped
// changing.
func TestCIStep_BitbucketStoppedCheckParksForADecision(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := newFakeBitbucketCI(t, "OPEN", `[{"name":"build","state":"STOPPED"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a stopped Bitbucket pipeline to park for a decision")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("findings = %+v, want the stopped check named", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round for a stopped pipeline, got %d", len(ag.calls))
	}
}

func TestCIStep_BitbucketAutoFixIncludesPipelineLogs(t *testing.T) {
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

	doctorKey, doctorResp := twgDoctorOK()
	env, logFile := fakeTwg(t, map[string]fakeTwgResponse{
		doctorKey: doctorResp,
		bitbucketPRGetKey(false): {
			Stdout: twgEnvelope(`{"id":42,"state":"OPEN"}`),
		},
		bitbucketPRGetKey(true): {
			Stdout: twgEnvelope(`{"id":42,"_statuses":[{"name":"test","state":"FAILED","url":"https://bitbucket.org/test/repo/pipelines/results/{pipeline-1}"}]}`),
		},
		twgArgsKey("bitbucket", "pipeline", "get", "--pipeline", "pipeline-1", "--logs", "--failed-steps", "--workspace", "test", "--repo", "repo", "--output", "json"): {
			Stdout: twgEnvelope(`{"steps":[{"state":{"result":{"name":"FAILED"}},"log":"error log output"}]}`),
		},
	})

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after auto-fix poll, got %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "error log output") {
		t.Fatalf("expected Bitbucket auto-fix prompt to include pipeline logs, got:\n%s", capturedPrompt)
	}
	if countLogLines(t, logFile, "pipeline get") == 0 {
		t.Fatal("expected the Bitbucket pipeline get endpoint to be called")
	}
}

// Two failing checks link to two different pipelines; only the log for the
// pipeline behind the actually-failing check name must be fetched and used.
func TestCIStep_BitbucketAutoFixUsesMatchingPipelineLogs(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	doctorKey, doctorResp := twgDoctorOK()
	env, _ := fakeTwg(t, map[string]fakeTwgResponse{
		doctorKey: doctorResp,
		bitbucketPRGetKey(false): {
			Stdout: twgEnvelope(`{"id":42,"state":"OPEN"}`),
		},
		bitbucketPRGetKey(true): {
			Stdout: twgEnvelope(`{"id":42,"_statuses":[` +
				`{"name":"lint","state":"SUCCESSFUL","url":"https://bitbucket.org/test/repo/pipelines/results/{pipeline-1}"},` +
				`{"name":"test","state":"FAILED","url":"https://bitbucket.org/test/repo/pipelines/results/{pipeline-2}"}` +
				`]}`),
		},
		twgArgsKey("bitbucket", "pipeline", "get", "--pipeline", "pipeline-2", "--logs", "--failed-steps", "--workspace", "test", "--repo", "repo", "--output", "json"): {
			Stdout: twgEnvelope(`{"steps":[{"state":{"result":{"name":"FAILED"}},"log":"matching pipeline log"}]}`),
		},
	})

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			if err := os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after auto-fix poll, got %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "matching pipeline log") {
		t.Fatalf("expected prompt to include matching pipeline log, got:\n%s", capturedPrompt)
	}
}

func TestResolveBitbucketRepoRef_RedactsCredentialInError(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret_DO_NOT_LEAK"
	// A non-Bitbucket host with embedded credentials: ParseRepoRef rejects the
	// host, no PR URL is set, so the function returns its fallback error. That
	// error surfaces the upstream URL and must not leak the embedded credential.
	credURL := "https://x-access-token:" + token + "@example.com/w/r.git"
	_, err := resolveBitbucketRepoRef(credURL, nil)
	if err == nil {
		t.Fatal("expected error for unresolved non-Bitbucket upstream")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("resolveBitbucketRepoRef error leaked credential: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redacted@example.com/w/r.git") {
		t.Errorf("error missing redacted URL, got: %q", err.Error())
	}
}

func TestLatestBitbucketStatusesKeepsNewestStatusPerCheck(t *testing.T) {
	t.Parallel()

	statuses := []bitbucket.CommitStatus{
		{Name: "build", State: "SUCCESSFUL", Key: "build"},
		{Name: "tests", State: "FAILED", Key: "tests"},
		{Name: "build", State: "FAILED", Key: "build"},
		{Name: "tests", State: "SUCCESSFUL", Key: "tests"},
		{Name: "lint", State: "INPROGRESS"},
	}

	got := bitbucket.LatestStatuses(statuses)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0].Name != "build" || got[0].State != "SUCCESSFUL" {
		t.Fatalf("got[0] = %#v, want latest successful build", got[0])
	}
	if got[1].Name != "tests" || got[1].State != "FAILED" {
		t.Fatalf("got[1] = %#v, want latest failed tests", got[1])
	}
	if got[2].Name != "lint" || got[2].State != "INPROGRESS" {
		t.Fatalf("got[2] = %#v, want pending lint", got[2])
	}
}

func TestLatestBitbucketStatusesDeduplicatesByKeyBeforeName(t *testing.T) {
	t.Parallel()

	statuses := []bitbucket.CommitStatus{
		{Name: "build v2", Key: "build", State: "SUCCESSFUL"},
		{Name: "build", Key: "build", State: "FAILED"},
		{Name: "tests", State: "SUCCESSFUL"},
		{Name: "tests", State: "FAILED"},
	}

	got := bitbucket.LatestStatuses(statuses)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Key != "build" || got[0].Name != "build v2" || got[0].State != "SUCCESSFUL" {
		t.Fatalf("got[0] = %#v, want newest keyed build status", got[0])
	}
	if got[1].Key != "" || got[1].Name != "tests" || got[1].State != "SUCCESSFUL" {
		t.Fatalf("got[1] = %#v, want newest unnamed tests status", got[1])
	}
}

func TestCIStep_GetCIChecksBitbucketFallsBackToKeyWhenNameMissing(t *testing.T) {
	t.Parallel()

	doctorKey, doctorResp := twgDoctorOK()
	env, _ := fakeTwg(t, map[string]fakeTwgResponse{
		doctorKey: doctorResp,
		bitbucketPRGetKey(true): {
			Stdout: twgEnvelope(`{"id":42,"_statuses":[{"key":"build","state":"FAILED"}]}`),
		},
	})

	binDir := strings.SplitN(env[0][len("PATH="):], string(os.PathListSeparator), 2)[0]
	cmdFactory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, filepath.Join(binDir, name), args...)
		cmd.Env = env
		return cmd
	}
	client := bitbucket.NewClient(cmdFactory)
	host := bitbucket.NewHost(client, bitbucket.RepoRef{Workspace: "test", RepoSlug: "repo"}, nil)
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks returned error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "build" {
		t.Fatalf("checks[0].Name = %q, want build", checks[0].Name)
	}
	if checks[0].Bucket != "fail" {
		t.Fatalf("checks[0].Bucket = %q, want fail", checks[0].Bucket)
	}
}
