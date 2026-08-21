package bitbucket

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseRepoRefRejectsLookalikeHosts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "rejects lookalike host",
			raw:     "https://bitbucket.org.evil.example/workspace/repo.git",
			wantErr: `unsupported Bitbucket host "bitbucket.org.evil.example"`,
		},
		{
			name: "accepts exact host",
			raw:  "https://bitbucket.org/workspace/repo.git",
		},
		{
			name: "accepts subdomain host",
			raw:  "https://foo.bitbucket.org/workspace/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := ParseRepoRef(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("error = %q, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepoRef returned error: %v", err)
			}
			if repo != (RepoRef{Workspace: "workspace", RepoSlug: "repo"}) {
				t.Fatalf("repo = %#v, want workspace/repo", repo)
			}
		})
	}
}

// fakeTwgResponse describes what the fake twg binary should emit for one
// matched invocation.
type fakeTwgResponse struct {
	Stdout   string `json:"stdout"`
	ExitCode int    `json:"exitCode"`
}

// newFakeTwgClient builds a Client whose CmdFactory invokes this test binary
// re-executed as a fake "twg" that replies from responses, keyed by the exact
// argv (joined with a unit separator) the Client is expected to invoke.
// logFile records one line per invocation (joined argv) for assertions.
func newFakeTwgClient(t *testing.T, responses map[string]fakeTwgResponse) (*Client, string) {
	t.Helper()
	binDir := fakeTwgBinDir(t)
	linkTestBinaryAs(t, binDir, "twg")

	respFile := filepath.Join(t.TempDir(), "responses.json")
	data, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(respFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "twg.log")

	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_TWG_MODE=1",
		"FAKE_TWG_RESPONSES=" + respFile,
		"FAKE_TWG_LOG=" + logFile,
	}

	cmdFactory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// exec.CommandContext resolves name against the CALLING process's PATH
		// immediately, before cmd.Env below ever takes effect for the child -
		// so the binary must be resolved from binDir explicitly here, or this
		// would silently invoke a real twg found on the test runner's PATH.
		resolved := filepath.Join(binDir, name)
		if runtime.GOOS == "windows" {
			resolved += ".exe"
		}
		cmd := exec.CommandContext(ctx, resolved, args...)
		cmd.Env = env
		return cmd
	}
	return NewClient(cmdFactory), logFile
}

// fakeTwgBinDir creates a temporary directory for the fake twg binary.
// Unlike t.TempDir(), cleanup tolerates a lingering file lock on the
// just-executed twg.exe on Windows (Defender's on-execution scan holds
// it briefly after the child process exits), which would otherwise fail
// TempDir's own RemoveAll with "Access is denied".
func fakeTwgBinDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "faketwg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return dir
}

func linkTestBinaryAs(t *testing.T, binDir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(binDir, name)
	if err := os.Link(exe, dst); err != nil {
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func readFakeTwgLog(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func envelope(t *testing.T, data string) string {
	t.Helper()
	return `{"apiVersion":"v2","command":"test","data":` + data + `}`
}

func TestClient_FindOpenPRBySourceBranch_QueriesOpenPRsByBranchAndDest(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fquery\x1f--scope\x1frepo\x1f--source\x1ffeature\x1f--state\x1fOPEN\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--dest\x1fmain\x1f--output\x1fjson"
	client, logFile := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `[{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}}}]`)},
	})

	pr, err := client.FindOpenPRBySourceBranch(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.ID != 42 {
		t.Fatalf("pr = %#v, want id 42", pr)
	}
	if pr.URL != "https://bitbucket.org/test/repo/pull-requests/42" {
		t.Fatalf("pr.URL = %q", pr.URL)
	}
	if got := readFakeTwgLog(t, logFile); len(got) != 1 {
		t.Fatalf("invocations = %v, want exactly 1", got)
	}
}

