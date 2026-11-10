package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// forkReleaseRepo is a clone with a bare origin, the shape the release
// workflow's checkout has: origin/main exists and tags are local.
type forkReleaseRepo struct {
	dir     string
	mainSHA string
}

func newForkReleaseRepo(t *testing.T, tags ...string) forkReleaseRepo {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(filteredEnv(os.Environ(), "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT"),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "--bare", "-b", "main", origin)
	run(root, "init", "-b", "main", work)
	run(work, "remote", "add", "origin", origin)
	run(work, "commit", "--allow-empty", "-m", "first")
	for _, tag := range tags {
		run(work, "tag", tag)
	}
	run(work, "commit", "--allow-empty", "-m", "second")
	run(work, "push", "origin", "main")
	run(work, "fetch", "origin")
	return forkReleaseRepo{dir: work, mainSHA: run(work, "rev-parse", "HEAD")}
}

// firstCommit is the commit the fixture tags point at, one behind main.
func (r forkReleaseRepo) firstCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", r.dir, "rev-parse", "HEAD~1").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// fakeGH answers the two `gh release list` queries the script makes from the
// FAKE_PUBLISHED and FAKE_DRAFT variables. The real --jq expressions are not
// evaluated here.
func fakeForkReleaseGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case " $* " in
*" --exclude-drafts "*) printf '%s' "$FAKE_PUBLISHED" ;;
*) printf '%s' "$FAKE_DRAFT" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

type forkVersionResult struct {
	output  string
	outputs map[string]string
	err     error
}

func runForkReleaseVersion(t *testing.T, repo forkReleaseRepo, env map[string]string) forkVersionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ghOutput := filepath.Join(t.TempDir(), "github_output")
	script, err := filepath.Abs("scripts/fork-release-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Dir = repo.dir
	cmd.Env = append(filteredEnv(os.Environ(), "PATH", "GITHUB_OUTPUT"),
		"PATH="+fakeForkReleaseGH(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_OUTPUT="+ghOutput,
		"GH_REPO=andrew-codes/no-mistakes",
		"DEFAULT_BRANCH=main",
		"GIT_CONFIG_GLOBAL="+os.DevNull)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()
	outputs := map[string]string{}
	if data, err := os.ReadFile(ghOutput); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				outputs[k] = v
			}
		}
	}
	return forkVersionResult{output: string(out), outputs: outputs, err: runErr}
}

func skipForkReleaseScriptTestsOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fork-release-version.sh runs on the ubuntu release runner")
	}
}

func TestForkReleaseVersionPublishedReleaseResolvesTag(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	repo := newForkReleaseRepo(t)
	res := runForkReleaseVersion(t, repo, map[string]string{
		"EVENT_NAME":       "release",
		"RELEASE_TAG_NAME": "v1.0.0",
		"GITHUB_SHA":       repo.mainSHA,
		"FAKE_PUBLISHED":   "channels\nv1.0.0\n",
	})
	if res.err != nil {
		t.Fatalf("script failed: %v\n%s", res.err, res.output)
	}
	want := map[string]string{"version": "1.0.0", "tag": "v1.0.0", "should_publish": "true"}
	for k, v := range want {
		if res.outputs[k] != v {
			t.Fatalf("output %s = %q, want %q\n%s", k, res.outputs[k], v, res.output)
		}
	}
}

func TestForkReleaseVersionOrdersNumericallyAndIgnoresNonVersionTags(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	repo := newForkReleaseRepo(t, "v1.9.0", "upstream-synced")
	res := runForkReleaseVersion(t, repo, map[string]string{
		"EVENT_NAME":       "release",
		"RELEASE_TAG_NAME": "v1.10.0",
		"GITHUB_SHA":       repo.mainSHA,
		// "channels" is a release but not a version; v1.9.0 sorts below v1.10.0.
		"FAKE_PUBLISHED": "v1.10.0\nchannels\nv1.9.0\n",
	})
	if res.err != nil {
		t.Fatalf("v1.10.0 should be accepted over v1.9.0: %v\n%s", res.err, res.output)
	}
}

