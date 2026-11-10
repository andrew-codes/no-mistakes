package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRRepositoryContent reads a numbered PR's text together with the web URL
// that names its repository, for the repository-bound read-only appendix
// verification. Like GetPRContent it reads twg's normalized top-level
// "description" and never infers an empty body from an absent one.
func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	id, err := strconv.Atoi(number)
	if err != nil || id <= 0 {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Bitbucket PR number")
	}
	view, trimmed, err := h.getPR(ctx, number)
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	var raw struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	if raw.ID != id || strings.TrimSpace(raw.Title) == "" || raw.Description == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("Bitbucket pull: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{
		Content:    scm.PRContent{Title: raw.Title, Body: *raw.Description},
		Repository: scm.RepositoryIdentityFromPRURL(view.Links.HTML.Href, "/pull-requests/", number),
	}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
