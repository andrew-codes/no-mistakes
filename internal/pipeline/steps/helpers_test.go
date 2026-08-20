package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type mockAgent struct {
	name  string
	runFn func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	calls []agent.RunOpts
}

func (m *mockAgent) Name() string { return m.name }

func (m *mockAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	m.calls = append(m.calls, opts)
	if m.runFn != nil {
		return m.runFn(ctx, opts)
	}
	return &agent.Result{}, nil
}

func (m *mockAgent) Close() error { return nil }

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "status", "--porcelain")
}

func lastCommitMessage(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "log", "-1", "--pretty=%s")
}

// gitRepoTemplate holds a cached template repo that setupGitRepo copies from
// instead of running git init + config + commits each time.
var gitRepoTemplate struct {
	once    sync.Once
	dir     string
	baseSHA string
	headSHA string
}

func ensureGitRepoTemplate(t *testing.T) {
	t.Helper()
	gitRepoTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "git-template-*")
		if err != nil {
			t.Fatal(err)
		}

		run := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test",
				"GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test",
				"GIT_COMMITTER_EMAIL=test@test.com",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				panic(fmt.Sprintf("git %v: %v: %s", args, err, out))
			}
			return strings.TrimSpace(string(out))
		}

		run("init")
		run("config", "user.name", "test")
		run("config", "user.email", "test@test.com")
		run("checkout", "-b", "main")

		os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base content"), 0o644)
		run("add", "-A")
		run("commit", "-m", "base commit")
		gitRepoTemplate.baseSHA = run("rev-parse", "HEAD")

		run("checkout", "-b", "feature")
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\n"), 0o644)
		run("add", "-A")
		run("commit", "-m", "add feature")
		gitRepoTemplate.headSHA = run("rev-parse", "HEAD")

		gitRepoTemplate.dir = dir
	})
}

// setupGitRepo creates a git repo with a base commit on main and a head commit on feature.
// Returns (repoDir, baseSHA, headSHA).
// Uses a cached template repo and copies it via cp -a for speed.
func setupGitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	ensureGitRepoTemplate(t)

	dir := t.TempDir()
	if err := copyDirContents(gitRepoTemplate.dir, dir); err != nil {
		t.Fatalf("copy template repo: %v", err)
	}

	return dir, gitRepoTemplate.baseSHA, gitRepoTemplate.headSHA
}

// newTestContext creates a StepContext for testing with optional config overrides.
func newTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return &pipeline.StepContext{
		Ctx:  context.Background(),
		Run:  &db.Run{ID: "run-1", RepoID: "repo-1", Branch: "refs/heads/feature", HeadSHA: headSHA, BaseSHA: baseSHA},
		Repo: &db.Repo{ID: "repo-1", WorkingPath: workDir, UpstreamURL: "https://github.com/test/repo", DefaultBranch: "main"},
		// The executor resolves this from the app root in production. Tests get
		// a per-test directory so a step under test can never write evidence
		// into a shared location the next test would then observe.
		EvidenceDir: filepath.Join(t.TempDir(), "evidence", "run-1"),
		WorkDir:     workDir,
		Agent:       ag,
		Config:      &config.Config{Agent: types.AgentClaude, Commands: cmds},
		DB:          database,
		Log:         func(s string) {},
		LogChunk:    func(s string) {},
		LogFile:     func(s string) {},
	}
}

// fakeCLIEnv builds environment variable entries for a fake CLI binary and PATH override.
// Returns env entries that should be set on StepContext.Env for parallel-safe tests.
func fakeCLIEnv(binDir string, vars map[string]string) []string {
	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

// fakeCLIBinDir creates a temporary directory for fake CLI binaries.
// Unlike t.TempDir(), cleanup tolerates file locks from recently-executed
// binaries on Windows (which prevent immediate deletion).
func fakeCLIBinDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fakecli")
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

// linkTestBinary creates a hard link (or copy) of the current test binary
// with the given name in binDir. On Windows, .exe is appended.
func linkTestBinary(t *testing.T, binDir, name string) {
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
		// Fallback to copy if hard link fails (cross-device, etc.)
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeGH creates a mock gh binary in a temp dir and returns env entries for StepContext.Env.
// The binary records all invocations to a log file and responds based on subcommand.
func fakeGH(t *testing.T, prViewURL string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "gh",
		"FAKE_CLI_LOG":    logFile,
		"FAKE_CLI_PR_URL": prViewURL,
	})
	return env, logFile
}

