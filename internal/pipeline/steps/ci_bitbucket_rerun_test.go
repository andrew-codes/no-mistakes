package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	bbStoppedBuild = `[{"name":"build","key":"build","state":"STOPPED","url":"https://bitbucket.org/test/repo/pipelines/results/7","created_on":"2026-11-10T10:00:00Z","updated_on":"2026-11-10T10:05:00Z"}]`
	bbRunningBuild = `[{"name":"build","key":"build","state":"INPROGRESS","url":"https://bitbucket.org/test/repo/pipelines/results/7","created_on":"2026-11-10T10:00:00Z","updated_on":"2026-11-10T10:06:00Z"}]`
	bbFailedBuild  = `[{"name":"build","key":"build","state":"FAILED","url":"https://bitbucket.org/test/repo/pipelines/results/7","created_on":"2026-11-10T10:00:00Z","updated_on":"2026-11-10T10:05:00Z"}]`
)

// bitbucketRerunContext is a Bitbucket run whose branch was pushed, so a rerun
// may verify the published head, with ci.rerun_transient set to budget.
func bitbucketRerunContext(t *testing.T, api *fakeBitbucketAPI, budget int, agentCalls *int) (*pipeline.StepContext, context.CancelFunc) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: budget}
	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	t.Cleanup(func() {
		if agentCalls != nil {
			*agentCalls = len(ag.calls)
		}
	})
	return sctx, cancel
}

func bitbucketRerunAPI(t *testing.T) *fakeBitbucketAPI {
	t.Helper()
	statusesFile := filepath.Join(t.TempDir(), "bb-statuses.json")
	if err := os.WriteFile(statusesFile, []byte(bbStoppedBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").withPRState("OPEN")
	api.env["FAKE_CLI_BB_STATUSES_FILE"] = statusesFile
	api.env["FAKE_CLI_BB_RERUN_STATUSES_JSON"] = bbRunningBuild
	return api
}

// A pipeline Bitbucket stopped is not a verdict on the code: with a rerun
// budget it is re-run for the same commit instead of parking, the fix agent is
// never involved, and the monitor goes on watching the replacement.
func TestCIStep_BitbucketStoppedPipelineIsRerunBeforeEscalating(t *testing.T) {
	t.Parallel()
	api := bitbucketRerunAPI(t)
	sctx, cancel := bitbucketRerunContext(t, api, 1, nil)
	defer cancel()
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	polls := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	if _, err := driveCI(t, step, sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue after the rerun, got %v", err)
	}
	if got := api.logCount("pipeline rerun-failed --pipeline 7"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one for build 7", got)
	}
	reran := false
	for _, line := range logs {
		reran = reran || strings.Contains(line, "re-running CI check build (1/1)")
	}
	if !reran {
		t.Fatalf("expected the rerun to be logged, got %v", logs)
	}
}

// Bitbucket can refuse the rerun (a token without pipeline write permission,
// for one). That is logged and the check escalates exactly as it would with no
// rerun budget at all.
func TestCIStep_BitbucketRefusedRerunEscalatesTheStoppedPipeline(t *testing.T) {
	t.Parallel()
	api := bitbucketRerunAPI(t)
	api.env["FAKE_CLI_BB_RERUN_FAIL"] = "token lacks pipeline write permission"
	sctx, cancel := bitbucketRerunContext(t, api, 1, nil)
	defer cancel()
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	polls := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 5 {
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	outcome, err := driveCI(t, step, sctx)
	if err != nil || !outcome.NeedsApproval {
		t.Fatalf("expected a refused rerun to park for a decision: outcome=%+v err=%v", outcome, err)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v (%v), want the stopped check as one ask-user finding", findings.Items, err)
	}
	warned := false
	for _, line := range logs {
		warned = warned || (strings.Contains(line, "could not re-run transient CI check build") && strings.Contains(line, "token lacks pipeline write permission"))
	}
	if !warned {
		t.Fatalf("expected the refusal to be logged, got %v", logs)
	}
}

// A pipeline that failed is a verdict on the code: it is never re-run, however
// large the rerun budget.
func TestCIStep_BitbucketFailedPipelineIsNeverRerun(t *testing.T) {
	t.Parallel()
	api := bitbucketRerunAPI(t)
	if err := os.WriteFile(api.env["FAKE_CLI_BB_STATUSES_FILE"], []byte(bbFailedBuild), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx, cancel := bitbucketRerunContext(t, api, 3, nil)
	defer cancel()
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	outcome, err := driveCI(t, &CIStep{}, sctx)
	if err != nil || !outcome.NeedsApproval {
		t.Fatalf("expected the failed pipeline to need a decision: outcome=%+v err=%v", outcome, err)
	}
	if got := api.logCount("pipeline rerun-failed"); got != 0 {
		t.Fatalf("a failed pipeline was re-run %d times, want none", got)
	}
}

// Reruns are opt-in: with no budget a stopped pipeline parks without any
// rerun request, as before.
func TestCIStep_BitbucketStoppedPipelineIsNotRerunWithoutABudget(t *testing.T) {
	t.Parallel()
	api := bitbucketRerunAPI(t)
	sctx, cancel := bitbucketRerunContext(t, api, 0, nil)
	defer cancel()
	polls := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 5 {
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	outcome, err := driveCI(t, step, sctx)
	if err != nil || !outcome.NeedsApproval {
		t.Fatalf("expected the stopped pipeline to park: outcome=%+v err=%v", outcome, err)
	}
	if got := api.logCount("pipeline rerun-failed"); got != 0 {
		t.Fatalf("a rerun was requested %d times with no budget", got)
	}
}
