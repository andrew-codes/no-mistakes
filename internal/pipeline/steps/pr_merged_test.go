package steps

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const (
	bbMergedURL = "https://bitbucket.org/test/repo/pull-requests/55"
	bbDeclined  = "https://bitbucket.org/test/repo/pull-requests/7"
	// missingSHA is a well-formed commit id this checkout does not have.
	missingSHA = "1111111111111111111111111111111111111111"
)

// newBitbucketPRStepContext builds a PR step context against the fake twg.
func newBitbucketPRStepContext(t *testing.T, api *fakeBitbucketAPI) (*pipeline.StepContext, *mockAgent, string, string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	return sctx, ag, dir, baseSHA, headSHA
}

func commitOnTopOf(t *testing.T, dir, parent string) string {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(testGitExecutable, append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	return run("commit-tree", parent+"^{tree}", "-p", parent, "-m", "follow-up merged elsewhere")
}

func requireNoPRWrites(t *testing.T, api *fakeBitbucketAPI) {
	t.Helper()
	if n := api.logCount("pull-requests create"); n != 0 {
		t.Fatalf("create calls = %d, want 0", n)
	}
	if n := api.logCount("pull-requests update"); n != 0 {
		t.Fatalf("update calls = %d, want 0", n)
	}
}

// The forge merged the PR at the run's head before the PR step ran. Nothing is
// created or updated, the merged PR becomes the run's PR, and the CI step then
// ends the run as a success on its first poll, naming the PR link.
func TestPRStep_BitbucketAlreadyMergedAtHeadCreatesNothing(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, ag, _, _, headSHA := newBitbucketPRStepContext(t, api)
	api.withPrimaryPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main", SourceCommit: headSHA})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != bbMergedURL || outcome.NeedsApproval || outcome.Skipped {
		t.Fatalf("outcome = %+v, want the merged PR recorded without approval or skip", outcome)
	}
	requireNoPRWrites(t, api)
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0: nothing is drafted for an already merged PR", len(ag.calls))
	}
	if api.logCount("--state OPEN") != 1 || api.logCount("--state MERGED") != 1 {
		t.Fatalf("expected one open and one merged lookup, log:\n%s", mustRead(t, api.logFile))
	}
	if !containsLog(logs, "already merged: "+bbMergedURL) {
		t.Fatalf("expected the already-merged fact with its link in the log, got %v", logs)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != bbMergedURL {
		t.Fatalf("PR URL = %v, want %q", run.PRURL, bbMergedURL)
	}

	// The CI step treats the recorded PR as satisfied: it reads MERGED, stops
	// watching and finishes as a success, with the link in its log.
	sctx.Run = run
	sctx.Config.CITimeout = 30 * time.Second
	logs = nil
	ciOutcome, err := driveCI(t, &CIStep{}, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if ciOutcome.NeedsApproval || ciOutcome.Skipped {
		t.Fatalf("CI outcome = %+v, want a clean finish", ciOutcome)
	}
	if !containsLog(logs, "PR has been merged: "+bbMergedURL) {
		t.Fatalf("expected the CI log to name the merged PR link, got %v", logs)
	}
	requireNoPRWrites(t, api)
	run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRState == nil || *run.PRState != "merged" {
		t.Fatalf("PR state = %v, want merged", run.PRState)
	}
}

// A merged head that contains the run's head (the forge merged a later
// commit of the same branch) still covers the run.
func TestPRStep_BitbucketMergedHeadContainingRunHeadCreatesNothing(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, dir, _, headSHA := newBitbucketPRStepContext(t, api)
	api.withPrimaryPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main", SourceCommit: commitOnTopOf(t, dir, headSHA)})

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != bbMergedURL {
		t.Fatalf("PR URL = %q, want %q", outcome.PRURL, bbMergedURL)
	}
	requireNoPRWrites(t, api)
}

// The branch gained commits the merged head lacks: a real follow-up, so a new
// PR is created as before.
func TestPRStep_BitbucketMergedButBranchAdvancedCreatesNewPR(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, baseSHA, _ := newBitbucketPRStepContext(t, api)
	// The merged head is the base commit: an ancestor of the run's head, which
	// therefore holds commits the merged head does not.
	api.withOtherPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main", SourceCommit: baseSHA})

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if api.logCount("pull-requests create") != 1 {
		t.Fatalf("create calls = %d, want 1", api.logCount("pull-requests create"))
	}
	if outcome.PRURL == bbMergedURL || outcome.PRURL == "" {
		t.Fatalf("PR URL = %q, want the newly created PR", outcome.PRURL)
	}
}

func TestPRStep_BitbucketMergedHeadNotAvailableLocallyCreatesNewPR(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, _, _ := newBitbucketPRStepContext(t, api)
	api.withOtherPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main", SourceCommit: missingSHA})

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if api.logCount("pull-requests create") != 1 {
		t.Fatalf("create calls = %d, want 1: a merged head that cannot be compared proves nothing", api.logCount("pull-requests create"))
	}
}

func TestPRStep_BitbucketMergedHeadNotReportedCreatesNewPR(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, _, _ := newBitbucketPRStepContext(t, api)
	api.withOtherPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main"})

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if api.logCount("pull-requests create") != 1 {
		t.Fatalf("create calls = %d, want 1", api.logCount("pull-requests create"))
	}
}

// A merged PR into another branch did not deliver the work to this run's base.
func TestPRStep_BitbucketMergedIntoDifferentBaseCreatesNewPR(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, _, headSHA := newBitbucketPRStepContext(t, api)
	api.withOtherPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "release", SourceCommit: headSHA})

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if api.logCount("pull-requests create") != 1 {
		t.Fatalf("create calls = %d, want 1", api.logCount("pull-requests create"))
	}
}

