package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	anchorOriginalIntent = "Let callers export a widget report as CSV, keeping the existing JSON export unchanged."
	anchorExistingPRURL  = "https://github.com/test/repo/pull/99"
)

// anchorCorrections are the restatements a driving agent hands the next run
// when it is told what the last one got wrong. None of them describes the
// purpose of the PR, and none may reach its Intent section.
var anchorCorrections = []struct {
	name   string
	intent string
}{
	{"behind base", "The PR is 40 commits behind main and not mergeable. Rebase the branch onto main and resolve drift."},
	{"merge conflict", "Resolve the merge conflict in report/export.go left after main moved, keeping both sides."},
	{"failing test", "TestExportCSV fails with an index out of range panic. Fix the test and rerun the pipeline."},
}

func anchorDraftAgent() *mockAgent {
	return &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: export CSV","body":"## What Changed\n\n- Adds a CSV exporter."}`)}, nil
	}}
}

// anchorRun records a run on the same repo and branch as base, the way the
// daemon does for a resubmission or a rerun: its own row, its own intent.
func anchorRun(t *testing.T, base *pipeline.StepContext, intent, source string) *pipeline.StepContext {
	t.Helper()
	run, err := base.DB.InsertRun(base.Repo.ID, base.Run.Branch, base.Run.HeadSHA, base.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.DB.UpdateRunIntent(run.ID, db.RunIntent{Summary: intent, Source: source, Score: 1}); err != nil {
		t.Fatal(err)
	}
	next := *base
	next.Run = run
	next.UserIntent, next.IntentSource = intent, source
	recordPRTestPush(t, &next)
	return &next
}

func anchorStep(t *testing.T, sctx *pipeline.StepContext, name types.StepName, status types.StepStatus, rounds ...[2]string) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rounds {
		var findings, fix *string
		if r[0] != "" {
			findings = &r[0]
		}
		if r[1] != "" {
			fix = &r[1]
		}
		if _, err := sctx.DB.InsertStepRound(sr.ID, i+1, r[1], findings, fix, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, status); err != nil {
		t.Fatal(err)
	}
}

func intentSection(body string) string {
	_, rest, ok := strings.Cut(body, "## Intent\n\n")
	if !ok {
		return ""
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	return strings.TrimSpace(section)
}

// The reported defect: run one opens the PR with the real intent; a later run
// triggered by a correction rewrites the description, and the Intent section
// becomes the correction. Reproduces on the unfixed tree (second body's Intent
// is the correction text), and pins the fixed behavior.
func TestPRStep_RerunAfterCorrectionKeepsTheOriginalIntentInTheDescription(t *testing.T) {
	t.Parallel()
	for _, tc := range anchorCorrections {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
			sctx1.Env, _ = fakeGH(t, "")
			if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
				t.Fatal(err)
			}
			sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
			anchorStep(t, sctx1, types.StepReview, types.StepStatusCompleted)
			if out, err := (&PRStep{}).Execute(sctx1); err != nil || out.PRURL != anchorExistingPRURL {
				t.Fatalf("first run: out=%+v err=%v", out, err)
			}
			bodyFile := envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE")
			first := readPRBodyFile(t, bodyFile)
			if got := intentSection(first); got != anchorOriginalIntent {
				t.Fatalf("first body Intent = %q, want the original intent:\n%s", got, first)
			}

			// The correction: a new run on the same branch, the PR now exists.
			sctx2 := anchorRun(t, sctx1, tc.intent, db.RunIntentSourceAgent)
			sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
			anchorStep(t, sctx2, types.StepReview, types.StepStatusCompleted)
			if out, err := (&PRStep{}).Execute(sctx2); err != nil || out.PRURL != anchorExistingPRURL {
				t.Fatalf("second run: out=%+v err=%v", out, err)
			}
			second := readPRBodyFile(t, bodyFile)
			t.Logf("before (first run) Intent: %q", intentSection(first))
			t.Logf("after  (second run) Intent: %q", intentSection(second))
			if got := intentSection(second); got != anchorOriginalIntent {
				t.Fatalf("second body Intent = %q, want the original intent unchanged:\n%s", got, second)
			}
			if strings.Contains(second, tc.intent) {
				t.Fatalf("correction leaked into the description:\n%s", second)
			}
			// The pin is scoped to the PR step: the run keeps its own intent for
			// every other consumer.
			if sctx2.UserIntent != tc.intent || sctx2.IntentSource != db.RunIntentSourceAgent {
				t.Fatalf("PR step left the run's intent pinned: %q (%s)", sctx2.UserIntent, sctx2.IntentSource)
			}
		})
	}
}

