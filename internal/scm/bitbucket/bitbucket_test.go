package bitbucket

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestParseRepoRef(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    RepoRef
		wantErr bool
	}{
		{"https url", "https://bitbucket.org/myws/myrepo.git", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"https url no dotgit", "https://bitbucket.org/myws/myrepo", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"scp-like", "git@bitbucket.org:myws/myrepo.git", RepoRef{Workspace: "myws", RepoSlug: "myrepo"}, false},
		{"unsupported host", "https://github.com/myws/myrepo.git", RepoRef{}, true},
		{"missing repo segment", "https://bitbucket.org/myws", RepoRef{}, true},
		{"empty", "", RepoRef{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRepoRef(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRepoRef(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepoRef(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRepoRef(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCapabilitiesReportsMergeableState(t *testing.T) {
	h := New(nil, nil, RepoRef{}, false)
	caps := h.Capabilities()
	if !caps.MergeableState {
		t.Fatal("expected MergeableState capability to be supported")
	}
	if !caps.FailedCheckLogs {
		t.Fatal("expected FailedCheckLogs capability to be supported")
	}
}

const mergeabilityKey = "twg bb pull-requests mergeability 42 --workspace ws --repo repo -o json --output-summary none"

func mergeStateFor(t *testing.T, stdout string) (scm.MergeableState, error) {
	t.Helper()
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		mergeabilityKey: {stdout: stdout},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	return h.GetMergeableState(context.Background(), &scm.PR{Number: "42"})
}

func TestGetMergeableState(t *testing.T) {
	const (
		gitPassed   = `{"type":"git_mergeability_check","status":"PASSED","blocking":false}`
		gitFailed   = `{"type":"git_mergeability_check","status":"FAILED","blocking":true}`
		gitUnknown  = `{"type":"git_mergeability_check","status":"UNKNOWN","blocking":false}`
		gitPending  = `{"type":"git_mergeability_check","status":"PENDING","blocking":false}`
		behindOK    = `{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"PASSED","blocking":false}`
		behindBad   = `{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"FAILED","blocking":true}`
		behindSoft  = `{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"FAILED","blocking":false}`
		behindUnset = `{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"FAILED"}`
		behindWait  = `{"type":"merge_check","check":{"kind":"maximum_commits_behind"},"status":"PENDING","blocking":false}`
		approvals   = `{"type":"merge_check","check":{"kind":"minimum_approvals"},"status":"FAILED","blocking":true}`
	)
	tests := []struct {
		name   string
		stdout string
		want   scm.MergeableState
	}{
		{"both checks pass", "[" + gitPassed + "," + behindOK + "]", scm.MergeableOK},
		{"git check alone passes", "[" + gitPassed + "]", scm.MergeableOK},
		{"git check failed is a conflict", "[" + gitFailed + "," + behindOK + "]", scm.MergeableConflict},
		{"git check failed without blocking is still a conflict", `[{"type":"git_mergeability_check","status":"FAILED"}]`, scm.MergeableConflict},
		{"blocking maximum_commits_behind failure", "[" + gitPassed + "," + behindBad + "]", scm.MergeableConflict},
		{"maximum_commits_behind failure with blocking unset", "[" + gitPassed + "," + behindUnset + "]", scm.MergeableConflict},
		{"non-blocking maximum_commits_behind failure", "[" + gitPassed + "," + behindSoft + "]", scm.MergeableOK},
		{"failure outranks a pending check", "[" + gitUnknown + "," + behindBad + "]", scm.MergeableConflict},
		{"unknown git check is pending", "[" + gitUnknown + "," + behindOK + "]", scm.MergeablePending},
		{"pending git check is pending", "[" + gitPending + "]", scm.MergeablePending},
		{"pending maximum_commits_behind is pending", "[" + gitPassed + "," + behindWait + "]", scm.MergeablePending},
		{"missing git check is pending", "[" + behindOK + "]", scm.MergeablePending},
		{"empty listing is pending", "[]", scm.MergeablePending},
		{"unrelated failing check is ignored", "[" + gitPassed + "," + approvals + "]", scm.MergeableOK},
		{"values envelope", `{"values":[` + gitFailed + `]}`, scm.MergeableConflict},
		{"statuses are case-insensitive", `[{"type":"git_mergeability_check","status":"failed"}]`, scm.MergeableConflict},
		{"type alone identifies maximum_commits_behind", `[` + gitPassed + `,{"type":"maximum_commits_behind","status":"FAILED","blocking":true}]`, scm.MergeableConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergeStateFor(t, tt.stdout)
			if err != nil {
				t.Fatalf("GetMergeableState() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("GetMergeableState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetMergeableStateFailsOnUnreadableOutput(t *testing.T) {
	for name, stdout := range map[string]string{
		"no json":        "something went wrong",
		"malformed":      "[{",
		"null":           "null",
		"object no list": `{"ok":false}`,
		"summary":        "summary: output too large to print inline\nfile: /tmp/x.json",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := mergeStateFor(t, stdout)
			if err == nil {
				t.Fatalf("GetMergeableState() = %q, want an error", got)
			}
			if got != "" {
				t.Fatalf("GetMergeableState() = %q alongside an error, want empty", got)
			}
		})
	}
}

func TestGetMergeableStateReturnsCLIError(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		mergeabilityKey: {stderr: "boom", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.GetMergeableState(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("expected the twg failure to surface")
	}
}

func TestGetMergeableStateRequiresPRNumber(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	for _, pr := range []*scm.PR{nil, {}} {
		if _, err := h.GetMergeableState(context.Background(), pr); err == nil {
			t.Fatalf("GetMergeableState(%+v) succeeded without a PR number", pr)
		}
	}
}

func TestAvailableReturnsErrorWhenCLIMissing(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return false }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err == nil {
		t.Fatal("expected error when twg CLI is not installed")
	}
}

func TestAvailableReturnsErrorOnAuthFailure(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg whoami": {stderr: "not logged in", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err == nil {
		t.Fatal("expected error when twg is not authenticated")
	}
}

func TestAvailableSucceedsWhenAuthenticated(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg whoami": {stdout: "Andrew Smith\n"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
}

func TestFindPRMatchesOpenPRForSourceBranch(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --dest main --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":42,"state":"OPEN","source":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" || pr.URL != "https://bitbucket.org/ws/repo/pull-requests/42" {
		t.Fatalf("FindPR() = %+v, want matched PR", pr)
	}
}

func TestFindPRSkipsForkPRWithSameSourceBranch(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":7,"state":"OPEN","source":{"repository":{"full_name":"someone/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/7"}}},` +
				`{"id":8,"state":"OPEN","source":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/8"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "8" {
		t.Fatalf("FindPR() = %+v, want the PR whose source repository is this repo", pr)
	}
}

func TestFindPRReturnsNilWhenOnlyForkPRsMatch(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":7,"state":"OPEN","source":{"repository":{"full_name":"someone/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/7"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil for a fork-only match", pr)
	}
}

func TestFindPRReturnsErrorWhenSourceRepositoryMissing(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":42,"state":"OPEN"}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err == nil {
		t.Fatalf("FindPR() = %+v, want error for an entry without source repository", pr)
	}
}

func TestFindPRReturnsNilWhenNoneOpen(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {stdout: "[]"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestFindPRReturnsErrorOnNonJSONOutput(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {stdout: ""},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for output with no JSON delimiter")
	}
}

func TestFindPRReturnsErrorOnMalformedJSON(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {stdout: `[{"id":`},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestFindPRReturnsErrorOnNullListing(t *testing.T) {
	// A literal JSON `null` (e.g. a nil slice marshaled at the top level) must
	// not be read as "no open PRs" - that reading would risk the PR step
	// creating a duplicate PR.
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {stdout: "null"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error for a null listing, not a silent 'no open PRs'")
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --workspace ws --repo repo -o json --output-summary none": {stderr: "boom", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected error propagated from twg CLI failure")
	}
}

func TestCreatePRWritesDescriptionToFileAndParsesResult(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	body := "line one\nline two\n"
	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: body})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "42" || pr.URL != "https://bitbucket.org/ws/repo/pull-requests/42" {
		t.Fatalf("CreatePR() = %+v", pr)
	}
	assertDescriptionRoundTrips(t, rec, body)
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if !strings.Contains(got, "--title T --source feature") || !strings.Contains(got, "--dest main") || !strings.Contains(got, "--workspace ws --repo repo -o json --output-summary none") {
		t.Fatalf("command = %q, missing expected flags", got)
	}
	if strings.Contains(got, "--draft") {
		t.Fatalf("command = %q, want no --draft when disabled", got)
	}
}

func TestCreatePRAddsDraftFlagWhenConfigured(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, true)

	if _, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "b"}); err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if !strings.Contains(got, "--draft") {
		t.Fatalf("command = %q, want --draft when configured", got)
	}
}

func TestUpdatePROmitsTitleWhenEmpty(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{
		stdout: `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: "", Body: "new body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	got := strings.Join(append([]string{rec[0].name}, rec[0].args...), " ")
	if strings.Contains(got, "--title") {
		t.Fatalf("command = %q, want no --title when content.Title is empty", got)
	}
	if !strings.Contains(got, "--pull-request 42") {
		t.Fatalf("command = %q, missing --pull-request", got)
	}
	assertDescriptionRoundTrips(t, rec, "new body")
}

func TestUpdatePRFailsClosedWithoutIdentity(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.UpdatePR(context.Background(), &scm.PR{}, scm.PRContent{}); err == nil {
		t.Fatal("expected error for missing PR number")
	}
	if _, err := h.UpdatePR(context.Background(), nil, scm.PRContent{}); err == nil {
		t.Fatal("expected error for nil PR")
	}
}

func TestGetPRStateNormalizesState(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"state":"MERGED","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	state, err := h.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want MERGED", state)
	}
}

func TestGetChecksReadsHydratedStatuses(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"_statuses":[{"key":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/42"}]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketFail || checks[0].ExecutionID != "42" {
		t.Fatalf("checks[0] = %+v", checks[0])
	}
}

func TestFetchFailedCheckTargetLogsFetchesFailedStepsPerBuild(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"source":{"commit":{"hash":"abc123"}},"_statuses":[{"name":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/1"}]}`,
		},
		"twg bb pipeline get --pipeline 1 --logs --failed-steps --lines 0 --output-summary none --workspace ws --repo repo": {
			stdout: "error log output\n",
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123def456", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "error log output" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "error log output")
	}
}

func TestFetchFailedCheckTargetLogsKeepsOnlyTheTailOfHugeOutput(t *testing.T) {
	// ~100 KB of output, over twice maxPipelineLogBytes, ending in the failure.
	huge := strings.Repeat("noise line\n", 9000) + "FINAL FAILURE\n"
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"source":{"commit":{"hash":"abc123"}},"_statuses":[{"name":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/1"}]}`,
		},
		"twg bb pipeline get --pipeline 1 --logs --failed-steps --lines 0 --output-summary none --workspace ws --repo repo": {
			stdout: huge,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123def456", []string{"build"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if len(huge) <= 2*maxPipelineLogBytes {
		t.Fatalf("fixture too small: %d bytes", len(huge))
	}
	if len(logs) > maxPipelineLogBytes {
		t.Fatalf("len(logs) = %d, want <= %d", len(logs), maxPipelineLogBytes)
	}
	if !strings.HasSuffix(logs, "FINAL FAILURE") {
		t.Fatalf("logs lost the tail of the output: ...%q", logs[max(0, len(logs)-40):])
	}
}

func TestTailBufferRetainsOnlyTheLastBytes(t *testing.T) {
	tail := &tailBuffer{max: 8}
	for _, chunk := range []string{"abc", "defg", "hijklmnopqrstuv", "wx"} {
		if n, err := tail.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := tail.String(); got != "qrstuvwx" {
		t.Fatalf("tail = %q, want %q", got, "qrstuvwx")
	}
}

func TestFetchFailedCheckTargetLogsRefusesWhenPRSourceCommitIsStale(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"source":{"commit":{"hash":"oldcommit"}},"_statuses":[{"name":"build","state":"FAILED","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/1"}]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "newcommitsha", []string{"build"}); err == nil {
		t.Fatal("expected error when the PR's source commit does not match the requested head")
	}
}

func TestFetchFailedCheckTargetLogsReturnsEmptyForNoTargets(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123", nil)
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty", logs)
	}
}

func TestLatestStatusesPicksNewestByTimestampNotArrayOrder(t *testing.T) {
	// The older pass appears first in the array; only the timestamp shows
	// it's stale. If LatestStatuses trusted array order it would keep the
	// pass and drop the newer failure.
	statuses := []CommitStatus{
		{Key: "build", State: "SUCCESSFUL", UpdatedOn: "2026-01-01T00:00:00Z"},
		{Key: "build", State: "FAILED", UpdatedOn: "2026-01-02T00:00:00Z"},
	}
	latest := LatestStatuses(statuses)
	if len(latest) != 1 || latest[0].State != "FAILED" {
		t.Fatalf("LatestStatuses() = %+v, want the newer FAILED status", latest)
	}
}

func TestLatestStatusesPrefersCreatedOnOverUpdatedOnForOrdering(t *testing.T) {
	// The older build (created first) has its updated_on touched after the
	// newer build was created - e.g. Bitbucket re-delivering the same status.
	// Ranking by updated_on would let the older build's SUCCESSFUL status
	// outrank the newer build's FAILED status and report the wrong verdict.
	statuses := []CommitStatus{
		{Key: "build", State: "SUCCESSFUL", CreatedOn: "2026-01-01T00:00:00Z", UpdatedOn: "2026-01-03T00:00:00Z"},
		{Key: "build", State: "FAILED", CreatedOn: "2026-01-02T00:00:00Z", UpdatedOn: "2026-01-02T00:00:00Z"},
	}
	latest := LatestStatuses(statuses)
	if len(latest) != 1 || latest[0].State != "FAILED" {
		t.Fatalf("LatestStatuses() = %+v, want the status with the newer created_on (FAILED)", latest)
	}
}

func TestGetChecksReportsTheNewerStatusRegardlessOfHydrationOrder(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --statuses --workspace ws --repo repo -o json --output-summary none": {
			stdout: `{"id":42,"_statuses":[` +
				`{"key":"build","state":"SUCCESSFUL","updated_on":"2026-01-01T00:00:00Z"},` +
				`{"key":"build","state":"FAILED","updated_on":"2026-01-02T00:00:00Z"}` +
				`]}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("GetChecks() = %+v, want a single failing check", checks)
	}
}

func TestBytesTrimToJSONSkipsBanner(t *testing.T) {
	if got := bytesTrimToJSON([]byte("notice: update available\n[]")); string(got) != "[]" {
		t.Fatalf("bytesTrimToJSON() = %q, want %q", got, "[]")
	}
	if got := bytesTrimToJSON([]byte("no json here")); got != nil {
		t.Fatalf("bytesTrimToJSON() = %q, want nil", got)
	}
}

// --- test doubles -----------------------------------------------------------

type bbTestResponse struct {
	stdout string
	// stdoutFile, when set, names a file the helper process prints instead of
	// stdout; it carries payloads too large for one environment value.
	stdoutFile string
	stderr     string
	code       int
}

func bbTestCmdFactory(responses map[string]bbTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = bbTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBBHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"BB_TEST_HELPER=1",
			"BB_TEST_STDOUT="+response.stdout,
			"BB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("BB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

type capturedCmd struct {
	name        string
	args        []string
	descPath    string
	descExists  bool
	descContent string
}

// capturingCmdFactory records every invocation (and the referenced
// --description-file's contents) into rec, and answers each one with the
// same fixed response.
func capturingCmdFactory(rec *[]capturedCmd, response bbTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := capturedCmd{name: name, args: append([]string(nil), args...)}
		for i, a := range args {
			if a == "--description-file" && i+1 < len(args) {
				c.descPath = args[i+1]
				if data, err := os.ReadFile(c.descPath); err == nil {
					c.descContent = string(data)
					c.descExists = true
				}
			}
		}
		*rec = append(*rec, c)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBBHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"BB_TEST_HELPER=1",
			"BB_TEST_STDOUT="+response.stdout,
			"BB_TEST_STDOUT_FILE="+response.stdoutFile,
			"BB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("BB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

// assertDescriptionRoundTrips verifies the recorded twg command passed its PR
// description through a temp file (not inline): the file held exactly body
// while the command ran, no argument contained the raw body, and (since the
// caller always defers removal) it existed at capture time.
func assertDescriptionRoundTrips(t *testing.T, rec []capturedCmd, body string) {
	t.Helper()
	if len(rec) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(rec))
	}
	c := rec[0]
	if c.descPath == "" {
		t.Fatal("--description-file was not passed")
	}
	if !c.descExists {
		t.Fatal("description temp file did not exist while the command ran")
	}
	if c.descContent != body {
		t.Fatalf("description file content = %q, want %q", c.descContent, body)
	}
	for _, a := range c.args {
		if a != body && strings.Contains(a, body) {
			t.Fatalf("raw body leaked into arg %q; it must travel via the temp file", a)
		}
	}
}

func TestBBHelperProcess(t *testing.T) {
	if os.Getenv("BB_TEST_HELPER") != "1" {
		return
	}
	out := os.Getenv("BB_TEST_STDOUT")
	if path := os.Getenv("BB_TEST_STDOUT_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			os.Exit(1)
		}
		out = string(data)
	}
	if _, err := fmt.Fprint(os.Stdout, out); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("BB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("BB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestEveryJSONCallPassesOutputSummaryNone(t *testing.T) {
	const flags = "-o json --output-summary none"
	pr := &scm.PR{Number: "42"}
	prJSON := `{"id":42,"title":"T","description":"D","state":"OPEN","source":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`
	tests := []struct {
		name string
		resp string
		run  func(h *Host) error
		want string
	}{
		{"FindPR", "[" + prJSON + "]", func(h *Host) error { _, err := h.FindPR(context.Background(), "feature", "main"); return err },
			"bb pull-requests query --source feature --state OPEN --dest main --workspace ws --repo repo " + flags},
		{"CreatePR", prJSON, func(h *Host) error {
			_, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "B"})
			return err
		}, "--workspace ws --repo repo " + flags},
		{"UpdatePR", prJSON, func(h *Host) error {
			_, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Title: "T", Body: "B"})
			return err
		}, "--workspace ws --repo repo " + flags},
		{"GetPRState", prJSON, func(h *Host) error { _, err := h.GetPRState(context.Background(), pr); return err },
			"bb pull-requests get 42 --workspace ws --repo repo " + flags},
		{"GetChecks", prJSON, func(h *Host) error { _, err := h.GetChecks(context.Background(), pr); return err },
			"bb pull-requests get 42 --statuses --workspace ws --repo repo " + flags},
		{"GetMergeableState", "[]", func(h *Host) error { _, err := h.GetMergeableState(context.Background(), pr); return err },
			"bb pull-requests mergeability 42 --workspace ws --repo repo " + flags},
		{"GetPRBaseBranch", `{"id":42,"destination":{"branch":{"name":"main"}}}`, func(h *Host) error { _, err := h.GetPRBaseBranch(context.Background(), pr); return err },
			"bb pull-requests get 42 --workspace ws --repo repo " + flags},
		{"SetPRBaseBranch", prJSON, func(h *Host) error { return h.SetPRBaseBranch(context.Background(), pr, "main") },
			"bb pull-requests update --pull-request 42 --dest main --workspace ws --repo repo " + flags},
		{"GetPRContent", prJSON, func(h *Host) error { _, err := h.GetPRContent(context.Background(), pr); return err },
			"bb pull-requests get 42 --workspace ws --repo repo " + flags},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec []capturedCmd
			h := New(capturingCmdFactory(&rec, bbTestResponse{stdout: tt.resp}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
			if err := tt.run(h); err != nil {
				t.Fatalf("%s error = %v", tt.name, err)
			}
			if len(rec) != 1 {
				t.Fatalf("recorded %d commands, want 1", len(rec))
			}
			got := strings.Join(rec[0].args, " ")
			if !strings.Contains(got, tt.want) {
				t.Fatalf("args = %q, want to contain %q", got, tt.want)
			}
			if strings.Count(got, "--output-summary") != 1 {
				t.Fatalf("args = %q, want exactly one --output-summary", got)
			}
		})
	}
}

func TestFetchFailedLogsPassOutputSummaryNone(t *testing.T) {
	var rec []capturedCmd
	h := New(capturingCmdFactory(&rec, bbTestResponse{stdout: "log text"}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.fetchPipelineLog(context.Background(), "1"); err != nil {
		t.Fatalf("fetchPipelineLog() error = %v", err)
	}
	got := strings.Join(rec[0].args, " ")
	if !strings.Contains(got, "--output-summary none") {
		t.Fatalf("args = %q, want --output-summary none", got)
	}
}

func TestFindPRParsesLargePRList(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 1; i <= 200; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"state":"OPEN","source":{"repository":{"full_name":"ws/repo"}},"title":"PR %d","description":%q,"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/%d"}}}`,
			i, i, strings.Repeat("long description ", 200), i)
	}
	b.WriteString("]")
	if b.Len() < 500_000 {
		t.Fatalf("fixture is only %d bytes; want a large payload", b.Len())
	}
	fixture := filepath.Join(t.TempDir(), "prs.json")
	if err := os.WriteFile(fixture, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(capturingCmdFactory(new([]capturedCmd), bbTestResponse{stdoutFile: fixture}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "1" {
		t.Fatalf("FindPR() = %+v, want first PR", pr)
	}
}

func TestFindPRRejectsSummaryEnvelope(t *testing.T) {
	h := New(capturingCmdFactory(new([]capturedCmd), bbTestResponse{stdout: "summary: output too large\nfile: /tmp/x.json\n"}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.FindPR(context.Background(), "feature", ""); err == nil {
		t.Fatal("expected an error for a YAML summary envelope")
	}
}

func TestFindMergedPRsReadsMergedHeadAndBase(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[
				{"id":41,"state":"MERGED","source":{"repository":{"full_name":"ws/repo"},"branch":{"name":"feature"},"commit":{"hash":"aaa"}},"destination":{"branch":{"name":"main"}}},
				{"id":55,"state":"MERGED","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/55"}},"source":{"repository":{"full_name":"ws/repo"},"branch":{"name":"feature"},"commit":{"hash":"bbb"}},"destination":{"branch":{"name":"release"}}}
			]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	prs, err := h.FindMergedPRs(context.Background(), "feature")
	if err != nil {
		t.Fatalf("FindMergedPRs() error = %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("FindMergedPRs() = %+v, want 2 PRs", prs)
	}
	// Newest first.
	if prs[0].Number != "55" || prs[0].URL != "https://bitbucket.org/ws/repo/pull-requests/55" || prs[0].HeadSHA != "bbb" || prs[0].BaseBranch != "release" {
		t.Fatalf("newest PR = %+v", prs[0])
	}
	if prs[1].Number != "41" || prs[1].HeadSHA != "aaa" || prs[1].BaseBranch != "main" {
		t.Fatalf("older PR = %+v", prs[1])
	}
}

func TestFindMergedPRsIgnoresOtherSourceBranches(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":9,"state":"MERGED","source":{"repository":{"full_name":"ws/repo"},"branch":{"name":"other"},"commit":{"hash":"aaa"}},"destination":{"branch":{"name":"main"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	prs, err := h.FindMergedPRs(context.Background(), "feature")
	if err != nil {
		t.Fatalf("FindMergedPRs() error = %v", err)
	}
	if len(prs) != 0 {
		t.Fatalf("FindMergedPRs() = %+v, want none", prs)
	}
}

func TestFindMergedPRsIgnoresForkPRsWithTheSameSourceBranch(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {
			stdout: `[{"id":9,"state":"MERGED","source":{"repository":{"full_name":"someone/repo"},"branch":{"name":"feature"},"commit":{"hash":"aaa"}},"destination":{"branch":{"name":"main"}}}]`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	prs, err := h.FindMergedPRs(context.Background(), "feature")
	if err != nil {
		t.Fatalf("FindMergedPRs() error = %v", err)
	}
	if len(prs) != 0 {
		t.Fatalf("FindMergedPRs() = %+v, want none for a fork-only match", prs)
	}
}

func TestFindMergedPRsReturnsEmptyWhenNoneMerged(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {stdout: "[]"},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	prs, err := h.FindMergedPRs(context.Background(), "feature")
	if err != nil || len(prs) != 0 {
		t.Fatalf("FindMergedPRs() = %+v, %v; want none and no error", prs, err)
	}
}

func TestFindMergedPRsFailsOnUnreadableOutput(t *testing.T) {
	for name, stdout := range map[string]string{
		"not json":  "something odd",
		"null":      "null",
		"envelope":  "summary: too large\nfile: /tmp/x.json",
		"missingid": `[{"state":"MERGED"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			h := New(bbTestCmdFactory(map[string]bbTestResponse{
				"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {stdout: stdout},
			}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
			if prs, err := h.FindMergedPRs(context.Background(), "feature"); err == nil {
				t.Fatalf("FindMergedPRs() = %+v, want an error", prs)
			}
		})
	}
}

func TestFindMergedPRsReturnsCLIError(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state MERGED --workspace ws --repo repo -o json --output-summary none": {stderr: "boom", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if _, err := h.FindMergedPRs(context.Background(), "feature"); err == nil {
		t.Fatal("expected a CLI failure to be an error, not an empty list")
	}
}
