package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// MaxEvidenceImageBytes is the largest image twg uploads to Bitbucket
// Downloads for a PR description (`--image` accepts PNG, JPEG, GIF and WebP up
// to 10 MiB). Bitbucket has no route for video through twg.
const MaxEvidenceImageBytes = 10 << 20

// EvidenceImage is one local image a PR description may embed. Name is the
// file name twg uploads it under in Bitbucket Downloads; it is derived from the
// file's content, so the same image is always uploaded under the same name.
type EvidenceImage struct {
	Path string
	Name string
}

// EvidenceImages lists the images the PR writes may embed, in the order the
// description's markers number them (1-based). It is read at write time, so it
// must be deterministic for an unchanged evidence set.
type EvidenceImages func() []EvidenceImage

// EvidenceImageMarker returns the line a PR body uses to place evidence image n
// (1-based). The host turns it into twg's `{{image:N}}` placeholder when it
// uploads that image with the write, so nothing outside the run's evidence set
// can be named by body text: a marker with no matching image is dropped.
func EvidenceImageMarker(n int) string {
	return fmt.Sprintf("[[nm-evidence-image:%d]]", n)
}

var evidenceMarkerLine = regexp.MustCompile(`(?m)^[ \t]*\[\[nm-evidence-image:(\d+)\]\][ \t]*$`)

// WithEvidenceImages lets PR writes embed the run's evidence images in the
// description. logf, when set, reports a write that had to drop its images.
func (h *Host) WithEvidenceImages(source EvidenceImages, logf func(string)) *Host {
	h.evidence = source
	h.logf = logf
	return h
}

func (h *Host) logEvidence(format string, args ...any) {
	if h.logf != nil {
		h.logf(fmt.Sprintf(format, args...))
	}
}

// evidenceBody is a PR body prepared for one write.
type evidenceBody struct {
	// withImages carries twg placeholders for the uploads in imageArgs.
	withImages string
	// plain is the same body with every image marker replaced by a note, for
	// a write that sends no image.
	plain     string
	imageArgs []string
}

// prepareEvidence resolves the body's image markers against the evidence set.
// A body with no usable marker, or a host with no evidence set, is returned
// untouched with no image arguments.
func (h *Host) prepareEvidence(body string) evidenceBody {
	unchanged := evidenceBody{withImages: body, plain: body}
	if h.evidence == nil || !evidenceMarkerLine.MatchString(body) {
		return unchanged
	}
	images := h.evidence()
	if len(images) == 0 {
		return evidenceBody{
			withImages: evidenceMarkerLine.ReplaceAllString(body, ""),
			plain:      evidenceMarkerLine.ReplaceAllString(body, ""),
		}
	}
	// Literal twg placeholders in the rest of the body must not pick up an
	// image the description never asked for.
	body = strings.ReplaceAll(body, "{{image", "{​{image")

	var args []string
	numbered := map[int]int{}
	withImages := evidenceMarkerLine.ReplaceAllStringFunc(body, func(line string) string {
		n, err := strconv.Atoi(evidenceMarkerLine.FindStringSubmatch(line)[1])
		if err != nil || n < 1 || n > len(images) {
			return ""
		}
		pos, seen := numbered[n]
		if !seen {
			image := images[n-1]
			args = append(args, "--image", image.Path, "--image-name", image.Name)
			pos = len(numbered) + 1
			numbered[n] = pos
		}
		return fmt.Sprintf("{{image:%d}}", pos)
	})
	plain := evidenceMarkerLine.ReplaceAllString(body, "_(image not attached)_")
	return evidenceBody{withImages: withImages, plain: plain, imageArgs: args}
}

// updateDescription writes a PR's description (and title when non-empty), with
// images uploaded alongside it when imageArgs is set.
func (h *Host) updateDescription(ctx context.Context, pr *scm.PR, title, body string, imageArgs []string) (*scm.PR, error) {
	out, err := h.runWithDescription(ctx, body, func(descPath string) []string {
		args := []string{"bb", "pull-requests", "update",
			"--pull-request", pr.Number,
			"--description-file", descPath,
		}
		if strings.TrimSpace(title) != "" {
			args = append(args, "--title", title)
		}
		args = append(args, imageArgs...)
		args = append(args, h.repoArgs()...)
		return args
	})
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: %w", err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var updated bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &updated); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return updated.toPR(h.repo), nil
}
