package steps

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

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
	log := mustReadText(t, logFile)
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
	if !strings.Contains(mustReadText(t, logFile), "pr create") {
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
	if strings.Contains(mustReadText(t, logFile), "pr create") {
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
	log := mustReadText(t, logFile)
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
	if !strings.Contains(mustReadText(t, logFile), "mr create") {
		t.Fatal("expected a new MR when the merged one targeted another branch")
	}
}

func mustReadText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
