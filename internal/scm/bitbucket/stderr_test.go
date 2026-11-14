package bitbucket

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const stderrNotice = "[notice] twg update available {run `twg update`}\n"

// A successful twg may write notices on stderr; only stdout is JSON.
func TestJSONReadsIgnoreStderrNoticeOnSuccess(t *testing.T) {
	prJSON := `{"id":42,"state":"OPEN","source":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`
	const flags = " --workspace ws --repo repo -o json --output-summary none"
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests query --source feature --state OPEN --dest main" + flags: {stdout: "[" + prJSON + "]", stderr: stderrNotice},
		"twg bb pull-requests get 42" + flags:                                          {stdout: prJSON, stderr: stderrNotice},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	pr, err := h.FindPR(context.Background(), "feature", "main")
	if err != nil || pr == nil || pr.Number != "42" {
		t.Fatalf("FindPR() = %+v, %v; want PR 42 despite stderr notice", pr, err)
	}
	state, err := h.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if err != nil || state != scm.PRStateOpen {
		t.Fatalf("GetPRState() = %q, %v; want OPEN despite stderr notice", state, err)
	}
}

func TestJSONReadFailureSurfacesStderr(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 42 --workspace ws --repo repo -o json --output-summary none": {stdout: "partial", stderr: "auth expired\n", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	_, err := h.GetPRState(context.Background(), &scm.PR{Number: "42"})
	if err == nil || !strings.Contains(err.Error(), "auth expired") {
		t.Fatalf("error = %v, want stderr detail", err)
	}
}

func TestFetchPipelineLogKeepsStderrOutOfLogText(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pipeline get --pipeline 7 --logs --failed-steps --lines 0 --output-summary none --workspace ws --repo repo": {stdout: "step failed\n", stderr: stderrNotice},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	got, err := h.fetchPipelineLog(context.Background(), "7")
	if err != nil || got != "step failed" {
		t.Fatalf("fetchPipelineLog() = %q, %v; want stdout only", got, err)
	}
}

func TestFetchPipelineLogFailureSurfacesStderr(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pipeline get --pipeline 7 --logs --failed-steps --lines 0 --output-summary none --workspace ws --repo repo": {stderr: "no such pipeline\n", code: 1},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)

	if _, err := h.fetchPipelineLog(context.Background(), "7"); err == nil || !strings.Contains(err.Error(), "no such pipeline") {
		t.Fatalf("error = %v, want stderr detail", err)
	}
}
