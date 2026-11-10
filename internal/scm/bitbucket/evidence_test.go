package bitbucket

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func evidenceHost(rec *[]capturedCmd, response bbTestResponse, images []EvidenceImage, logs *[]string) *Host {
	h := baseHost(rec, response)
	return h.WithEvidenceImages(func() []EvidenceImage { return images }, func(line string) { *logs = append(*logs, line) })
}

var twoImages = []EvidenceImage{
	{Path: "/run/a.png", Name: "nm-aaaaaaaaaaaa-a.png"},
	{Path: "/run/b.png", Name: "nm-bbbbbbbbbbbb-b.png"},
}

func TestPrepareEvidenceTurnsMarkersIntoTwgPlaceholders(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	h := evidenceHost(&rec, bbTestResponse{}, twoImages, &logs)
	body := "## Evidence\n\n`b.png`\n\n" + EvidenceImageMarker(2) + "\n\n`a.png`\n\n  " + EvidenceImageMarker(1) + "  \n\n" + EvidenceImageMarker(2) + "\n"
	got := h.prepareEvidence(body)
	// Images are numbered in the order the body first uses them.
	if want := "`b.png`\n\n{{image:1}}\n\n`a.png`\n\n{{image:2}}\n\n{{image:1}}\n"; !strings.HasSuffix(got.withImages, want) {
		t.Fatalf("withImages = %q, want it to end with %q", got.withImages, want)
	}
	wantArgs := []string{"--image", "/run/b.png", "--image-name", "nm-bbbbbbbbbbbb-b.png", "--image", "/run/a.png", "--image-name", "nm-aaaaaaaaaaaa-a.png"}
	if strings.Join(got.imageArgs, "|") != strings.Join(wantArgs, "|") {
		t.Fatalf("imageArgs = %v, want %v", got.imageArgs, wantArgs)
	}
	if strings.Contains(got.plain, "[[nm-evidence-image") || strings.Contains(got.plain, "{{image") || strings.Count(got.plain, "_(image not attached)_") != 3 {
		t.Fatalf("plain = %q, want every marker replaced by a note", got.plain)
	}
}

// A body that names no image, or a host with no evidence set, is sent exactly as
// written: nothing about an ordinary PR write changes.
func TestPrepareEvidenceLeavesOtherBodiesAlone(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	body := "text {{image}} and [[nm-evidence-image:1]] inline, not on its own line"
	for name, h := range map[string]*Host{
		"no evidence set":   baseHost(&rec, bbTestResponse{}),
		"with evidence set": evidenceHost(&rec, bbTestResponse{}, twoImages, &logs),
	} {
		got := h.prepareEvidence(body)
		if got.withImages != body || got.plain != body || len(got.imageArgs) != 0 {
			t.Fatalf("%s: prepared %+v, want the body untouched", name, got)
		}
	}
	marker := EvidenceImageMarker(1)
	if got := baseHost(&rec, bbTestResponse{}).prepareEvidence(marker); got.withImages != marker || len(got.imageArgs) != 0 {
		t.Fatalf("a host with no evidence set must not rewrite markers, got %+v", got)
	}
}

// Body text can name only images in the evidence set: a marker with no such
// image is dropped, and a literal twg placeholder in the narrative is defused
// so it cannot pick up an upload.
func TestPrepareEvidenceCannotBeSteeredByBodyText(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	h := evidenceHost(&rec, bbTestResponse{}, twoImages[:1], &logs)
	got := h.prepareEvidence("narrative {{image}} and {{image:2}}\n\n" + EvidenceImageMarker(1) + "\n\n" + EvidenceImageMarker(7) + "\n\n" + EvidenceImageMarker(0) + "\n")
	if strings.Contains(got.withImages, "{{image}}") || strings.Contains(got.withImages, "{{image:2}}") {
		t.Fatalf("a narrative placeholder survived: %q", got.withImages)
	}
	if strings.Count(got.withImages, "{{image:1}}") != 1 || strings.Contains(got.withImages, "{{image:2") {
		t.Fatalf("withImages = %q, want exactly the one real image placed", got.withImages)
	}
	if len(got.imageArgs) != 4 {
		t.Fatalf("imageArgs = %v, want one image only", got.imageArgs)
	}
	none := evidenceHost(&rec, bbTestResponse{}, nil, &logs).prepareEvidence(EvidenceImageMarker(1) + "\n")
	if strings.Contains(none.withImages, "nm-evidence-image") || strings.Contains(none.plain, "nm-evidence-image") || len(none.imageArgs) != 0 {
		t.Fatalf("with no images the markers must disappear, got %+v", none)
	}
}