// fakeTwgResponse describes what the fake twg binary should emit for one
// matched invocation, keyed by its exact argv (see fakeTwg).
type fakeTwgResponse struct {
	Stdout   string
	ExitCode int
}

// fakeGHWithBase behaves like fakeGH but additionally records the existing
// PR's actual base branch, so the fake `gh pr list --base X` only returns the
// PR when X matches it - mirroring GitHub's server-side base filtering.
func fakeGHWithBase(t *testing.T, prViewURL, prBase string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":    "gh",
		"FAKE_CLI_LOG":     logFile,
		"FAKE_CLI_PR_URL":  prViewURL,
		"FAKE_CLI_PR_BASE": prBase,
	})
	return env, logFile
}

// twgEnvelope wraps data in twg's standard `--output json` response shape:
// {"apiVersion":"v2","command":"...","data":<payload>}.
func twgEnvelope(data string) string {
	return `{"apiVersion":"v2","command":"test","data":` + data + `}`
}

// twgMaskedFlags are flags whose value is agent-generated (PR title/body) and
// therefore unpredictable at response-table build time; twgArgsKey replaces
// their value with a placeholder so a canned response matches regardless of
// the exact generated content. The real content still reaches the fake twg
// binary's invocation log unmasked, so log-content assertions stay meaningful.
var twgMaskedFlags = map[string]bool{"--title": true, "--description": true}

// twgArgsKey joins argv the same way the fake twg dispatcher (steps_test.go
// fakeTwgHandler) computes its lookup key, masking agent-generated flag
// values so a canned response can match regardless of exact PR content.
func twgArgsKey(args ...string) string {
	masked := make([]string, len(args))
	copy(masked, args)
	for i := 0; i < len(masked)-1; i++ {
		if twgMaskedFlags[masked[i]] {
			masked[i+1] = "<value>"
		}
	}
	return strings.Join(masked, "\x1f")
}

// twgDoctorOK is the response every twg-backed Bitbucket step needs for its
// Available() precondition check.
func twgDoctorOK() (string, fakeTwgResponse) {
	return twgArgsKey("doctor", "--output", "json"), fakeTwgResponse{Stdout: twgEnvelope(`{"bitbucket":{"ok":true}}`)}
}

// fakeTwg creates a mock twg binary in a temp dir and returns env entries for
// StepContext.Env plus the invocation log file path. responses maps a
// twgArgsKey-joined argv to the canned reply for that exact invocation.
func fakeTwg(t *testing.T, responses map[string]fakeTwgResponse) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "twg.log")
	linkTestBinary(t, binDir, "twg")

	respFile := filepath.Join(t.TempDir(), "twg-responses.json")
	data, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(respFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":      "twg",
		"FAKE_CLI_LOG":       logFile,
		"FAKE_TWG_RESPONSES": respFile,
	})
	return env, logFile
}

// setProcessEnvFromFakeCLIEnv publishes every entry from a fakeCLIEnv-built
// env slice (see fakeGH/fakeTwg) onto the real test process's environment via
// t.Setenv, so a step running with sctx.Env == nil - which makes stepCmd fall
// back to ambient PATH/env resolution instead of the StepContext-scoped
// override - still finds and correctly drives the fake CLI binary.
func setProcessEnvFromFakeCLIEnv(t *testing.T, env []string) {
	t.Helper()
	// fakeCLIEnv already builds PATH as "<binDir><sep><original PATH>", so each
	// entry (including PATH) can be published as-is.
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		t.Setenv(key, value)
	}
}

func countLogLines(t *testing.T, logFile, substr string) int {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

func fakeGlab(t *testing.T, mrViewJSON string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "glab.log")
	linkTestBinary(t, binDir, "glab")
	env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "glab",
		"FAKE_CLI_LOG":          logFile,
		"FAKE_CLI_MR_VIEW_JSON": mrViewJSON,
	})
	return env, logFile
}

// newTestContextWithDBRecords is like newTestContext but also inserts
// repo and run records into the database so GetRun works after updates.
func recordReviewApproval(t *testing.T, sctx *pipeline.StepContext, headSHA string) {
	t.Helper()
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	approved := headSHA
	sctx.Run.ReviewApprovedHeadSHA = &approved
}

