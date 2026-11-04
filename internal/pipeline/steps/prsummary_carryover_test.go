package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	carryTotals = "Jest 10 of 1977 suites and 18 of 28787 tests failed, Cypress 1 of 142 specs failed"
	carryReason = "Test step approved by the captain. Totals: " + carryTotals + "."
)

func insertCarryStep(t *testing.T, d *db.DB, runID string, name types.StepName, status types.StepStatus, findings *string) *db.StepResult {
	t.Helper()
	sr, err := d.InsertStepResult(runID, name)
	if err != nil {
		t.Fatal(err)
	}
	if findings != nil {
		if _, err := d.InsertStepRound(sr.ID, 1, "initial", findings, nil, 1200); err != nil {
			t.Fatal(err)
		}
	}
	if status == types.StepStatusSkipped {
		if err := d.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	} else if err := d.UpdateStepStatus(sr.ID, status); err != nil {
		t.Fatal(err)
	}
	return sr
}

// acceptedExceptionFindings is what the Test step of the validated run
// recorded: a failed command, a live-validation verdict and the scenarios.
func acceptedExceptionFindings(t *testing.T, head string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Items:   []types.Finding{{ID: "t1", Severity: "error", Description: exceptionCondition, Action: types.ActionAskUser}},
		Summary: "1 error",
		Tested:  []string{"`yarn test:unit`"},
		Scenarios: []types.TestScenario{
			{Name: "home draft clears", Result: types.ScenarioResultPass, Live: true, Evidence: "home.png"},
			{Name: "streaming resumes", Result: types.ScenarioResultUntested, Reason: "no sandbox account"},
		},
		Verdict:       types.TestVerdictGo,
		TestedHeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const carryPRURL = "https://bitbucket.org/test/repo/pull-requests/42"

// carryContext is a PR step whose run already owns Bitbucket PR 42 and keeps
// PRs draft until they are ready.
func carryContext(t *testing.T, api *fakeBitbucketAPI) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	owned := carryPRURL
	sctx.Run.PRURL = &owned
	sctx.Config.Providers.Bitbucket.DraftPullRequests = true
	return sctx
}

// refreshFixture is a branch whose head an earlier run fully validated, and a
// rerun now refreshing the description with the passing steps skipped. The
// earlier run is the context's original run; the rerun is created after it.
type refreshFixture struct {
	c       *pipeline.StepContext
	earlier *db.Run
}

