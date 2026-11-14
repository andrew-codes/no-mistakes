package testgit

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestRealGit_IgnoresWrapperOnPATH is a resolution-only assertion: it never
// runs the resolved binary, let alone through the fake CLI forwarding chain.
// Doing so live would recurse the test runner itself - the exact failure
// this package exists to prevent (issue #5).
func TestRealGit_IgnoresWrapperOnPATH(t *testing.T) {
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := RealGit()
	if err != nil {
		t.Skipf("no real git installed in a standard location: %v", err)
	}
	if got == wrapper {
		t.Fatalf("RealGit() returned the PATH-shadowing wrapper %q, want an absolute real-git path", got)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("RealGit() = %q, want an absolute path", got)
	}
	known := false
	for _, p := range gitLocations() {
		known = known || p == got
	}
	if !known {
		t.Fatalf("RealGit() = %q, not one of the fixed locations %v", got, gitLocations())
	}
}

// TestRealGit_NeverReturnsTheXcodeShim pins the macOS half of issue #5: an
// absolute /usr/bin/git is still the xcrun shim, which re-resolves git through
// PATH under nix-shell, so a fake git on PATH is launched again by the "real
// git" it forwards to. Resolution only; the shim is never run.
func TestRealGit_NeverReturnsTheXcodeShim(t *testing.T) {
	if !isXcodeShim("darwin", "/usr/bin/git") {
		t.Fatal("/usr/bin/git must be treated as the xcrun shim on darwin")
	}
	if isXcodeShim("linux", "/usr/bin/git") {
		t.Fatal("/usr/bin/git is real git off macOS and must stay usable")
	}
	if isXcodeShim("darwin", "/Library/Developer/CommandLineTools/usr/bin/git") {
		t.Fatal("the Command Line Tools git is the real binary, not the shim")
	}

	for _, active := range []string{"", "/Applications/Xcode-15.app/Contents/Developer"} {
		for _, p := range darwinGitLocations(active) {
			if path.Dir(filepath.ToSlash(p)) == "/usr/bin" {
				t.Errorf("darwinGitLocations(%q) lists %q, a macOS shim", active, p)
			}
		}
	}

	if runtime.GOOS != "darwin" {
		return
	}
	got, err := RealGit()
	if err != nil {
		t.Skipf("no real git installed in a standard location: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(got); err != nil || filepath.Dir(resolved) == "/usr/bin" {
		t.Fatalf("RealGit() = %q (resolves to %q, err %v), want a non-shim git", got, resolved, err)
	}

	// A symlink to the shim is the shim, wherever the link lives.
	link := filepath.Join(t.TempDir(), "git")
	if err := os.Symlink("/usr/bin/git", link); err != nil {
		t.Fatal(err)
	}
	if usableGit(link) {
		t.Fatalf("usableGit(%q) accepted a symlink to the shim", link)
	}
}

func TestEnter_CountsUpAndRefusesAtTheLimit(t *testing.T) {
	t.Setenv(DepthEnv, "")
	for lap := 0; lap < MaxDepth; lap++ {
		if err := Enter(); err != nil {
			t.Fatalf("lap %d: Enter() = %v, want nil below the limit", lap, err)
		}
		if got, want := os.Getenv(DepthEnv), strconv.Itoa(lap+1); got != want {
			t.Fatalf("lap %d: %s = %q, want %q", lap, DepthEnv, got, want)
		}
	}
	err := Enter()
	if err == nil {
		t.Fatalf("Enter() at %s=%d returned nil, want the re-entry refusal", DepthEnv, MaxDepth)
	}
	if !strings.Contains(err.Error(), "re-entry limit") {
		t.Fatalf("refusal %q does not name the re-entry limit", err)
	}
	if got := os.Getenv(DepthEnv); got != strconv.Itoa(MaxDepth) {
		t.Fatalf("a refused Enter() changed %s to %q", DepthEnv, got)
	}
}

func TestEnter_FailsClosedOnACorruptCounter(t *testing.T) {
	for _, raw := range []string{"abc", "-1", "1.5", " 2", "0x3"} {
		t.Setenv(DepthEnv, raw)
		if err := Enter(); err == nil {
			t.Errorf("Enter() with %s=%q returned nil, want a refusal", DepthEnv, raw)
		}
		if got := os.Getenv(DepthEnv); got != raw {
			t.Errorf("a refused Enter() rewrote %s from %q to %q", DepthEnv, raw, got)
		}
	}
}

func TestGuardsAgreeOnTheLimit(t *testing.T) {
	if maxDepthText != strconv.Itoa(MaxDepth) {
		t.Fatalf("maxDepthText = %q, MaxDepth = %d: the shell and Go guards would disagree with Enter", maxDepthText, MaxDepth)
	}
}

// TestShellGuard runs the shell form of the guard in a bounded, single shell:
// below the limit it passes and counts up, at the limit it refuses without
// reaching the line after it.
func TestShellGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell guard is for POSIX shell fake git scripts")
	}
	script := "#!/bin/sh\n" + ShellGuard + "echo reached $" + DepthEnv + "\n"
	path := filepath.Join(t.TempDir(), "fake-git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(depth string) (string, string, int) {
		cmd := exec.Command("/bin/sh", path)
		cmd.Env = append(os.Environ(), DepthEnv+"="+depth)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), stderr.String(), exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), 0
	}

	if out, _, code := run("0"); code != 0 || strings.TrimSpace(out) != "reached 1" {
		t.Fatalf("depth 0: stdout %q exit %d, want %q exit 0", out, code, "reached 1")
	}
	if out, stderr, code := run(strconv.Itoa(MaxDepth)); code != ExitCode || out != "" || !strings.Contains(stderr, "re-entry limit") {
		t.Fatalf("depth %d: stdout %q stderr %q exit %d, want a refusal with exit %d", MaxDepth, out, stderr, code, ExitCode)
	}
	if out, _, code := run("junk"); code != ExitCode || out != "" {
		t.Fatalf("corrupt depth: stdout %q exit %d, want a refusal with exit %d", out, code, ExitCode)
	}
}

// TestGoSourceGuard compiles the Go form of the guard into a tiny program and
// checks it the same way, so a fake git built from a source string (as
// internal/cli's rerun test does) cannot drift from Enter.
func TestGoSourceGuard(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	source := "package main\nimport (\"fmt\"; \"os\"; \"strconv\")\nvar _ = strconv.Itoa\nfunc main() {\n\tnmFakeCLIEnter()\n\tfmt.Println(\"reached\", os.Getenv(\"" + DepthEnv + "\"))\n}\n" + GoSourceGuard
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "guarded"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", bin, file)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build guarded program: %v\n%s", err, out)
	}
	run := func(depth string) (string, string, int) {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), DepthEnv+"="+depth)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), stderr.String(), exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), 0
	}
	if out, _, code := run("0"); code != 0 || strings.TrimSpace(out) != "reached 1" {
		t.Fatalf("depth 0: stdout %q exit %d, want %q exit 0", out, code, "reached 1")
	}
	if out, stderr, code := run(strconv.Itoa(MaxDepth)); code != ExitCode || out != "" || !strings.Contains(stderr, "re-entry limit") {
		t.Fatalf("depth %d: stdout %q stderr %q exit %d, want a refusal with exit %d", MaxDepth, out, stderr, code, ExitCode)
	}
	if out, _, code := run("junk"); code != ExitCode || out != "" {
		t.Fatalf("corrupt depth: stdout %q exit %d, want a refusal with exit %d", out, code, ExitCode)
	}
}
