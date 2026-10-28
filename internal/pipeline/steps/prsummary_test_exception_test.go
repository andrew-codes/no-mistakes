package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	exceptionCondition = "configured test command failed with exit code 1"
	exceptionReason    = "Recorded exception (host-load unit failures in frontend suites this change does not touch): 1 failed of 1200 in usePanelState.test.ts plus a jest worker crash"
	exceptionFindings  = `{"findings":[{"id":"t1","severity":"error","description":"configured test command failed with exit code 1","action":"ask-user"}],"summary":"1 error"}`
	cleanTestFindings  = `{"findings":[],"summary":""}`
)

// acceptedExceptionTestStep is the accepted-exception shape: the configured
// test command failed, an operator approved the run anyway with a recorded
// reason, so the step completed over the failure.
func acceptedExceptionTestStep(t *testing.T, tested ...string) ([]*db.StepResult, map[string][]*db.StepRound) {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Items:   []types.Finding{{ID: "t1", Severity: "error", Description: exceptionCondition, Action: types.ActionAskUser}},
		Summary: "1 error",
		Tested:  tested,
	})
	if err != nil {
		t.Fatal(err)
	}
	findings := string(raw)
	override := exceptionCondition
	approval := exceptionReason
	return []*db.StepResult{{
			ID: "t", StepName: types.StepTest, Status: types.StepStatusCompleted,
			FindingsJSON: &findings, OverrideReason: &override, ApprovalReason: &approval,
		}}, map[string][]*db.StepRound{
			"t": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 4000}},
		}
}

func unresolvedFailureTestStep() ([]*db.StepResult, map[string][]*db.StepRound) {
	findings := exceptionFindings
	return []*db.StepResult{{ID: "t", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}},
		map[string][]*db.StepRound{"t": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 4000}}}
}

func passedTestStep() ([]*db.StepResult, map[string][]*db.StepRound) {
	findings := cleanTestFindings
	return []*db.StepResult{{ID: "t", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}},
		map[string][]*db.StepRound{"t": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 4000}}}
}

func TestPipelineSummary_AcceptedTestExceptionIsNotAnUnexplainedError(t *testing.T) {
	t.Parallel()
	for name, provider := range map[string]scm.Provider{"github": scm.ProviderGitHub, "bitbucket": scm.ProviderBitbucket} {
		steps, rounds := acceptedExceptionTestStep(t)
		md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, provider, pipelineAttestationPolicy{Final: true, CIPassed: true})

		for _, want := range []string{
			"**Test** - passed with an accepted exception",
			"Accepted exception** - " + exceptionCondition + ".",
			"The failure was reviewed and accepted.",
			`Reason: "` + exceptionReason + `".`,
			"usePanelState.test.ts",
			"CI on the PR head passed.",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("%s: pipeline section is missing %q:\n%s", name, want, md)
			}
		}
		if strings.Contains(md, "1 error") || strings.Contains(md, "🚨") {
			t.Fatalf("%s: an accepted exception must not render as an error:\n%s", name, md)
		}
	}
}

func TestPipelineSummary_ExceptionOmitsCIWhenNotKnown(t *testing.T) {
	t.Parallel()
	steps, rounds := acceptedExceptionTestStep(t)
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	if !strings.Contains(md, "passed with an accepted exception") {
		t.Fatalf("exception must render before CI is known:\n%s", md)
	}
	if strings.Contains(md, "CI on the PR head") {
		t.Fatalf("no CI result is known yet, none may be claimed:\n%s", md)
	}
}

func TestPipelineSummary_ExceptionWithoutRecordedReasonSaysSo(t *testing.T) {
	t.Parallel()
	steps, rounds := acceptedExceptionTestStep(t)
	empty := ""
	steps[0].ApprovalReason = &empty
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	if !strings.Contains(md, "The failure was reviewed and accepted. No reason was recorded.") {
		t.Fatalf("an approval without a reason must say none was recorded:\n%s", md)
	}
	if strings.Contains(md, "no operator reason supplied") {
		t.Fatalf("the internal placeholder must not leak into the description:\n%s", md)
	}
}