// A declined PR is never consulted: it does not block a new PR, even when its
// head is exactly the run's head.
func TestPRStep_BitbucketDeclinedPRDoesNotBlockNewPR(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, _, headSHA := newBitbucketPRStepContext(t, api)
	api.withOtherPR(bbFakePRForTest{ID: 7, URL: bbDeclined, State: "DECLINED", Source: "feature", Dest: "main", SourceCommit: headSHA})

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if api.logCount("pull-requests create") != 1 {
		t.Fatalf("create calls = %d, want 1", api.logCount("pull-requests create"))
	}
	if outcome.PRURL == bbDeclined || outcome.PRURL == "" {
		t.Fatalf("PR URL = %q, want the newly created PR", outcome.PRURL)
	}
	if api.logCount("DECLINED") != 0 {
		t.Fatalf("the declined state must never be queried, log:\n%s", mustRead(t, api.logFile))
	}
}

// With both a declined and a merged PR on the branch, the merged one decides.
func TestPRStep_BitbucketMergedPRWinsOverDeclinedDuplicate(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 0, "")
	sctx, _, _, _, headSHA := newBitbucketPRStepContext(t, api)
	api.withOtherPR(bbFakePRForTest{ID: 7, URL: bbDeclined, State: "DECLINED", Source: "feature", Dest: "main", SourceCommit: headSHA}).
		withOtherPR(bbFakePRForTest{ID: 55, URL: bbMergedURL, State: "MERGED", Source: "feature", Dest: "main", SourceCommit: headSHA})

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != bbMergedURL {
		t.Fatalf("PR URL = %q, want the merged PR %q", outcome.PRURL, bbMergedURL)
	}
	requireNoPRWrites(t, api)
}

// An open PR is updated as before and the merged lookup is never made.
func TestPRStep_BitbucketOpenPRSkipsMergedLookup(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")
	sctx, _, _, _, _ := newBitbucketPRStepContext(t, api)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if api.logCount("--state MERGED") != 0 {
		t.Fatalf("an existing open PR needs no merged lookup, log:\n%s", mustRead(t, api.logFile))
	}
	if api.logCount("pull-requests update") != 1 {
		t.Fatalf("update calls = %d, want 1", api.logCount("pull-requests update"))
	}
}

