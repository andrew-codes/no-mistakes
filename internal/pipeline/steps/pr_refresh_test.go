package steps

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	staleTestFailureFinding = `{"findings":[{"id":"t1","severity":"error","description":"configured test command failed with exit code 1","action":"auto-fix"}],"summary":"1 error"}`
	cleanFindings           = `{"findings":[],"summary":""}`
)

// seedRefreshRun records a Test step that fails in round 1 and is fixed in
// round 2, plus a published PR step, the shape of a description that went stale
// while the run kept working.
var cleanRound = cleanFindings

const refreshPRURL = "https://github.com/test/repo/pull/42"

// descriptionFixture is a PR step context for an open GitHub PR whose body the fake
// gh keeps in a file, so a test reads back exactly what the step published.
type descriptionFixture struct {
	sctx     *pipeline.StepContext
	bodyFile string
	logFile  string
}

func newDescriptionFixture(t *testing.T) *descriptionFixture {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, refreshPRURL)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	return &descriptionFixture{sctx: sctx, bodyFile: envEntry(env, "FAKE_CLI_PR_BODY_FILE"), logFile: logFile}
}

func (f *descriptionFixture) description(t *testing.T) string {
	t.Helper()
	return readPRBodyFile(t, f.bodyFile)
}

func (f *descriptionFixture) setDescription(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// edits counts the description writes the fake gh has seen.
func (f *descriptionFixture) edits(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.logFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "--method PATCH")
}

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
func TestFinalizeRun_DescriptionReflectsAPassingFinalState(t *testing.T) {
	t.Parallel()
	f := newDescriptionFixture(t)
	c := f.sctx
	seedRefreshRun(t, c, types.StepStatusAwaitingApproval, &cleanRound)

	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	before := f.description(t)
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
	puts := f.edits(t)

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	after := f.description(t)
	if f.edits(t) != puts+1 {
		t.Fatalf("expected exactly one description rewrite at run end, got %d", f.edits(t)-puts)
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
	f := newDescriptionFixture(t)
	c := f.sctx
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
	after := f.description(t)
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
	f := newDescriptionFixture(t)
	c := f.sctx
	f.setDescription(t, "Hand written by an earlier run")
	if _, err := c.DB.InsertStepResult(c.Run.ID, types.StepPR); err != nil { // pending: never published
		t.Fatal(err)
	}
	c.Run.Status = types.RunFailed
	if err := (&PRStep{}).FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if f.edits(t) != 0 || f.description(t) != "Hand written by an earlier run" {
		t.Fatal("a run that never wrote the body must not rewrite it")
	}
}

func TestFinalizeRun_IsANoOpWhenTheBodyIsAlreadyCurrent(t *testing.T) {
	t.Parallel()
	f := newDescriptionFixture(t)
	c := f.sctx
	seedRefreshRun(t, c, types.StepStatusCompleted, &cleanRound)
	c.Run.Status = types.RunCompleted
	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	puts := f.edits(t)
	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	if f.edits(t) != puts {
		t.Fatal("refreshing an up to date description must not write again")
	}
}