func newTestContextWithDBRecords(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, workDir, baseSHA, headSHA, cmds)

	// Insert repo + run records so DB queries work
	repo, err := sctx.DB.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	sctx.Repo = repo
	return sctx
}

// fakeCIGH creates a fake gh binary that responds to CI-related
// commands (pr view --json state, pr checks --json, pr view --json comments).
func fakeCIGH(t *testing.T, state, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "ci-gh",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
	})
}

func fakeCIGHMergeable(t *testing.T, state, checksJSON, mergeable string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":      "ci-gh",
		"FAKE_CLI_STATE":     state,
		"FAKE_CLI_CHECKS":    checksJSON,
		"FAKE_CLI_MERGEABLE": mergeable,
	})
}

func fakeCIGHMergeableError(t *testing.T, state, checksJSON, mergeableErr string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":          "ci-gh",
		"FAKE_CLI_STATE":         state,
		"FAKE_CLI_CHECKS":        checksJSON,
		"FAKE_CLI_MERGEABLE_ERR": mergeableErr,
	})
}

func fakeCIGHStateError(t *testing.T, stateErr, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":      "ci-gh",
		"FAKE_CLI_STATE_ERR": stateErr,
		"FAKE_CLI_CHECKS":    checksJSON,
	})
}

func fakeCIGHChecksError(t *testing.T, state, mergeable, checksErr string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":       "ci-gh",
		"FAKE_CLI_STATE":      state,
		"FAKE_CLI_MERGEABLE":  mergeable,
		"FAKE_CLI_CHECKS_ERR": checksErr,
	})
}

func fakeCIGHSequenceMergeable(t *testing.T, state string, checks []string, mergeable string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
		"FAKE_CLI_MERGEABLE":         mergeable,
	})
}

func fakeCIGHSequence(t *testing.T, state string, checks []string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
	})
}

// fakeCIGHLoggedSequence is fakeCIGHSequence with a recorded argv log, so tests
// can assert which gh commands the CI monitor issued (for example whether it
// asked for a check rerun). mergeable overrides the reported mergeable state
// ("" reports MERGEABLE); rerunErr, when set, makes `gh run rerun` fail.
func fakeCIGHLoggedSequence(t *testing.T, state string, checks []string, mergeable, rerunErr string) (env []string, logFile string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")

	tempDir := t.TempDir()
	checksPath := filepath.Join(tempDir, "checks.txt")
	indexPath := filepath.Join(tempDir, "checks-index.txt")
	logFile = filepath.Join(tempDir, "gh.log")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-gh-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
		"FAKE_CLI_MERGEABLE":         mergeable,
		"FAKE_CLI_LOG":               logFile,
		"FAKE_CLI_RERUN_ERR":         rerunErr,
	}), logFile
}

func fakeCIGHNoChecks(t *testing.T) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE": "ci-gh-nochecks",
	})
}

// fakeCIGlab creates a fake glab binary that serves the CI monitoring endpoints.
// state is the MR state ("opened", "merged", "closed"); checksJSON is a JSON
// array of jobs for `glab ci status` / `glab ci get`.
func fakeCIGlab(t *testing.T, state, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "ci-glab",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
	})
}

func fakeCIGlabConflict(t *testing.T, state, checksJSON string, conflict bool) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	conflicts := "false"
	if conflict {
		conflicts = "true"
	}
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "ci-glab",
		"FAKE_CLI_STATE":        state,
		"FAKE_CLI_CHECKS":       checksJSON,
		"FAKE_CLI_MR_CONFLICTS": conflicts,
	})
}

func fakeCIGlabWithTrace(t *testing.T, state, checksJSON, trace string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "ci-glab",
		"FAKE_CLI_STATE":  state,
		"FAKE_CLI_CHECKS": checksJSON,
		"FAKE_CLI_TRACE":  trace,
	})
}

func fakeCIGlabSequence(t *testing.T, state string, checks []string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")

	checksPath := filepath.Join(t.TempDir(), "checks.txt")
	indexPath := filepath.Join(t.TempDir(), "checks-index.txt")

	if err := os.WriteFile(checksPath, []byte(strings.Join(checks, "\n")), 0o644); err != nil {
		t.Fatalf("write checks sequence: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("write checks index: %v", err)
	}

	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":              "ci-glab-seq",
		"FAKE_CLI_STATE":             state,
		"FAKE_CLI_CHECKS_PATH":       checksPath,
		"FAKE_CLI_CHECKS_INDEX_PATH": indexPath,
	})
}