func TestClient_FindOpenPRBySourceBranch_NoResultsReturnsNil(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fquery\x1f--scope\x1frepo\x1f--source\x1ffeature\x1f--state\x1fOPEN\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `[]`)},
	})

	pr, err := client.FindOpenPRBySourceBranch(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	if pr != nil {
		t.Fatalf("pr = %#v, want nil", pr)
	}
}

func TestClient_CreatePR(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fcreate\x1f--title\x1ffeat: thing\x1f--source\x1ffeature\x1f--dest\x1fmain\x1f--description\x1fbody text\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, logFile := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"id":99,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/99"}}}`)},
	})

	pr, err := client.CreatePR(context.Background(), repo, "feature", "main", "feat: thing", "body text")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.ID != 99 {
		t.Fatalf("pr = %#v, want id 99", pr)
	}
	if got := readFakeTwgLog(t, logFile); len(got) != 1 {
		t.Fatalf("invocations = %v, want exactly 1", got)
	}
}

func TestClient_UpdatePR(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fupdate\x1f--pull-request\x1f42\x1f--title\x1fnew title\x1f--description\x1fnew body\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}}}`)},
	})

	pr, err := client.UpdatePR(context.Background(), repo, 42, "new title", "new body")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.ID != 42 {
		t.Fatalf("pr = %#v, want id 42", pr)
	}
}

func TestClient_GetPR(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fget\x1f42\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"id":42,"state":"MERGED","source":{"commit":{"hash":"abc123"}},"links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}}}`)},
	})

	pr, err := client.GetPR(context.Background(), repo, 42)
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.State != "MERGED" || pr.SourceCommitHash != "abc123" {
		t.Fatalf("pr = %#v", pr)
	}
}

func TestClient_ListPRStatuses(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fget\x1f42\x1f--statuses\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"id":42,"_statuses":[{"name":"build","state":"SUCCESSFUL"},{"name":"tests","state":"FAILED","url":"https://bitbucket.org/test/repo/pipelines/results/{pipe-1}"}]}`)},
	})

	statuses, err := client.ListPRStatuses(context.Background(), repo, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}
	if statuses[0].Name != "build" || statuses[1].Name != "tests" {
		t.Fatalf("statuses = %#v, want build then tests", statuses)
	}
}

func TestClient_GetFailedStepLog(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpipeline\x1fget\x1f--pipeline\x1f{pipe-1}\x1f--logs\x1f--failed-steps\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"steps":[{"state":{"result":{"name":"FAILED"}},"log":"boom: test failed\n"}]}`)},
	})

	logOutput, err := client.GetFailedStepLog(context.Background(), repo, "{pipe-1}")
	if err != nil {
		t.Fatal(err)
	}
	if logOutput != "boom: test failed" {
		t.Fatalf("logOutput = %q", logOutput)
	}
}

func TestClient_GetFailedStepLog_NoFailedStepReturnsEmpty(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpipeline\x1fget\x1f--pipeline\x1f{pipe-1}\x1f--logs\x1f--failed-steps\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: envelope(t, `{"steps":[]}`)},
	})

	logOutput, err := client.GetFailedStepLog(context.Background(), repo, "{pipe-1}")
	if err != nil {
		t.Fatal(err)
	}
	if logOutput != "" {
		t.Fatalf("logOutput = %q, want empty", logOutput)
	}
}

func TestClient_RunPropagatesCLIFailure(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	key := "bitbucket\x1fpull-requests\x1fget\x1f42\x1f--workspace\x1ftest\x1f--repo\x1frepo\x1f--output\x1fjson"
	client, _ := newFakeTwgClient(t, map[string]fakeTwgResponse{
		key: {Stdout: "not authenticated with Bitbucket", ExitCode: 1},
	})

	_, err := client.GetPR(context.Background(), repo, 42)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not authenticated with Bitbucket") {
		t.Fatalf("error = %v, want CLI failure message surfaced", err)
	}
}
