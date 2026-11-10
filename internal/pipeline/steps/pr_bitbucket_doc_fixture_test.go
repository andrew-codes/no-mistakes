package steps

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// bitbucketDocIntent is the shape of a real Intent that rendered badly on
// Bitbucket Cloud: several paragraphs of the author's own brief.
const bitbucketDocIntent = `We have started to add a lot of tests to the scripts directories, and many of them use manual test mechanisms.

The ask:

Write script guidelines for agents to follow when writing future scripts.

Script tests all use the existing testing framework in the codebase; Jest. Running a project's test/unit should run the tests for its scripts as part of it.`

const bitbucketDocWhatChanged = "## What Changed\n\n- `jest.config.js` now discovers `.cjs` and `.mjs` tests as well as `.ts` ones.\n- The `scripts/build-tools` test moves from `node:test` to Jest."

// newBitbucketDocContext seeds a finished run: a long multi-paragraph Intent, a
// Test step approved over a failing configured command with a verdict, a
// scenario table and tested commands (one a script, one prose around a code
// span), and clean steps around it.
func newBitbucketDocContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.UserIntent = bitbucketDocIntent
	sctx.Run.HeadSHA = testPipelineHeadSHA
	// The shipped default: the Testing section links evidence and leaves the
	// tested commands to the Pipeline's Test group.
	sctx.Config.Test.Evidence.Compact = true

	testFindings, err := json.Marshal(types.Findings{
		Items:          []types.Finding{{ID: "t1", Severity: "error", Description: exceptionCondition, Action: types.ActionAskUser}},
		Summary:        "1 error",
		TestingSummary: "I ran the 7 ported suites directly under Jest and all 56 tests passed, but this was not a live product drive.",
		Verdict:        types.TestVerdictInconclusive,
		Scenarios: []types.TestScenario{{
			Name:     "Ported tests are discovered and pass under standard Jest",
			Result:   "untested",
			Evidence: "Unit-level run of the tooling tests, not a drive of the running product.",
		}},
		Tested: []string{
			"`yarn exec jest scripts/build-tools` (7 suites, 56 tests, all passing)",
			"bash <<'SCRIPT'\nset -e\nyarn install --immutable\nyarn exec jest scripts\nSCRIPT",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	seed := func(name types.StepName, findings string) string {
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 4000); err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.SetStepFindings(sr.ID, findings); err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.CompleteStep(sr.ID, 0, 4000, ""); err != nil {
			t.Fatal(err)
		}
		return sr.ID
	}
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview} {
		seed(name, cleanTestFindings)
	}
	testID := seed(types.StepTest, string(testFindings))
	// The approval reason can only be recorded while the gate is parked.
	if err := sctx.DB.UpdateStepStatus(testID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetTestApprovalReason(testID, exceptionReason); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepOverrideReason(testID, exceptionCondition); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepDocument, types.StepLint, types.StepPush} {
		seed(name, cleanTestFindings)
	}
	ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(ci.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	sctx.Run.Status = types.RunCompleted
	return sctx
}

// bitbucketDocBody assembles the description exactly as the PR step does for
// a finished run on Bitbucket, with no body budget.
func bitbucketDocBody(t *testing.T, sctx *pipeline.StepContext) string {
	t.Helper()
	pipelineMD, riskLine, testingMD, _ := (&PRStep{}).buildPipelineSectionWith(sctx, scm.ProviderBitbucket, false, true)
	if pipelineMD == "" || testingMD == "" {
		t.Fatalf("fixture rendered no pipeline or testing section:\n%q\n%q", pipelineMD, testingMD)
	}
	return assembleDraftPRBody(sctx, bitbucketDocWhatChanged, riskLine, testingMD, pipelineMD, "", 0, scm.ProviderBitbucket)
}
