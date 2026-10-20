package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var (
	_ scm.ReviewCommentsHost     = (*Host)(nil)
	_ scm.ReviewCommentResponder = (*Host)(nil)
)

const (
	// ReplyMarker is embedded (as an HTML comment, which Bitbucket keeps in
	// the raw body and does not render) in every reply this package posts.
	// It is how a comment that was already answered is recognized on the next
	// read, durably and across daemon restarts: a root comment with a marked
	// reply on its thread is never returned again.
	ReplyMarker = "<!-- no-mistakes-review-reply:v1 -->"

	// pipelineAttestationPrefix is the marker the pipeline's own attestation
	// carries (see steps.pipelineAttestationCommentPrefix). A comment holding
	// it is the pipeline's own record, never review feedback.
	pipelineAttestationPrefix = "<!-- no-mistakes-pipeline-attestation:v1 "

	// reviewCommentLimit is passed to `comment query --limit`; twg's default of
	// 50 would silently drop the rest of a long review thread.
	reviewCommentLimit = "500"
)

// bitbucketComment is the subset of one `twg bb pull-requests comment query`
// entry this package reads. The shape is Bitbucket Cloud's own comment object,
// which twg passes through verbatim.
type bitbucketComment struct {
	ID      int64 `json:"id"`
	Deleted bool  `json:"deleted"`
	Pending bool  `json:"pending"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	CreatedOn string `json:"created_on"`
	User      struct {
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		Nickname    string `json:"nickname"`
		AccountID   string `json:"account_id"`
		AIAgent     bool   `json:"ai_agent"`
	} `json:"user"`
	// Parent is set on a reply and names the comment it answers.
	Parent *struct {
		ID int64 `json:"id"`
	} `json:"parent"`
	// Resolution is present (non-null) once the thread is resolved.
	Resolution json.RawMessage `json:"resolution"`
	// Inline anchors the comment to a diff line; absent on a general comment.
	Inline *struct {
		Path string `json:"path"`
		From *int   `json:"from"`
		To   *int   `json:"to"`
	} `json:"inline"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (c bitbucketComment) author() string {
	if name := strings.TrimSpace(c.User.DisplayName); name != "" {
		return name
	}
	return strings.TrimSpace(c.User.Nickname)
}

func (c bitbucketComment) resolved() bool {
	trimmed := strings.TrimSpace(string(c.Resolution))
	return trimmed != "" && trimmed != "null"
}

// human reports whether the author is a person. Bitbucket marks apps with a
// non-"user" account type, and automation accounts that are ordinary users
// (a pipelines bot, for example) are recognized by a bot-style name.
func (c bitbucketComment) human() bool {
	if c.User.AIAgent || (c.User.Type != "" && c.User.Type != "user") {
		return false
	}
	for _, name := range []string{c.User.Nickname, c.User.DisplayName} {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "bot" || strings.HasSuffix(lower, "-bot") || strings.HasSuffix(lower, " bot") || strings.HasSuffix(lower, "[bot]") {
			return false
		}
	}
	return true
}

// GetReviewComments implements scm.ReviewCommentsHost. It returns the root
// comments of the PR's open review threads, human and bot, inline and
// general, that nobody has answered yet:
//
//   - deleted comments, unpublished (pending) drafts, replies, and threads
//     already resolved are never feedback;
//   - the authenticated user's own comments are never feedback: the pipeline
//     speaks as that user, and so does its author when they annotate the PR;
//   - a comment carrying the pipeline's attestation marker or a reply marker
//     is the pipeline's own;
//   - a thread that already holds a reply posted through ReplyToReviewComment
//     is handled, so no comment is raised or answered twice;
//   - a GENERAL comment from a non-human author (a merge-on-green notice, a
//     review bot's summary) is a notice, not a request: the bot's actionable
//     feedback is its inline comments, which are kept.
func (h *Host) GetReviewComments(ctx context.Context, pr *scm.PR) ([]scm.ReviewComment, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return nil, errors.New("twg bb pull-requests comment query: missing pull request number")
	}
	self, err := h.currentAccountID(ctx)
	if err != nil {
		return nil, err
	}
	comments, err := h.queryComments(ctx, pr.Number)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]bitbucketComment, len(comments))
	for _, c := range comments {
		byID[c.ID] = c
	}
	// rootOf walks a reply chain up to its thread's root. The visited guard
	// keeps a malformed cyclic parent link from looping.
	rootOf := func(c bitbucketComment) int64 {
		seen := map[int64]bool{c.ID: true}
		for c.Parent != nil {
			parent, ok := byID[c.Parent.ID]
			if !ok || seen[parent.ID] {
				return c.Parent.ID
			}
			seen[parent.ID] = true
			c = parent
		}
		return c.ID
	}
	answered := map[int64]bool{}
	for _, c := range comments {
		if c.Parent != nil && !c.Deleted && strings.Contains(c.Content.Raw, ReplyMarker) {
			answered[rootOf(c)] = true
		}
	}

	var out []scm.ReviewComment
	for _, c := range comments {
		if c.Deleted || c.Pending || c.Parent != nil || c.resolved() || answered[c.ID] {
			continue
		}
		body := strings.TrimSpace(c.Content.Raw)
		if body == "" || strings.Contains(c.Content.Raw, pipelineAttestationPrefix) || strings.Contains(c.Content.Raw, ReplyMarker) {
			continue
		}
		if self != "" && c.User.AccountID == self {
			continue
		}
		inline := c.Inline != nil && strings.TrimSpace(c.Inline.Path) != ""
		if !inline && !c.human() {
			continue
		}
		comment := scm.ReviewComment{
			ID:     strconv.FormatInt(c.ID, 10),
			Author: c.author(),
			Body:   body,
			URL:    strings.TrimSpace(c.Links.HTML.Href),
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(c.CreatedOn)); err == nil {
			comment.CreatedAt = t
		}
		if inline {
			comment.Path = strings.TrimSpace(c.Inline.Path)
			switch {
			case c.Inline.To != nil:
				comment.Line = *c.Inline.To
			case c.Inline.From != nil:
				comment.Line = *c.Inline.From
			}
		}
		out = append(out, comment)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := strconv.ParseInt(out[i].ID, 10, 64)
		b, _ := strconv.ParseInt(out[j].ID, 10, 64)
		return a < b
	})
	return out, nil
}