func TestPipelineSummary_ExceptionReasonIsBoundedAndSingleLine(t *testing.T) {
	t.Parallel()
	steps, rounds := acceptedExceptionTestStep(t)
	long := "flaky\n" + strings.Repeat("host load ", 200)
	steps[0].ApprovalReason = &long
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket, pipelineAttestationPolicy{})
	if strings.Count(md, "host load") > maxTestExceptionReasonBytes/len("host load")+1 {
		t.Fatalf("the reason must be bounded:\n%s", md)
	}
	if !strings.Contains(md, `Reason: "flaky host load`) || !strings.Contains(md, `...".`) {
		t.Fatalf("the reason must be flattened to one line and end with an ellipsis:\n%s", md)
	}
}

func TestPipelineSummary_UnresolvedTestFailureStaysLoud(t *testing.T) {
	t.Parallel()
	steps, rounds := unresolvedFailureTestStep()
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{Final: true, CIPassed: true})
	for _, want := range []string{"**Test** - 1 error", "🚨 " + exceptionCondition} {
		if !strings.Contains(md, want) {
			t.Fatalf("an unapproved failure must stay loud, missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "accepted") {
		t.Fatalf("an unapproved failure must not read as accepted:\n%s", md)
	}
}

func TestPipelineSummary_PassedTestIsUnchanged(t *testing.T) {
	t.Parallel()
	steps, rounds := passedTestStep()
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{CIPassed: true})
	if !strings.Contains(md, "✅ **Test** - passed") || strings.Contains(md, "exception") || strings.Contains(md, "CI on the PR head") {
		t.Fatalf("a plain pass must render as before:\n%s", md)
	}
}

func TestTestingSummary_AcceptedTestExceptionReadsAsPassedWithException(t *testing.T) {
	t.Parallel()
	for name, flavor := range map[string]prBodyFlavor{"html": prBodyHTML, "markdown": prBodyMarkdown} {
		steps, rounds := acceptedExceptionTestStep(t, heredocTestCommand)
		md := buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: flavor, includeTestedDetails: true, ciPassed: true})
		for _, want := range []string{
			"Accepted exception** - " + exceptionCondition + ".",
			`Reason: "` + exceptionReason + `".`,
			"CI on the PR head passed.",
			"Outcome: ⚠️ passed with an accepted exception across 1 run",
			"- Ran `bash <<'SCRIPT'` (13-line script)",
		} {
			if !strings.Contains(md, want) {
				t.Fatalf("%s: testing section is missing %q:\n%s", name, want, md)
			}
		}
		for _, bad := range []string{"Outcome: 🚨", "1 error across", "set -uo pipefail", "```"} {
			if strings.Contains(md, bad) {
				t.Fatalf("%s: testing section must not contain %q:\n%s", name, bad, md)
			}
		}
	}
}

func TestTestingSummary_ExceptionWithNothingElseToShowStillExplainsItself(t *testing.T) {
	t.Parallel()
	steps, rounds := acceptedExceptionTestStep(t)
	md := buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: prBodyMarkdown})
	if !strings.HasPrefix(md, "## Testing\n\n- ⚠️ **Test** - passed with an accepted exception\n- ⚠️ **Accepted exception** - ") {
		t.Fatalf("the status line must be followed by its explanation:\n%s", md)
	}
}

func TestTestingSummary_UnresolvedFailureKeepsErrorOutcome(t *testing.T) {
	t.Parallel()
	steps, rounds := unresolvedFailureTestStep()
	md := buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: prBodyHTML, includeTestedDetails: true})
	if !strings.Contains(md, "**Test** - 1 error") {
		t.Fatalf("an unapproved failure must keep its error outcome:\n%s", md)
	}
	if strings.Contains(md, "accepted") {
		t.Fatalf("an unapproved failure must not read as accepted:\n%s", md)
	}
}

func TestTestExceptionDetailsNameFailingSuites(t *testing.T) {
	t.Parallel()
	steps, rounds := acceptedExceptionTestStep(t)
	findings := `{"findings":[{"id":"t1","severity":"error","description":"configured test command failed with exit code 1"},{"id":"t2","severity":"error","description":"FAIL src/usePanelState.test.ts"},{"id":"t3","severity":"error","description":"FAIL src/usePanelState.test.ts"}],"summary":"3 errors"}`
	steps[0].FindingsJSON = &findings
	ex, ok := testExceptionFor(steps[0], rounds["t"])
	if !ok {
		t.Fatal("expected an exception")
	}
	if len(ex.Details) != 1 || ex.Details[0] != "FAIL src/usePanelState.test.ts" {
		t.Fatalf("details = %q, want the one distinct suite finding", ex.Details)
	}
}