func TestForkReleaseVersionRejectsInvalidAndReusedVersions(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	tests := []struct {
		name      string
		tag       string
		published string
		tags      []string
		wantErr   string
	}{
		{"prerelease suffix", "v1.0.0-rc.1", "v1.0.0-rc.1\n", nil, "not plain semver"},
		{"fork style suffix", "v1.111.0-andrew-codes.1", "", nil, "not plain semver"},
		{"missing v", "1.0.0", "", nil, "not plain semver"},
		{"leading zero", "v01.0.0", "", nil, "not plain semver"},
		{"two segments", "v1.0", "", nil, "not plain semver"},
		{"unresolved drafter template", "v$RESOLVED_VERSION", "", nil, "not plain semver"},
		{"lower than a published release", "v1.0.5", "v1.1.0\nv1.0.5\n", nil, "not greater than"},
		{"the release own tag and entry are not compared with themselves", "v1.2.0", "v1.2.0\n", []string{"v1.2.0"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newForkReleaseRepo(t, tt.tags...)
			res := runForkReleaseVersion(t, repo, map[string]string{
				"EVENT_NAME":       "release",
				"RELEASE_TAG_NAME": tt.tag,
				"GITHUB_SHA":       repo.mainSHA,
				"FAKE_PUBLISHED":   tt.published,
			})
			if tt.wantErr == "" {
				// The release's own tag is never compared with itself.
				if res.err != nil {
					t.Fatalf("expected success, got %v\n%s", res.err, res.output)
				}
				return
			}
			if res.err == nil {
				t.Fatalf("expected failure for %q\n%s", tt.tag, res.output)
			}
			if !strings.Contains(res.output, tt.wantErr) {
				t.Fatalf("output should mention %q:\n%s", tt.wantErr, res.output)
			}
			if len(res.outputs) != 0 {
				t.Fatalf("a rejected version must not write outputs, got %v", res.outputs)
			}
		})
	}
}

func TestForkReleaseVersionIgnoresGitTagsThatAreNotReleases(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	// Upstream's version line is unrelated to the fork's, so upstream tags that
	// were pushed into the fork must not block a release.
	repo := newForkReleaseRepo(t, "v1.111.0", "v1.105.0")
	res := runForkReleaseVersion(t, repo, map[string]string{
		"EVENT_NAME":       "release",
		"RELEASE_TAG_NAME": "v1.0.0",
		"GITHUB_SHA":       repo.mainSHA,
		"FAKE_PUBLISHED":   "v1.0.0\n",
	})
	if res.err != nil {
		t.Fatalf("upstream-style tags must not block a fork release: %v\n%s", res.err, res.output)
	}
}

func TestForkReleaseVersionRequiresTheTagAtTheDefaultBranchTip(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	// The tag already existed at an older commit, so publishing the draft would
	// release that old tree under a new number.
	repo := newForkReleaseRepo(t, "v1.0.1")
	res := runForkReleaseVersion(t, repo, map[string]string{
		"EVENT_NAME":       "release",
		"RELEASE_TAG_NAME": "v1.0.1",
		"GITHUB_SHA":       repo.firstCommit(t),
		"FAKE_PUBLISHED":   "v1.0.1\nv1.0.0\n",
	})
	if res.err == nil {
		t.Fatalf("a tag behind main must be rejected\n%s", res.output)
	}
	if !strings.Contains(res.output, "not the tip of main") {
		t.Fatalf("unexpected error:\n%s", res.output)
	}
}

func TestForkReleaseVersionRehearsalNeverShips(t *testing.T) {
	skipForkReleaseScriptTestsOnWindows(t)
	t.Run("uses the draft tag", func(t *testing.T) {
		repo := newForkReleaseRepo(t)
		res := runForkReleaseVersion(t, repo, map[string]string{
			"EVENT_NAME":     "workflow_dispatch",
			"GITHUB_SHA":     repo.firstCommit(t), // a rehearsal may run from any commit
			"FAKE_DRAFT":     "v1.2.0",
			"FAKE_PUBLISHED": "v1.1.0\n",
		})
		if res.err != nil {
			t.Fatalf("script failed: %v\n%s", res.err, res.output)
		}
		if res.outputs["tag"] != "v1.2.0" || res.outputs["should_publish"] != "false" {
			t.Fatalf("outputs = %v\n%s", res.outputs, res.output)
		}
	})
	t.Run("placeholder without a draft", func(t *testing.T) {
		repo := newForkReleaseRepo(t)
		res := runForkReleaseVersion(t, repo, map[string]string{
			"EVENT_NAME":     "workflow_dispatch",
			"FAKE_PUBLISHED": "v1.1.0\n",
		})
		if res.err != nil {
			t.Fatalf("script failed: %v\n%s", res.err, res.output)
		}
		if res.outputs["tag"] != "v0.0.0" || res.outputs["should_publish"] != "false" {
			t.Fatalf("outputs = %v\n%s", res.outputs, res.output)
		}
	})
	t.Run("rejects a stale draft version", func(t *testing.T) {
		repo := newForkReleaseRepo(t)
		res := runForkReleaseVersion(t, repo, map[string]string{
			"EVENT_NAME":     "workflow_dispatch",
			"FAKE_DRAFT":     "v1.0.0",
			"FAKE_PUBLISHED": "v1.1.0\n",
		})
		if res.err == nil || !strings.Contains(res.output, "not greater than") {
			t.Fatalf("a draft that is not ahead of the latest release must fail the rehearsal:\n%s", res.output)
		}
	})
}