func (h *Host) queryComments(ctx context.Context, number string) ([]bitbucketComment, error) {
	args := []string{"bb", "pull-requests", "comment", "query", number, "--limit", reviewCommentLimit}
	args = append(args, h.repoArgs()...)
	out, err := h.runJSON(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests comment query: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	invalid := fmt.Errorf("twg bb pull-requests comment query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	if len(trimmed) == 0 {
		return nil, invalid
	}
	var comments []bitbucketComment
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &comments); err != nil {
			return nil, invalid
		}
		return comments, nil
	}
	var wrapped struct {
		Values *[]bitbucketComment `json:"values"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil || wrapped.Values == nil {
		return nil, invalid
	}
	return *wrapped.Values, nil
}

// currentAccountID reads the authenticated twg user's Atlassian account id,
// which Bitbucket stamps on every comment that user posts. An unreadable
// answer is an error: guessing "nobody" would make the pipeline treat its own
// replies and its author's own notes as review feedback.
func (h *Host) currentAccountID(ctx context.Context) (string, error) {
	out, err := h.runJSON(ctx, []string{"whoami"})
	if err != nil {
		return "", fmt.Errorf("twg whoami: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var who struct {
		Data struct {
			AccountID string `json:"accountId"`
		} `json:"data"`
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &who) != nil || strings.TrimSpace(who.Data.AccountID) == "" {
		return "", fmt.Errorf("twg whoami: could not read the account id: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(who.Data.AccountID), nil
}

// ReplyToReviewComment implements scm.ReviewCommentResponder. The reply carries
// ReplyMarker so GetReviewComments recognizes the thread as answered.
func (h *Host) ReplyToReviewComment(ctx context.Context, pr *scm.PR, commentID, body string) error {
	if pr == nil || strings.TrimSpace(pr.Number) == "" || strings.TrimSpace(commentID) == "" {
		return errors.New("twg bb pull-requests comment create: missing pull request number or comment id")
	}
	text := strings.TrimSpace(body)
	if text == "" {
		return errors.New("twg bb pull-requests comment create: empty reply")
	}
	args := []string{"bb", "pull-requests", "comment", "create",
		"--pull-request", pr.Number,
		"--reply-to", strings.TrimSpace(commentID),
		"--text", text + "\n\n" + ReplyMarker,
	}
	args = append(args, h.repoArgs()...)
	if out, err := h.runJSON(ctx, args); err != nil {
		return fmt.Errorf("twg bb pull-requests comment create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ResolveReviewComment implements scm.ReviewCommentResponder.
func (h *Host) ResolveReviewComment(ctx context.Context, pr *scm.PR, commentID string) error {
	if pr == nil || strings.TrimSpace(pr.Number) == "" || strings.TrimSpace(commentID) == "" {
		return errors.New("twg bb pull-requests comment resolve: missing pull request number or comment id")
	}
	args := []string{"bb", "pull-requests", "comment", "resolve",
		"--pull-request", pr.Number,
		"--comment", strings.TrimSpace(commentID),
	}
	args = append(args, h.repoArgs()...)
	if out, err := h.runJSON(ctx, args); err != nil {
		return fmt.Errorf("twg bb pull-requests comment resolve: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