func TestFinalCIPassed(t *testing.T) {
	t.Parallel()
	empty, reason := "", "checks still failing"
	ciStep := func(status types.StepStatus, override *string) []*db.StepResult {
		return []*db.StepResult{{StepName: types.StepCI, Status: status, OverrideReason: override}}
	}
	for name, tc := range map[string]struct {
		run   *db.Run
		steps []*db.StepResult
		want  bool
	}{
		"green":                    {&db.Run{Status: types.RunCompleted}, ciStep(types.StepStatusCompleted, nil), true},
		"checks passed":            {&db.Run{Status: types.RunChecksPassed}, ciStep(types.StepStatusCompleted, &empty), true},
		"approved over failing":    {&db.Run{Status: types.RunCompleted}, ciStep(types.StepStatusCompleted, &reason), false},
		"declared no CI":           {&db.Run{Status: types.RunCompleted, CIReadyNoCI: true}, ciStep(types.StepStatusCompleted, nil), false},
		"CI skipped":               {&db.Run{Status: types.RunCompleted}, ciStep(types.StepStatusSkipped, nil), false},
		"no CI step":               {&db.Run{Status: types.RunCompleted}, nil, false},
		"run failed":               {&db.Run{Status: types.RunFailed}, ciStep(types.StepStatusCompleted, nil), false},
		"nil run is never passing": {nil, ciStep(types.StepStatusCompleted, nil), false},
	} {
		if got := finalCIPassed(tc.run, tc.steps); got != tc.want {
			t.Errorf("%s: finalCIPassed = %v, want %v", name, got, tc.want)
		}
	}
}

func TestFinalRunOutcomeLine_TestException(t *testing.T) {
	t.Parallel()
	run := &db.Run{Status: types.RunCompleted, HeadSHA: testPipelineHeadSHA}
	exceptionSteps, _ := acceptedExceptionTestStep(t)
	ci := &db.StepResult{StepName: types.StepCI, Status: types.StepStatusCompleted}

	got := finalRunOutcomeLine(run, append(exceptionSteps, ci))
	want := "**Run outcome:** ✅ pipeline passed on `01234567` with an accepted Test exception; CI on the PR head passed."
	if got != want {
		t.Fatalf("outcome = %q, want %q", got, want)
	}
	if got := finalRunOutcomeLine(run, exceptionSteps); strings.Contains(got, "CI on the PR head") || !strings.Contains(got, "with an accepted Test exception.") {
		t.Fatalf("without a known CI result the outcome must not claim one: %q", got)
	}
	passedSteps, _ := passedTestStep()
	if got := finalRunOutcomeLine(run, passedSteps); got != "**Run outcome:** ✅ pipeline passed on `01234567`." {
		t.Fatalf("a plain pass must keep its outcome line, got %q", got)
	}
}

// The 6160 case end to end: the run record carries an approved Test exception
// and a green CI step, and the description the run's end writes explains both.
func TestFinalizeRun_BitbucketDescriptionExplainsAnApprovedTestException(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, bbBasePRURL)
	c := bbBaseContext(t, api, "")
	seedRefreshRun(t, c, types.StepStatusAwaitingApproval, &exceptionRoundFindings)

	steps, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}
		if err := c.DB.SetTestApprovalReason(sr.ID, exceptionReason); err != nil {
			t.Fatal(err)
		}
		if err := c.DB.SetStepOverrideReason(sr.ID, exceptionCondition); err != nil {
			t.Fatal(err)
		}
	}
	step := &PRStep{}
	if _, err := step.Execute(c); err != nil {
		t.Fatal(err)
	}
	for _, sr := range steps {
		if sr.StepName == types.StepTest {
			if err := c.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
	}
	ci, err := c.DB.InsertStepResult(c.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DB.UpdateStepStatus(ci.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.UpdateRunStatus(c.Run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	c.Run.Status = types.RunCompleted

	if err := step.FinalizeRun(c); err != nil {
		t.Fatal(err)
	}
	after := api.lastDescription(t)
	for _, want := range []string{
		"**Run outcome:** ✅ pipeline passed on",
		"with an accepted Test exception; CI on the PR head passed.",
		"**Test** - passed with an accepted exception",
		`Reason: "` + exceptionReason + `".`,
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("final description is missing %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "**Test** - 1 error") || strings.Contains(after, "🚨") {
		t.Fatalf("an approved exception must not read as an error:\n%s", after)
	}
}

var exceptionRoundFindings = exceptionFindings
