package steps

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
)

var tinyPNG = []byte("\x89PNG\r\n\x1a\n-evidence-")

func writeBBEvidenceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// bitbucketEvidenceContext is a Bitbucket run with a recorded push, a run
// evidence directory, and attach_media set to attach.
func bitbucketEvidenceContext(t *testing.T, route string, attach bool, env map[string]string) (*pipeline.StepContext, *fakeBitbucketAPI, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var api *fakeBitbucketAPI
	if route == "update" {
		api = newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42")
	} else {
		api = newFakeBitbucketAPI(t, 0, "")
	}
	for k, v := range env {
		api.env[k] = v
	}
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	sctx.Config.Test.Evidence.BitbucketAttachMedia = attach
	runDir := filepath.Join(t.TempDir(), "evidence", "run-1")
	sctx.EvidenceDir = runDir
	completeCleanReview(t, sctx)
	return sctx, api, runDir
}

func logLines(sctx *pipeline.StepContext) *[]string {
	var logs []string
	log := sctx.Log
	sctx.Log = func(line string) {
		logs = append(logs, line)
		log(line)
	}
	return &logs
}

// The described route: the run's images are embedded in the PR description,
// everything else is listed without a path, and nothing outside the run's
// evidence set is uploaded.
func TestPRStep_BitbucketEmbedsEvidenceImagesInTheDescription(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"create", "update"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			sctx, api, runDir := bitbucketEvidenceContext(t, route, true, nil)
			writeBBEvidenceFile(t, filepath.Join(runDir, "shots", "checkout.png"), tinyPNG)
			writeBBEvidenceFile(t, filepath.Join(runDir, "demo.mp4"), []byte("video"))
			writeBBEvidenceFile(t, filepath.Join(runDir, "huge.png"), bytes.Repeat([]byte{1}, bitbucket.MaxEvidenceImageBytes+1))
			writeBBEvidenceFile(t, filepath.Join(runDir, "notes.txt"), []byte("not media"))
			// Neither of these is part of the run's evidence set.
			outside := filepath.Join(t.TempDir(), "outside.png")
			writeBBEvidenceFile(t, outside, tinyPNG)
			writeBBEvidenceFile(t, filepath.Join(sctx.WorkDir, "repo-image.png"), tinyPNG)
			if err := os.Symlink(outside, filepath.Join(runDir, "link.png")); err != nil {
				t.Skip("symlinks are not available:", err)
			}

			if out, err := (&PRStep{}).Execute(sctx); err != nil || out == nil || out.PRURL == "" {
				t.Fatalf("Execute() = %+v, %v", out, err)
			}
			description := api.lastDescription(t)
			uploads := api.readState(t).Uploads
			if len(uploads) != 1 || !strings.HasSuffix(uploads[0], "-checkout.png") || !strings.HasPrefix(uploads[0], "nm-") {
				t.Fatalf("uploads = %v, want only the run's checkout.png under a content-derived name", uploads)
			}
			if !strings.Contains(description, "![nm-") || !strings.Contains(description, "/downloads/"+uploads[0]) {
				t.Fatalf("description does not embed the uploaded image:\n%s", description)
			}
			for _, want := range []string{"## Evidence", "`checkout.png`", "`demo.mp4` - not attached: Bitbucket attachments through twg are limited to PNG, JPEG, GIF and WebP images", "`huge.png` - not attached: larger than 10 MiB"} {
				if !strings.Contains(description, want) {
					t.Fatalf("description missing %q:\n%s", want, description)
				}
			}
			for _, leak := range []string{runDir, filepath.Dir(runDir), sctx.WorkDir, "outside.png", "repo-image.png", "link.png", "notes.txt", "[[nm-evidence-image", "{{image"} {
				if strings.Contains(description, leak) {
					t.Fatalf("description leaks %q:\n%s", leak, description)
				}
			}
			if route == "create" && api.logCount("pull-requests create") != 1 {
				t.Fatalf("create calls = %d, want 1", api.logCount("pull-requests create"))
			}
			if got := api.logCount("--image "); got != 1 {
				t.Fatalf("writes carrying images = %d, want exactly one", got)
			}
		})
	}
}