// A transcript-inferred intent on the rerun (the daemon only inherits explicit
// intent, so a rerun of an inferred run re-infers from a transcript that now
// ends in the correction) is no more able to replace the original.
func TestPRStep_InferredIntentOnRerunDoesNotReplaceTheOriginal(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx1.Env, _ = fakeGH(t, "")
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: "claude", Score: 0.9}); err != nil {
		t.Fatal(err)
	}
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, "claude"
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}

	sctx2 := anchorRun(t, sctx1, anchorCorrections[0].intent, "claude")
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	second := readPRBodyFile(t, envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE"))
	if got := intentSection(second); got != anchorOriginalIntent {
		t.Fatalf("Intent = %q, want the original:\n%s", got, second)
	}
}

// The fallback body (agent unavailable) renders Intent through a separate
// path from the drafted one.
func TestPRStep_FallbackBodyKeepsTheOriginalIntentAfterCorrection(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	failing := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, context.DeadlineExceeded
	}}
	sctx1 := newPRTestContext(t, failing, dir, base, head, config.Commands{})
	sctx1.Env, _ = fakeGH(t, "")
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}
	sctx2 := anchorRun(t, sctx1, anchorCorrections[1].intent, db.RunIntentSourceRerun)
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	second := readPRBodyFile(t, envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE"))
	if got := intentSection(second); got != anchorOriginalIntent {
		t.Fatalf("Intent = %q, want the original:\n%s", got, second)
	}
}

// Pipeline state is the current run's, not an accumulation. Run one's Test
// step failed then was fixed and Review flagged an issue; after the correction
// run two passes both, and the description must not carry run one's failures
// or any trace of the correction.
func TestPRStep_RerunDescriptionReflectsOnlyTheCurrentPipelineState(t *testing.T) {
	t.Parallel()
	const failure = "TestExportCSV panics: index out of range [3] with length 3"
	const reviewIssue = "exporter ignores the locale when formatting decimals"
	dir, base, head := setupGitRepo(t)
	sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx1.Env, _ = fakeGH(t, "")
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	testFindings := `{"findings":[{"id":"t1","severity":"error","description":"` + failure + `"}],"summary":"1 failing test"}`
	reviewFindings := `{"findings":[{"id":"r1","severity":"warning","file":"report/export.go","line":9,"description":"` + reviewIssue + `"}],"summary":"1 issue","risk_level":"low"}`
	anchorStep(t, sctx1, types.StepReview, types.StepStatusCompleted, [2]string{reviewFindings, "initial"}, [2]string{"", "auto_fix"})
	anchorStep(t, sctx1, types.StepTest, types.StepStatusCompleted, [2]string{testFindings, "initial"}, [2]string{"", "auto_fix"})
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}
	bodyFile := envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE")
	first := readPRBodyFile(t, bodyFile)
	if !strings.Contains(first, failure) || !strings.Contains(first, reviewIssue) {
		t.Fatalf("setup: first body should narrate its own run's findings:\n%s", first)
	}

	sctx2 := anchorRun(t, sctx1, anchorCorrections[2].intent, db.RunIntentSourceAgent)
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
	anchorStep(t, sctx2, types.StepReview, types.StepStatusCompleted, [2]string{"", ""})
	anchorStep(t, sctx2, types.StepTest, types.StepStatusCompleted, [2]string{"", ""})
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	second := readPRBodyFile(t, bodyFile)
	t.Logf("before:\n%s\n\nafter:\n%s", first, second)
	for _, stale := range []string{failure, reviewIssue, "auto-fixed", "TestExportCSV fails", "index out of range"} {
		if strings.Contains(second, stale) {
			t.Fatalf("description still carries %q from an earlier iteration:\n%s", stale, second)
		}
	}
	if !strings.Contains(second, "✅ **Test** - passed") || !strings.Contains(second, "✅ **Review** - passed") {
		t.Fatalf("description does not show the current passing state:\n%s", second)
	}
	if got := intentSection(second); got != anchorOriginalIntent {
		t.Fatalf("Intent = %q, want the original:\n%s", got, second)
	}
}

