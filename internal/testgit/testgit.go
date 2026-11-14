// Package testgit resolves the real system git binary for tests that spawn
// a fake CLI on PATH (see internal/pipeline/fakecli), and carries the
// re-entry guard every such fake must run before it forwards to that binary.
//
// It exists because exec.LookPath("git") returns the PATH winner, not
// necessarily real git: if a passthrough git wrapper (guard shim, audit
// wrapper, etc.) sits on the developer's PATH, that wrapper gets mistaken for
// "real git" and, once the fake CLI directory is prepended ahead of it, the
// two forward into each other without bound
// (github.com/ironerumi/no-mistakes#5).
//
// An absolute path is not enough on macOS. /usr/bin/git there is not git: it
// is Apple's xcode-select shim, which hands off to `xcrun git`. Under
// nix-shell that xcrun is nix's, and it finds `git` by searching PATH, so a
// fake git placed first on PATH is launched again by the "real git" it
// forwards to, and the pair loop until the process table is full. RealGit
// therefore never returns the shim, and Enter bounds any chain that gets
// through anyway.
package testgit

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// RealGit resolves the system git binary by absolute path only, never
// consulting PATH: a wrapper (guard shim, audit wrapper, etc.) or the fake
// CLI itself can win PATH, and either one being mistaken for "real git" is
// what let fakecli and a wrapper forward into each other without bound
// (github.com/ironerumi/no-mistakes#5). Restricting resolution to a fixed
// list of well-known install locations means PATH is never consulted, so
// neither a wrapper nor the fake CLI can ever be selected.
//
// On macOS a candidate is also refused when it is, or resolves to, a binary
// in /usr/bin: that directory holds only the xcrun shim, whose resolution of
// the real git depends on PATH and on DEVELOPER_DIR (see the package comment).
func RealGit() (string, error) {
	locations := gitLocations()
	for _, p := range locations {
		if usableGit(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("testgit: real git not found in standard locations (%s)", strings.Join(locations, ", "))
}

func usableGit(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS != "windows" && fi.Mode()&0111 == 0 {
		return false
	}
	return !isXcodeShim(runtime.GOOS, p)
}

// isXcodeShim reports whether path, after following symlinks, is a binary in
// /usr/bin on macOS - where every entry is an xcode-select shim rather than
// the tool it names. It is false on every other OS, where /usr/bin/git is git.
func isXcodeShim(goos, gitPath string) bool {
	if goos != "darwin" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(gitPath)
	if err != nil {
		resolved = gitPath
	}
	return path.Dir(filepath.ToSlash(filepath.Clean(resolved))) == "/usr/bin"
}

func gitLocations() []string {
	switch runtime.GOOS {
	case "windows":
		return windowsGitLocations()
	case "darwin":
		return darwinGitLocations(developerDir())
	}
	return []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"}
}

// darwinGitLocations lists installed gits that are real binaries: Apple's own
// (Command Line Tools, Xcode, and the active developer directory) first, then
// the common package managers. /usr/bin/git is deliberately absent.
func darwinGitLocations(activeDeveloperDir string) []string {
	locations := []string{
		"/Library/Developer/CommandLineTools/usr/bin/git",
		"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
	}
	if activeDeveloperDir != "" {
		locations = append(locations, filepath.Join(activeDeveloperDir, "usr", "bin", "git"))
	}
	return append(locations,
		"/opt/homebrew/bin/git",
		"/usr/local/bin/git",
		"/run/current-system/sw/bin/git",
		"/nix/var/nix/profiles/default/bin/git",
	)
}

// developerDir is the developer directory xcode-select has selected, for an
// Xcode installed somewhere other than the default path. It runs the absolute
// /usr/bin/xcode-select with DEVELOPER_DIR removed (a nix-shell sets it to an
// SDK that has no git) and never consults PATH.
func developerDir() string {
	cmd := exec.Command("/usr/bin/xcode-select", "-p")
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DEVELOPER_DIR=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}

func windowsGitLocations() []string {
	locations := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	appendRoot := func(root string) {
		if root == "" || !filepath.IsAbs(root) {
			return
		}
		for _, p := range []string{
			filepath.Join(root, "Git", "cmd", "git.exe"),
			filepath.Join(root, "Git", "bin", "git.exe"),
		} {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			locations = append(locations, p)
		}
	}

	appendRoot(os.Getenv("ProgramFiles"))
	appendRoot(`C:\Program Files`)
	appendRoot(os.Getenv("ProgramFiles(x86)"))
	appendRoot(`C:\Program Files (x86)`)
	return locations
}

// DepthEnv carries how many fake CLIs deep the current process is. Each fake
// CLI that runs raises it by one for everything it launches, so a fake git
// that is re-launched by the "real git" it forwards to counts up on every lap.
const DepthEnv = "NM_TEST_FAKE_CLI_DEPTH"

// MaxDepth is the deepest fake-CLI chain allowed. Legitimate chains are
// short - a fake git forwarding to real git, which may run a hook that calls
// git again - so a loop is caught after a handful of processes instead of
// when the process table fills.
const MaxDepth = 5

// Enter is the re-entry guard. Every fake CLI that forwards to another
// process calls it before doing anything else and exits non-zero on an error:
// it refuses to start once DepthEnv has reached MaxDepth, and otherwise raises
// DepthEnv by one so the processes this one launches count a level deeper. An
// unparseable or negative DepthEnv is refused too, so a corrupted counter can
// never switch the guard off.
func Enter() error {
	depth, err := parseDepth(os.Getenv(DepthEnv))
	if err != nil {
		return err
	}
	if depth >= MaxDepth {
		return depthError(depth)
	}
	return os.Setenv(DepthEnv, strconv.Itoa(depth+1))
}

func parseDepth(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 0 {
		return 0, fmt.Errorf("%s=%q is not a non-negative integer", DepthEnv, raw)
	}
	return depth, nil
}

func depthError(depth int) error {
	return fmt.Errorf("fake CLI re-entry limit reached (%s=%d, max %d): a fake CLI on PATH is being launched again by the program it forwards to; "+
		"forward to testgit.RealGit(), which never returns a PATH-resolved or macOS xcrun shim git", DepthEnv, depth, MaxDepth)
}

// ShellGuard is the POSIX-shell form of Enter, for a fake git written as a
// shell script. Put it first in the script, before the line that execs the
// real git.
const ShellGuard = `nm_depth=${` + DepthEnv + `:-0}
case "$nm_depth" in
  ''|*[!0-9]*) echo "fake git: ` + DepthEnv + `=$nm_depth is not a non-negative integer" >&2; exit 125 ;;
esac
if [ "$nm_depth" -ge ` + maxDepthText + ` ]; then
  echo "fake git: re-entry limit reached (` + DepthEnv + `=$nm_depth, max ` + maxDepthText + `); it is being launched again by the git it forwards to - forward to testgit.RealGit()" >&2
  exit 125
fi
` + DepthEnv + `=$((nm_depth + 1)); export ` + DepthEnv + `
`

// GoSourceGuard is the Go form of Enter, for a fake git compiled from a source
// string. It needs "fmt", "os" and "strconv" imported, and its nmFakeCLIEnter
// must be the first call in main.
const GoSourceGuard = `
func nmFakeCLIEnter() {
	depth := 0
	if raw := os.Getenv("` + DepthEnv + `"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "fake git: ` + DepthEnv + `=%q is not a non-negative integer\n", raw)
			os.Exit(125)
		}
		depth = n
	}
	if depth >= ` + maxDepthText + ` {
		fmt.Fprintf(os.Stderr, "fake git: re-entry limit reached (` + DepthEnv + `=%d, max ` + maxDepthText + `); it is being launched again by the git it forwards to - forward to testgit.RealGit()\n", depth)
		os.Exit(125)
	}
	os.Setenv("` + DepthEnv + `", strconv.Itoa(depth+1))
}
`

const maxDepthText = "5"

// ExitCode is the status a fake CLI exits with when the guard refuses it, so a
// caller can tell a refused re-entry from any ordinary git failure.
const ExitCode = 125