// The same evidence is uploaded under the same name on every write, so a later
// run's update replaces the description's image and never piles up copies, and
// a changed image gets a new name.
func TestPRStep_BitbucketEvidenceUploadsAreStableAcrossWrites(t *testing.T) {
	t.Parallel()
	sctx, api, runDir := bitbucketEvidenceContext(t, "update", true, nil)
	image := filepath.Join(runDir, "a.png")
	writeBBEvidenceFile(t, image, tinyPNG)
	for i := 0; i < 2; i++ {
		if _, err := (&PRStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
	}
	uploads := api.readState(t).Uploads
	if len(uploads) != 2 || uploads[0] != uploads[1] {
		t.Fatalf("uploads = %v, want one name used for the same image both times", uploads)
	}
	writeBBEvidenceFile(t, image, append(append([]byte{}, tinyPNG...), "changed"...))
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if uploads = api.readState(t).Uploads; len(uploads) != 3 || uploads[2] == uploads[0] {
		t.Fatalf("uploads = %v, want a new name for changed content", uploads)
	}
	description := api.lastDescription(t)
	if strings.Count(description, "![nm-") != 1 {
		t.Fatalf("description must carry one embed for the one image:\n%s", description)
	}
}

// Nothing leaves the machine unless attach_media is explicitly true: with it
// off there is no upload and no Evidence section, and link_base_url alone
// lists the files as links.
func TestPRStep_BitbucketEvidenceIsOptIn(t *testing.T) {
	t.Parallel()
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		sctx, api, runDir := bitbucketEvidenceContext(t, "create", false, nil)
		writeBBEvidenceFile(t, filepath.Join(runDir, "a.png"), tinyPNG)
		if _, err := (&PRStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		if api.logCount("--image") != 0 || len(api.readState(t).Uploads) != 0 {
			t.Fatal("an image was uploaded with attach_media off")
		}
		if strings.Contains(api.lastDescription(t), "## Evidence") {
			t.Fatalf("unexpected Evidence section:\n%s", api.lastDescription(t))
		}
	})
	t.Run("links only", func(t *testing.T) {
		t.Parallel()
		sctx, api, runDir := bitbucketEvidenceContext(t, "create", false, nil)
		sctx.Config.Test.Evidence.LinkBaseURL = "https://evidence.example.test/runs"
		writeBBEvidenceFile(t, filepath.Join(runDir, "a.png"), tinyPNG)
		writeBBEvidenceFile(t, filepath.Join(runDir, "demo.mp4"), []byte("video"))
		if _, err := (&PRStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		description := api.lastDescription(t)
		if api.logCount("--image") != 0 || len(api.readState(t).Uploads) != 0 {
			t.Fatal("an image was uploaded with attach_media off")
		}
		for _, want := range []string{"## Evidence", "[a.png](https://evidence.example.test/runs/", "[demo.mp4](https://evidence.example.test/runs/"} {
			if !strings.Contains(description, want) {
				t.Fatalf("description missing %q:\n%s", want, description)
			}
		}
		if strings.Contains(description, "not attached") {
			t.Fatalf("a linked video must not be reported as not attached:\n%s", description)
		}
	})
}

// A refused upload never costs the description: the PR is still published, with
// a note where the image would have been, and the refusal is logged.
func TestPRStep_BitbucketEvidenceUploadRefusalKeepsTheDescription(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"create", "update"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			sctx, api, runDir := bitbucketEvidenceContext(t, route, true, map[string]string{"FAKE_CLI_BB_REJECT_IMAGES": "Downloads are disabled for this repository"})
			writeBBEvidenceFile(t, filepath.Join(runDir, "a.png"), tinyPNG)
			logs := logLines(sctx)
			out, err := (&PRStep{}).Execute(sctx)
			if err != nil || out == nil || out.PRURL == "" {
				t.Fatalf("a refused upload failed the PR step: %+v %v", out, err)
			}
			description := api.lastDescription(t)
			if !strings.Contains(description, "`a.png`") || !strings.Contains(description, "_(image not attached)_") || strings.Contains(description, "![nm-") {
				t.Fatalf("description should keep its text and say the image was not attached:\n%s", description)
			}
			logged := false
			for _, line := range *logs {
				logged = logged || (strings.Contains(line, "evidence images were not attached") && strings.Contains(line, "Downloads are disabled"))
			}
			if !logged {
				t.Fatalf("expected the refusal to be logged, got %v", *logs)
			}
			if route == "create" && api.logCount("pull-requests create") != 1 {
				t.Fatalf("a refused upload must not create a second PR, create calls = %d", api.logCount("pull-requests create"))
			}
		})
	}
}

// Only a bounded number of images go into one description; the rest are listed.
func TestPRStep_BitbucketEvidenceImageCount(t *testing.T) {
	t.Parallel()
	sctx, api, runDir := bitbucketEvidenceContext(t, "update", true, nil)
	for i := 0; i < maxBitbucketEvidenceImages+2; i++ {
		writeBBEvidenceFile(t, filepath.Join(runDir, "img"+string(rune('a'+i))+".png"), append(append([]byte{}, tinyPNG...), byte(i)))
	}
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := len(api.readState(t).Uploads); got != maxBitbucketEvidenceImages {
		t.Fatalf("uploads = %d, want the cap of %d", got, maxBitbucketEvidenceImages)
	}
	if got := strings.Count(api.lastDescription(t), "not attached: more than 10 images in one PR"); got != 2 {
		t.Fatalf("over-cap notes = %d, want 2:\n%s", got, api.lastDescription(t))
	}
}

// The end-of-run refresh rewrites the description from the run's final state, so
// it must carry the same image again: a refresh that dropped the embed would
// undo the PR step's work.
func TestFinalizeRun_BitbucketRefreshKeepsTheEvidenceImages(t *testing.T) {
	t.Parallel()
	api := newFakeBitbucketAPI(t, 42, carryPRURL)
	f := newRefreshFixture(t, api, "")
	f.c.Config.Test.Evidence.BitbucketAttachMedia = true
	runDir := filepath.Join(t.TempDir(), "evidence", "run-1")
	f.c.EvidenceDir = runDir
	writeBBEvidenceFile(t, filepath.Join(runDir, "final.png"), tinyPNG)

	if _, err := (&PRStep{}).Execute(f.c); err != nil {
		t.Fatal(err)
	}
	if err := (&PRStep{}).FinalizeRun(f.c); err != nil {
		t.Fatal(err)
	}
	uploads := api.readState(t).Uploads
	if len(uploads) < 2 {
		t.Fatalf("uploads = %v, want the image sent again by the refresh", uploads)
	}
	for _, name := range uploads {
		if name != uploads[0] || !strings.HasSuffix(name, "-final.png") {
			t.Fatalf("uploads = %v, want one stable name for the one image", uploads)
		}
	}
	description := api.lastDescription(t)
	if strings.Count(description, "![nm-") != 1 || strings.Contains(description, "[[nm-evidence-image") {
		t.Fatalf("refreshed description should carry the embed once:\n%s", description)
	}
}