// With no earlier intent recorded against the PR (a PR the pipeline did not
// open, or one that predates the anchor) the run's own intent is published,
// exactly as before.
func TestPRStep_NoRecordedOriginalPublishesTheRunsOwnIntent(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx.Env, _ = fakeGH(t, anchorExistingPRURL)
	sctx.UserIntent, sctx.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, envEntry(sctx.Env, "FAKE_CLI_PR_BODY_FILE"))
	if got := intentSection(body); got != anchorOriginalIntent {
		t.Fatalf("Intent = %q, want the run's own intent:\n%s", got, body)
	}
}

// The anchor is per PR. A branch reused for a new PR after the old one was
// closed starts over with its own intent.
func TestPRStep_NewPRForAReusedBranchStartsANewIntent(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx1.Env, _ = fakeGH(t, "")
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}

	const newIntent = "Add a retry budget to the exporter so a slow sink cannot stall the queue."
	sctx2 := anchorRun(t, sctx1, newIntent, db.RunIntentSourceAgent)
	// No PR exists for the branch any more: the host lists none.
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL=")
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE"))
	if got := intentSection(body); got != newIntent {
		t.Fatalf("Intent = %q, want the new PR's own intent:\n%s", got, body)
	}
}

// Publication controls stay tighten-only against the current run: a rerun that
// omits the section removes it, the pin never restores it.
func TestPRStep_OriginalIntentStillHonorsOmission(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx1.Env, _ = fakeGH(t, "")
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}
	sctx2 := anchorRun(t, sctx1, anchorCorrections[0].intent, db.RunIntentSourceAgent)
	sctx2.Run.OmitIntent = true
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE"))
	if strings.Contains(body, "## Intent") || strings.Contains(body, anchorOriginalIntent) {
		t.Fatalf("omitted intent was republished:\n%s", body)
	}
}

// Bitbucket runs the same PR step against a different host and body skin.
func TestPRStep_BitbucketRerunKeepsTheOriginalIntent(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")
	sctx1 := newPRTestContext(t, anchorDraftAgent(), dir, base, head, config.Commands{})
	sctx1.Env = fakeBitbucketEnv(api.server.URL)
	sctx1.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}
	if got := intentSection(bitbucketPRDescriptionForTest(t, api.lastUpdateBody)); got != anchorOriginalIntent {
		t.Fatalf("first Intent = %q", got)
	}
	sctx2 := anchorRun(t, sctx1, anchorCorrections[0].intent, db.RunIntentSourceAgent)
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	if got := intentSection(bitbucketPRDescriptionForTest(t, api.lastUpdateBody)); got != anchorOriginalIntent {
		t.Fatalf("second Intent = %q, want the original", got)
	}
}

// The owned (pr.template) appendix renders Intent through its own path; the
// author's narrative is kept and the appendix refreshed, still from the
// original intent.
func TestPRStep_TemplateRerunKeepsTheOriginalIntentInTheAppendix(t *testing.T) {
	t.Parallel()
	sctx1, _, _ := templateTestContext(t)
	sctx1.Env, _ = fakeGH(t, "")
	sctx1.UserIntent, sctx1.IntentSource = anchorOriginalIntent, db.RunIntentSourceAgent
	if err := sctx1.DB.UpdateRunIntent(sctx1.Run.ID, db.RunIntent{Summary: anchorOriginalIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PRStep{}).Execute(sctx1); err != nil {
		t.Fatal(err)
	}
	bodyFile := envEntry(sctx1.Env, "FAKE_CLI_PR_BODY_FILE")
	if got := intentSection(readPRBodyFile(t, bodyFile)); !strings.HasPrefix(got, anchorOriginalIntent) {
		t.Fatalf("first body Intent = %q", got)
	}

	sctx2 := anchorRun(t, sctx1, anchorCorrections[0].intent, db.RunIntentSourceAgent)
	sctx2.Env = append(append([]string(nil), sctx1.Env...), "FAKE_CLI_PR_URL="+anchorExistingPRURL)
	anchorStep(t, sctx2, types.StepReview, types.StepStatusCompleted)
	if _, err := (&PRStep{}).Execute(sctx2); err != nil {
		t.Fatal(err)
	}
	second := readPRBodyFile(t, bodyFile)
	if !strings.Contains(second, anchorOriginalIntent) || strings.Contains(second, anchorCorrections[0].intent) {
		t.Fatalf("owned appendix lost the original intent or took the correction:\n%s", second)
	}
}
