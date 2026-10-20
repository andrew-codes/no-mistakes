package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	staleTestFailureFinding = `{"findings":[{"id":"t1","severity":"error","description":"configured test command failed with exit code 1","action":"auto-fix"}],"summary":"1 error"}`
	cleanFindings           = `{"findings":[],"summary":""}`
)

// seedRefreshRun records a Test step that fails in round 1 and is fixed in
// round 2, plus a published PR step, the shape every stale description in
// rovo-chat-desktop had.
var cleanRound = cleanFindings

func seedRefreshRun(t *testing.T, sctx *pipeline.StepContext, testStatus types.StepStatus, round2Findings *string) {
	t.Helper()
	prStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepPR)
	if err != nil {
		t.Fatal(err)
	}
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	f1 := staleTestFailureFinding
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 1, "initial", &f1, nil, 800); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 2, "auto_fix", round2Findings, nil, 600); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, testStatus); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(prStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
}

// The reported staleness: the body is written by the PR step while the Test
// step still shows its failing round, and nothing rewrites it once the run
// ends green.
func TestFinalizeRun_BitbucketDescriptionReflectsAPassingFinalState(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	seedRefreshRun(t, c, types.StepStatusAwaitingApproval, &cleanRound)

	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	before := api.lastDescription(t)
	if !strings.Contains(before, "configured test command failed with exit code 1") {
		t.Fatalf("setup: the PR step's body should carry the failing snapshot, got:\n%s", before)
	}

	// The run goes on: the Test step settles green and the run completes.
	steps, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range steps {
		if sr.StepName == types.StepTest {
			if err := c.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.DB.UpdateRunStatus(c.Run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	c.Run.Status = types.RunCompleted
	puts := api.puts(t)

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	after := api.lastDescription(t)
	if api.puts(t) != puts+1 {
		t.Fatalf("expected exactly one description rewrite at run end, got %d", api.puts(t)-puts)
	}
	if strings.Contains(after, "configured test command failed with exit code 1") {
		t.Fatalf("the failing round must not outlive a passing final state:\n%s", after)
	}
	for _, want := range []string{"**Run outcome:** ✅ pipeline passed", "Round 1 raised 1 error; superseded by a later round."} {
		if !strings.Contains(after, want) {
			t.Fatalf("final description is missing %q:\n%s", want, after)
		}
	}
}

func TestFinalizeRun_FailedRunStatesWhereItStopped(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	still := staleTestFailureFinding
	seedRefreshRun(t, c, types.StepStatusFailed, &still)
	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.UpdateRunErrorStatus(c.Run.ID, "test failed after 3 attempts", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	msg := "test failed after 3 attempts"
	c.Run.Status = types.RunFailed
	c.Run.Error = &msg

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	after := api.lastDescription(t)
	if !strings.Contains(after, "**Run outcome:** ❌ pipeline failed at Test") || !strings.Contains(after, "test failed after 3 attempts") {
		t.Fatalf("a failed run must say where it stopped, got:\n%s", after)
	}
	// Still open in the latest round, so it stays visible.
	if !strings.Contains(after, "configured test command failed with exit code 1") {
		t.Fatalf("a finding still open at the end must stay in the description:\n%s", after)
	}
}

func TestFinalizeRun_LeavesAPRThisRunDidNotPublishAlone(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	api.setDescription(t, "Hand written by an earlier run")
	if _, err := c.DB.InsertStepResult(c.Run.ID, types.StepPR); err != nil { // pending: never published
		t.Fatal(err)
	}
	c.Run.Status = types.RunFailed
	if err := (&PRStep{}).FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if api.puts(t) != 0 || api.lastDescription(t) != "Hand written by an earlier run" {
		t.Fatal("a run that never wrote the body must not rewrite it")
	}
}

func TestFinalizeRun_IsANoOpWhenTheBodyIsAlreadyCurrent(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	seedRefreshRun(t, c, types.StepStatusCompleted, &cleanRound)
	c.Run.Status = types.RunCompleted
	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	puts := api.puts(t)
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if api.puts(t) != puts {
		t.Fatal("refreshing an up to date description must not write again")
	}
}