func TestUpdatePRUploadsEvidenceWithTheDescription(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	h := evidenceHost(&rec, bbTestResponse{stdout: `{"id":42}`}, twoImages, &logs)
	if _, err := h.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: "T", Body: "x\n\n" + EvidenceImageMarker(1) + "\n"}); err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 {
		t.Fatalf("commands = %d, want one write", len(rec))
	}
	got := joined(rec[0])
	if want := "--image /run/a.png --image-name nm-aaaaaaaaaaaa-a.png --workspace ws --repo repo"; !strings.Contains(got, want) {
		t.Fatalf("command = %q, want the upload before the repository flags (%q)", got, want)
	}
	if rec[0].descContent != "x\n\n{{image:1}}\n" {
		t.Fatalf("description sent = %q", rec[0].descContent)
	}
	if len(logs) != 0 {
		t.Fatalf("unexpected log: %v", logs)
	}
}

// Bitbucket refusing the images must not cost the description.
func TestUpdatePRRetriesWithoutImagesWhenTheUploadIsRefused(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	// Every write fails: the point is that a second write without images is attempted.
	h := evidenceHost(&rec, bbTestResponse{stderr: "uploads disabled", code: 1}, twoImages, &logs)
	_, err := h.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Body: EvidenceImageMarker(1) + "\n"})
	if err == nil {
		t.Fatal("expected the final write to report its own failure")
	}
	if len(rec) != 2 || strings.Contains(joined(rec[1]), "--image") || !strings.Contains(joined(rec[0]), "--image") {
		t.Fatalf("commands = %v, want a write with the image and then one without", rec)
	}
	if rec[1].descContent != "_(image not attached)_\n" {
		t.Fatalf("fallback description = %q", rec[1].descContent)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "evidence images were not attached") || !strings.Contains(logs[0], "uploads disabled") {
		t.Fatalf("logs = %v, want one line naming the refusal", logs)
	}
}

// A PR is created from text alone and the images follow in an update, so a
// failure while uploading can never produce a second PR.
func TestCreatePRSendsTextFirstThenTheImages(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	h := evidenceHost(&rec, bbTestResponse{stdout: `{"id":42,"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}}}`}, twoImages, &logs)
	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: "x\n\n" + EvidenceImageMarker(2) + "\n"})
	if err != nil || pr == nil || pr.Number != "42" {
		t.Fatalf("CreatePR() = %v, %v", pr, err)
	}
	if len(rec) != 2 {
		t.Fatalf("commands = %d, want create then update", len(rec))
	}
	if strings.Contains(joined(rec[0]), "--image") || !strings.Contains(joined(rec[0]), "pull-requests create") || rec[0].descContent != "x\n\n_(image not attached)_\n" {
		t.Fatalf("create must carry text only: %q / %q", joined(rec[0]), rec[0].descContent)
	}
	if !strings.Contains(joined(rec[1]), "pull-requests update --pull-request 42") || !strings.Contains(joined(rec[1]), "--image /run/b.png --image-name nm-bbbbbbbbbbbb-b.png") || rec[1].descContent != "x\n\n{{image:1}}\n" {
		t.Fatalf("update must add the image: %q / %q", joined(rec[1]), rec[1].descContent)
	}
}

func TestCreatePRKeepsTheTextWhenTheImageUpdateFails(t *testing.T) {
	var rec []capturedCmd
	var logs []string
	calls := 0
	h := evidenceHost(&rec, bbTestResponse{}, twoImages, &logs)
	h.cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		response := bbTestResponse{stdout: `{"id":42}`}
		if strings.Contains(strings.Join(args, " "), "pull-requests update") {
			response = bbTestResponse{stderr: "image refused", code: 1}
		}
		return capturingCmdFactory(&rec, response)(ctx, name, args...)
	}
	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "T", Body: EvidenceImageMarker(1) + "\n"})
	if err != nil || pr == nil || pr.Number != "42" {
		t.Fatalf("a refused image update must not fail the PR creation: %v, %v", pr, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "image refused") {
		t.Fatalf("logs = %v", logs)
	}
}