// An unreadable merged lookup fails the step rather than risk a duplicate.
func TestPRStep_BitbucketUnreadableMergedLookupFailsTheStep(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"not json": "twg: something went wrong",
		"null":     "null",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeBitbucketAPI(t, 0, "")
			api.env["FAKE_CLI_BB_QUERY_MERGED_RAW"] = raw
			sctx, _, _, _, _ := newBitbucketPRStepContext(t, api)

			_, err := (&PRStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "merged pull requests") {
				t.Fatalf("err = %v, want a merged-lookup failure", err)
			}
			requireNoPRWrites(t, api)
		})
	}
}

func TestPRStep_GitHubAlreadyMergedAtHeadCreatesNothing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	merged := `[{"number":7,"url":"https://github.com/test/repo/pull/7","baseRefName":"main","headRefOid":"` + headSHA + `"}]`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_MERGED_PR_LIST_JSON="+merged)

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != "https://github.com/test/repo/pull/7" {
		t.Fatalf("PR URL = %q, want the merged PR", outcome.PRURL)
	}
	log := mustRead(t, logFile)
	if strings.Contains(log, "pr create") || strings.Contains(log, "pr edit") {
		t.Fatalf("nothing may be created or edited for an already merged PR, got:\n%s", log)
	}
	if !strings.Contains(log, "--state open") || !strings.Contains(log, "--state merged --json number,url,baseRefName,headRefOid") {
		t.Fatalf("expected separate open and merged lookups, got:\n%s", log)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0", len(ag.calls))
	}
}

func TestPRStep_GitHubMergedButBranchAdvancedCreatesNewPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	merged := `[{"number":7,"url":"https://github.com/test/repo/pull/7","baseRefName":"main","headRefOid":"` + baseSHA + `"}]`
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_MERGED_PR_LIST_JSON="+merged)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mustRead(t, logFile), "pr create") {
		t.Fatal("expected a new PR for a branch that gained commits after the merge")
	}
}

func TestPRStep_GitHubUnreadableMergedLookupFailsTheStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_MERGED_PR_LIST_JSON=not json")

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "merged pull requests") {
		t.Fatalf("err = %v, want a merged-lookup failure", err)
	}
	if strings.Contains(mustRead(t, logFile), "pr create") {
		t.Fatal("no PR may be created after an unreadable merged lookup")
	}
}

func TestPRStep_GitLabAlreadyMergedAtHeadCreatesNothing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGlab(t, "")
	merged := `[{"iid":9,"web_url":"https://gitlab.com/test/repo/-/merge_requests/9","target_branch":"main","sha":"` + headSHA + `"}]`
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_MERGED_MR_LIST_JSON="+merged)
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != "https://gitlab.com/test/repo/-/merge_requests/9" {
		t.Fatalf("PR URL = %q, want the merged MR", outcome.PRURL)
	}
	log := mustRead(t, logFile)
	if strings.Contains(log, "mr create") || strings.Contains(log, "mr update") {
		t.Fatalf("nothing may be created or updated for an already merged MR, got:\n%s", log)
	}
	if !strings.Contains(log, "mr list --source-branch feature --merged") {
		t.Fatalf("expected a merged lookup, got:\n%s", log)
	}
}

func TestPRStep_GitLabMergedIntoDifferentBaseCreatesNewMR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGlab(t, "")
	merged := `[{"iid":9,"web_url":"https://gitlab.com/test/repo/-/merge_requests/9","target_branch":"release","sha":"` + headSHA + `"}]`
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_MERGED_MR_LIST_JSON="+merged)
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mustRead(t, logFile), "mr create") {
		t.Fatal("expected a new MR when the merged one targeted another branch")
	}
}

// The PR merges after the step created it: the CI step notices MERGED on its
// first poll, stops watching and finishes as a success without creating or
// updating anything.
func TestCIStep_BitbucketPRMergedDuringRunFinishesCleanly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("MERGED").
		withStatuses(`[{"name":"build","state":"INPROGRESS"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := driveCI(t, &CIStep{}, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.Skipped {
		t.Fatalf("outcome = %+v, want a clean finish", outcome)
	}
	if !containsLog(logs, "PR has been merged: "+prURL) {
		t.Fatalf("expected the merged log to name the PR link, got %v", logs)
	}
	requireNoPRWrites(t, api)
	if api.logCount("--statuses") != 0 {
		t.Fatal("a merged PR must stop the watch before any check is read")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
