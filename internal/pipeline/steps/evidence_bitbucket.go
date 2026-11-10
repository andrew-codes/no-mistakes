package steps

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
)

// maxBitbucketEvidenceImages bounds how many images one PR description embeds.
const maxBitbucketEvidenceImages = 10

// bitbucketEvidenceEntry is one evidence file as the Evidence section shows it.
type bitbucketEvidenceEntry struct {
	// mention names the file: a link when test.evidence.link_base_url is set,
	// else its bare file name. It never carries a path.
	mention string
	// upload is set for an image the description embeds.
	upload *bitbucket.EvidenceImage
	// note says why a file is listed without an embed. Empty for an embed and
	// for a plain link.
	note string
}

// bitbucketEvidencePlan reads the run's evidence directory, and only that
// directory, into the entries the Evidence section lists, in a stable order.
// Embedded images are the only files that leave the machine: PNG, JPEG, GIF and
// WebP files that twg can upload (up to 10 MiB each), and only when
// test.evidence.attach_media is explicitly true. Video has no twg route, so it
// is linked when link_base_url is set and otherwise listed as not attached.
func bitbucketEvidencePlan(sctx *pipeline.StepContext) []bitbucketEvidenceEntry {
	if sctx == nil || sctx.Config == nil {
		return nil
	}
	runDir := testEvidenceDir(sctx)
	if runDir == "" {
		return nil
	}
	upload := sctx.Config.Test.Evidence.BitbucketAttachMedia
	linkBase := evidenceLinkBase(sctx.Config)
	if !upload && linkBase == "" {
		return nil
	}
	root := resolveArtifactPathSymlinks(filepath.Clean(runDir))
	type found struct {
		rel  string
		path string
		size int64
	}
	var files []found
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == reviewqa.DirName && path != root {
				return fs.SkipDir
			}
			return nil
		}
		// Regular files only: a symlink could point outside the evidence set.
		if !entry.Type().IsRegular() {
			return nil
		}
		if !bitbucketEvidenceImageExt(path) && !isVideoArtifact("", path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, found{rel: filepath.ToSlash(rel), path: path, size: info.Size()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	var entries []bitbucketEvidenceEntry
	embedded := 0
	for _, file := range files {
		label := bitbucketEvidenceLabel(filepath.Base(file.path))
		mention := "`" + label + "`"
		if link := renderEvidenceFileLink(linkBase, root, file.path, label); link != "" {
			mention = link
		}
		entry := bitbucketEvidenceEntry{mention: mention}
		switch {
		case isVideoArtifact("", file.path):
			if linkBase == "" {
				entry.note = "not attached: Bitbucket attachments through twg are limited to PNG, JPEG, GIF and WebP images"
			}
		case !upload:
			// Linked, not uploaded.
		case file.size > bitbucket.MaxEvidenceImageBytes:
			entry.note = "not attached: larger than 10 MiB"
		case embedded >= maxBitbucketEvidenceImages:
			entry.note = fmt.Sprintf("not attached: more than %d images in one PR", maxBitbucketEvidenceImages)
		default:
			digest, err := mediaFileDigest(file.path)
			if err != nil {
				entry.note = "not attached: unreadable"
				break
			}
			embedded++
			entry.upload = &bitbucket.EvidenceImage{Path: file.path, Name: bitbucketEvidenceUploadName(digest, label)}
		}
		entries = append(entries, entry)
	}
	return entries
}

// bitbucketEvidenceUploads is the ordered image list a PR write may embed: the
// description numbers its markers by position in it.
func bitbucketEvidenceUploads(sctx *pipeline.StepContext) []bitbucket.EvidenceImage {
	var images []bitbucket.EvidenceImage
	for _, entry := range bitbucketEvidencePlan(sctx) {
		if entry.upload != nil {
			images = append(images, *entry.upload)
		}
	}
	return images
}

// bitbucketEvidenceSection renders the Evidence section: each uploadable image
// as its file name and an image marker the host swaps for the uploaded image,
// and every other file as a link or a one-line reason. Bitbucket only.
func (s *PRStep) bitbucketEvidenceSection(sctx *pipeline.StepContext, provider scm.Provider) string {
	if provider != scm.ProviderBitbucket {
		return ""
	}
	var blocks []string
	embedded := 0
	for _, entry := range bitbucketEvidencePlan(sctx) {
		switch {
		case entry.upload != nil:
			embedded++
			blocks = append(blocks, entry.mention+"\n\n"+bitbucket.EvidenceImageMarker(embedded))
		case entry.note != "":
			blocks = append(blocks, entry.mention+" - "+entry.note)
		default:
			blocks = append(blocks, entry.mention)
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return "## Evidence\n\n" + strings.Join(blocks, "\n\n")
}

// providerEvidenceSection is the Evidence section for providers that publish
// run images into the PR description.
func (s *PRStep) providerEvidenceSection(sctx *pipeline.StepContext, provider scm.Provider) string {
	switch provider {
	case scm.ProviderGitLab:
		return s.gitlabEvidenceSection(sctx, provider)
	case scm.ProviderBitbucket:
		return s.bitbucketEvidenceSection(sctx, provider)
	}
	return ""
}

func bitbucketEvidenceImageExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// bitbucketEvidenceLabel is a file's bare name made safe for a code span or a
// link text: no markup characters, no control characters, bounded length.
func bitbucketEvidenceLabel(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '`' || r == '[' || r == ']' || r == '|' || r == '<' || r == '>':
			b.WriteRune('_')
		case unicode.IsControl(r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	label := b.String()
	if len([]rune(label)) > 80 {
		label = string([]rune(label)[:80])
	}
	return label
}

// bitbucketEvidenceUploadName is the file name an image is uploaded under in
// Bitbucket Downloads: the start of its content digest and a safe spelling of
// its own name, so the same image always lands under the same name and two
// images never share one.
func bitbucketEvidenceUploadName(digest, label string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	var b strings.Builder
	for _, r := range label {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "nm-" + digest + "-" + b.String()
}
