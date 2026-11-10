package bitbucket

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// minAbbreviatedHashLen is the shortest source.commit.hash that proves a head.
// Bitbucket Cloud reports a pull request's source commit as a 12-character
// abbreviation of the full object ID, so a prefix of that length is the most
// the forge ever states. Anything shorter identifies nothing.
const minAbbreviatedHashLen = 12

// VerifyPRHead proves from a fresh `twg bb pull-requests get` read, by the PR's
// immutable number and independently of FindPR and any create/update response,
// that the review object is open in the configured repository and that its
// source branch and source commit are the ones just pushed.
//
// It returns nil only for a positive proof. Every other outcome is an error: a
// twg failure, an unreadable or incomplete read, a PR that is not open or does
// not belong to the configured repository or branch, and a source commit that
// is missing, too short to identify anything, or does not match. A source
// commit that disagrees with the pushed head is reported as a
// *scm.PRHeadChangedError so the caller can wait out the forge's propagation
// delay, exactly as for the other providers. Bitbucket states the commit as a
// 12-character prefix, so the comparison is prefix-wise and the live value in
// that error is the abbreviation the forge returned.
func (h *Host) VerifyPRHead(ctx context.Context, pr *scm.PR, branch, pushedHead string) error {
	if pr == nil || strings.TrimSpace(branch) == "" || !completeObjectID(pushedHead) {
		return fmt.Errorf("verify PR head: incomplete expected identity or pushed head")
	}
	number, err := strconv.Atoi(strings.TrimSpace(pr.Number))
	if err != nil || number <= 0 {
		return fmt.Errorf("verify PR head: invalid Bitbucket pull number")
	}
	live, _, err := h.getPR(ctx, pr.Number)
	if err != nil {
		return fmt.Errorf("verify PR head: %w", err)
	}
	if live.ID != number || !h.namesPullRequest(live.Links.HTML.Href, number) {
		return fmt.Errorf("verify PR head: live review object does not match the configured repository and PR")
	}
	if normalizePRState(live.State) != scm.PRStateOpen {
		return fmt.Errorf("verify PR head: review object is not proven open")
	}
	if want := h.repo.Workspace + "/" + h.repo.RepoSlug; !strings.EqualFold(strings.TrimSpace(live.Source.Repository.FullName), want) {
		return fmt.Errorf("verify PR head: source repository does not match the push target")
	}
	if live.Source.Branch.Name != branch {
		return fmt.Errorf("verify PR head: source branch does not match the push target")
	}
	hash := strings.ToLower(strings.TrimSpace(live.Source.Commit.Hash))
	if len(hash) < minAbbreviatedHashLen || len(hash) > len(pushedHead) || !isHex(hash) {
		return fmt.Errorf("verify PR head: live head is missing or too short to identify a commit")
	}
	if !strings.HasPrefix(strings.ToLower(pushedHead), hash) {
		return fmt.Errorf("verify PR head: %w does not equal successfully pushed head %s", &scm.PRHeadChangedError{HeadSHA: hash}, pushedHead)
	}
	return nil
}

// namesPullRequest reports whether href is the web URL of pull request number
// in the configured workspace and repository.
func (h *Host) namesPullRequest(href string, number int) bool {
	suffix := fmt.Sprintf("/%s/%s/pull-requests/%d", h.repo.Workspace, h.repo.RepoSlug, number)
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(href)), strings.ToLower(suffix))
}

func completeObjectID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHex(s)
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return s != ""
}

var _ scm.PRHeadVerifier = (*Host)(nil)
