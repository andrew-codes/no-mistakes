package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

// buildFakeGit compiles fakecli without the race detector and installs it as
// `git`, the way the pipeline-step tests do.
func buildFakeGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", path, "github.com/kunchenguid/no-mistakes/internal/pipeline/fakecli")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fakecli: %v\n%s", err, out)
	}
	return path
}

type reentryRun struct {
	stderr string
	code   int
	log    []string
}

// runFakeGit runs the fake git once, bounded: a hard timeout, and on unix its
// whole process group is killed with it (see boundProcessGroup), so a guard
// regression cannot leave a runaway chain behind.
func runFakeGit(t *testing.T, fake string, env []string, args ...string) reentryRun {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "invocations.log")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fake, args...)
	cmd.Env = append(os.Environ(), "FAKE_CLI_MODE=git-passthrough", "FAKE_CLI_LOG="+logFile)
	cmd.Env = append(cmd.Env, env...)
	boundProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := reentryRun{stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		result.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("run fake git: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("fake git did not stop within its bound: the re-entry guard failed")
	}
	if data, err := os.ReadFile(logFile); err == nil {
		result.log = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return result
}

// TestFakeGitReentryStopsInAHandfulOfProcesses reproduces, with a bounded
// number of processes, the loop that forked ~6,000 processes under macOS
// nix-shell: a fake git first on PATH forwards to a "real git" that is really
// a shim re-resolving `git` through PATH, which finds the fake git again.
// Without the guard every lap forks two more processes until the process table
// is full; with it the chain ends after testgit.MaxDepth fake invocations.
func TestFakeGitReentryStopsInAHandfulOfProcesses(t *testing.T) {
	fake := buildFakeGit(t)

	if !t.Run("refuses to start at the limit", func(t *testing.T) {
		got := runFakeGit(t, fake, []string{testgit.DepthEnv + "=" + strconv.Itoa(testgit.MaxDepth)}, "status")
		if got.code != testgit.ExitCode {
			t.Errorf("exit = %d, want %d; stderr %q", got.code, testgit.ExitCode, got.stderr)
		}
		if !strings.Contains(got.stderr, "re-entry limit") {
			t.Errorf("stderr %q does not name the re-entry limit", got.stderr)
		}
		if len(got.log) != 0 {
			t.Errorf("a refused fake git still ran a handler: %q", got.log)
		}
	}) {
		t.Fatal("the guard does not refuse at the limit; not running the chained cases, which would loop")
	}

	// A fake git pointed straight back at itself: the simplest possible loop.
	if !t.Run("a direct self-forward is bounded", func(t *testing.T) {
		got := runFakeGit(t, fake, []string{"FAKE_CLI_REAL_GIT=" + fake}, "status")
		if got.code != testgit.ExitCode {
			t.Errorf("exit = %d, want %d (the refusal propagates up the chain); stderr %q", got.code, testgit.ExitCode, got.stderr)
		}
		if len(got.log) != testgit.MaxDepth {
			t.Errorf("fake git ran %d times, want exactly %d before the guard stopped it", len(got.log), testgit.MaxDepth)
		}
	}) {
		t.Fatal("the counter does not propagate through a direct forward; not running the shim case, which would loop")
	}

	// The shape of the incident: real git is a shim that execs `git` from PATH.
	t.Run("a PATH-resolving shim in the middle is bounded", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the xcrun-shaped shim is a POSIX shell script")
		}
		shim := filepath.Join(t.TempDir(), "xcrun-git-shim")
		if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec git \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Dir(fake) + string(os.PathListSeparator) + os.Getenv("PATH")
		got := runFakeGit(t, fake, []string{"FAKE_CLI_REAL_GIT=" + shim, "PATH=" + path}, "status")
		if got.code != testgit.ExitCode {
			t.Errorf("exit = %d, want %d; stderr %q", got.code, testgit.ExitCode, got.stderr)
		}
		if len(got.log) != testgit.MaxDepth {
			t.Errorf("fake git ran %d times, want exactly %d before the guard stopped it", len(got.log), testgit.MaxDepth)
		}
	})
}