// earlierHead, when set, is the head the earlier run is recorded at instead of
// the rerun's, so the rerun is on a head the earlier run never validated.
func newRefreshFixture(t *testing.T, api *fakeBitbucketAPI, earlierHead string) *refreshFixture {
	t.Helper()
	c := carryContext(t, api)
	earlier := c.Run
	head := earlier.HeadSHA
	d := c.DB

	findings := acceptedExceptionFindings(t, head)
	cleanReview := `{"findings":[],"summary":"","risk_level":"low","risk_rationale":"Well bounded.","reviewed_paths":["a.go"]}`
	insertCarryStep(t, d, earlier.ID, types.StepReview, types.StepStatusCompleted, &cleanReview)
	// The approval is recorded while the gate is parked, then the step completes.
	test := insertCarryStep(t, d, earlier.ID, types.StepTest, types.StepStatusAwaitingApproval, &findings)
	if err := d.SetStepOverrideReason(test.ID, exceptionCondition); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTestApprovalReason(test.ID, carryReason); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(test.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	clean := cleanFindings
	insertCarryStep(t, d, earlier.ID, types.StepDocument, types.StepStatusCompleted, &clean)
	insertCarryStep(t, d, earlier.ID, types.StepLint, types.StepStatusCompleted, &clean)
	if err := d.UpdateRunPushBinding(earlier.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(earlier.ID, carryPRURL); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(earlier.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	rerun, err := d.InsertRun(c.Repo.ID, earlier.Branch, head, earlier.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if earlierHead != "" {
		if err := d.UpdateRunHeadSHA(earlier.ID, earlierHead); err != nil {
			t.Fatal(err)
		}
	}
	owned := carryPRURL
	rerun.PRURL = &owned
	c.Run = rerun
	insertCarryStep(t, d, rerun.ID, types.StepReview, types.StepStatusCompleted, &cleanReview)
	insertCarryStep(t, d, rerun.ID, types.StepTest, types.StepStatusSkipped, nil)
	insertCarryStep(t, d, rerun.ID, types.StepDocument, types.StepStatusSkipped, nil)
	insertCarryStep(t, d, rerun.ID, types.StepLint, types.StepStatusSkipped, nil)
	insertCarryStep(t, d, rerun.ID, types.StepPR, types.StepStatusCompleted, nil)
	return &refreshFixture{c: c, earlier: earlier}
}

// The captain's case: a refresh with review rerun and the rest skipped must
// not drop what the same head was already validated against.
func TestPRStep_RerunOnTheSameHeadCarriesSkippedStepResultsForward(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, "")

	if _, err := (&PRStep{}).Execute(f.c); err != nil {
		t.Fatal(err)
	}
	body := api.lastDescription(t)
	for _, want := range []string{
		"**Test** - passed with an accepted exception (" + carriedOverNote + ")",
		"**Document** - passed (" + carriedOverNote + ")",
		"**Lint** - passed (" + carriedOverNote + ")",
		// The accepted exception, its reason and totals, and the verdict.
		"Accepted exception** - " + exceptionCondition,
		carryTotals,
		"Live validation: ✅ go - 1 of 2 scenarios driven live against the product",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("description is missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"**Test** - skipped", "**Document** - skipped", "**Lint** - skipped"} {
		if strings.Contains(body, bad) {
			t.Fatalf("a step an earlier run on this head passed must not read %q:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "## Intent") && strings.Index(body, "## Testing") < 0 {
		t.Fatalf("setup: expected the usual sections:\n%s", body)
	}
}

// The head moved: the earlier run validated different code, so nothing is
// carried and the skipped steps are reported as skipped.
func TestPRStep_RerunOnAChangedHeadDoesNotCarryResultsForward(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, strings.Repeat("b", 40))

	if _, err := (&PRStep{}).Execute(f.c); err != nil {
		t.Fatal(err)
	}
	body := api.lastDescription(t)
	for _, want := range []string{"**Test** - skipped", "**Document** - skipped", "**Lint** - skipped"} {
		if !strings.Contains(body, want) {
			t.Fatalf("a skipped step on a new head must stay skipped, missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, carriedOverNote) || strings.Contains(body, "Accepted exception") || strings.Contains(body, "Live validation") {
		t.Fatalf("nothing validated on another head may be carried over:\n%s", body)
	}
}

// A chain of refreshes keeps the original result: the middle run skipped Test
// too, so the lookup continues to the run that actually ran it.
func TestCarriedSkippedSteps_ChainOfRefreshesReachesTheRunThatExecutedTheStep(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, "")
	d := f.c.DB
	// A second refresh after the one the fixture made: the fixture's rerun
	// becomes the middle run, which skipped Test.
	third, err := d.InsertRun(f.c.Repo.ID, f.earlier.Branch, f.earlier.HeadSHA, f.earlier.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	f.c.Run = third
	insertCarryStep(t, d, third.ID, types.StepTest, types.StepStatusSkipped, nil)
	steps, err := d.GetStepsByRun(third.ID)
	if err != nil {
		t.Fatal(err)
	}

	carried := carriedSkippedSteps(f.c, steps)
	if len(carried) != 1 {
		t.Fatalf("carried = %d steps, want the Test result only", len(carried))
	}
	for _, c := range carried {
		if c.Step.StepName != types.StepTest || c.Step.RunID != f.earlier.ID {
			t.Fatalf("carried %s from run %s, want Test from the run that executed it (%s)", c.Step.StepName, c.Step.RunID, f.earlier.ID)
		}
	}
}

// A step the earlier run did not complete has nothing to carry.
func TestCarriedSkippedSteps_OnlyACompletedStepIsCarried(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, "")
	earlierSteps, err := f.c.DB.GetStepsByRun(f.earlier.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range earlierSteps {
		if sr.StepName == types.StepTest {
			if err := f.c.DB.UpdateStepStatus(sr.ID, types.StepStatusFailed); err != nil {
				t.Fatal(err)
			}
		}
	}
	steps, err := f.c.DB.GetStepsByRun(f.c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range carriedSkippedSteps(f.c, steps) {
		if c.Step.StepName == types.StepTest {
			t.Fatal("a Test step that failed in the earlier run must not be carried as a result")
		}
	}
}

// The attestation is what other tooling decides on; a carried result is
// presentation only, so it still says this run skipped the step.
func TestPipelineSummary_CarriedStepKeepsTheAttestationSkipped(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{{ID: "now", StepName: types.StepTest, Status: types.StepStatusSkipped}}
	findings := cleanTestFindings
	prior := &db.StepResult{ID: "then", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}
	carried := map[string]*carriedStep{"now": {Step: prior, Rounds: []*db.StepRound{{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 100}}}}

	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{Carried: carried})

	if !strings.Contains(md, "**Test** - passed ("+carriedOverNote+")") {
		t.Fatalf("the visible line should carry the earlier result:\n%s", md)
	}
	if !strings.Contains(md, `"step":"test","status":"skipped"`) {
		t.Fatalf("the attestation must keep this run's own status:\n%s", md)
	}
}

func TestCarriedSkippedSteps_NothingWhenNoStepWasSkipped(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, "")
	steps := []*db.StepResult{{ID: "r", StepName: types.StepReview, Status: types.StepStatusCompleted}}
	if got := carriedSkippedSteps(f.c, steps); got != nil {
		t.Fatalf("carried = %v, want nothing", got)
	}
}

func riskReviewFindings(level, rationale string, paths ...string) *string {
	raw, _ := json.Marshal(types.Findings{Items: []types.Finding{}, RiskLevel: level, RiskRationale: rationale, ReviewedPaths: paths})
	s := string(raw)
	return &s
}

// The 6212 refresh: its review read only the latest commit while an earlier
// review of the same head covered the whole PR.
func TestCarriedRiskLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		prior, current *string
		skipped        bool
		want           string
	}{
		"fuller earlier review is carried": {
			prior:   riskReviewFindings("low", "Whole PR is a bounded autoscaler change.", "a.go", "b.go", "c.go"),
			current: riskReviewFindings("low", "The latest commit only deletes three keys.", "a.go"),
			skipped: true,
			want:    "✅ Low: Whole PR is a bounded autoscaler change. (" + carriedOverNote + ")",
		},
		"never softens a higher current risk": {
			prior:   riskReviewFindings("low", "Whole PR is bounded.", "a.go", "b.go"),
			current: riskReviewFindings("medium", "Latest commit is risky.", "a.go"),
			skipped: true,
			want:    "⚠️ Medium: Latest commit is risky.",
		},
		"an earlier review that covered less is not carried": {
			prior:   riskReviewFindings("low", "Only one file.", "a.go"),
			current: riskReviewFindings("low", "Two files.", "a.go", "b.go"),
			skipped: true,
			want:    "✅ Low: Two files.",
		},
		"an earlier review that missed a path this one read is not carried": {
			prior:   riskReviewFindings("low", "Other files.", "x.go", "y.go", "z.go"),
			current: riskReviewFindings("low", "This file.", "a.go"),
			skipped: true,
			want:    "✅ Low: This file.",
		},
		"a run that skipped nothing is left alone": {
			prior:   riskReviewFindings("low", "Whole PR is bounded.", "a.go", "b.go", "c.go"),
			current: riskReviewFindings("low", "The latest commit only.", "a.go"),
			skipped: false,
			want:    "✅ Low: The latest commit only.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeBitbucketAPI(t, 42, carryPRURL)
			c := carryContext(t, api)
			earlier := c.Run
			insertCarryStep(t, c.DB, earlier.ID, types.StepReview, types.StepStatusCompleted, tc.prior)
			rerun, err := c.DB.InsertRun(c.Repo.ID, earlier.Branch, earlier.HeadSHA, earlier.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			c.Run = rerun
			insertCarryStep(t, c.DB, rerun.ID, types.StepReview, types.StepStatusCompleted, tc.current)
			if tc.skipped {
				insertCarryStep(t, c.DB, rerun.ID, types.StepTest, types.StepStatusSkipped, nil)
			}
			steps, err := c.DB.GetStepsByRun(rerun.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds := map[string][]*db.StepRound{}
			for _, sr := range steps {
				r, err := c.DB.GetRoundsByStep(sr.ID)
				if err != nil {
					t.Fatal(err)
				}
				rounds[sr.ID] = r
			}
			current := extractRiskLine(steps, rounds)

			if got := carriedRiskLine(c, steps, rounds, current); got != tc.want {
				t.Fatalf("risk line = %q, want %q", got, tc.want)
			}
		})
	}
}
