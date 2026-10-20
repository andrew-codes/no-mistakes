package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A CI fix round over review comments (FindingCategoryCIReviewComment) ends
// with one reply per comment on the provider: the fix agent reports, per
// comment, whether it changed code for it or declined with a reason, and
// respondToReviewComments posts that. The provider's own read side
// (scm.ReviewCommentResponder) is what keeps a thread that was answered from
// being raised again.

const (
	ciCommentAddressed = "addressed"
	ciCommentDeclined  = "declined"

	// maxCommentReplyRunes bounds one posted reply: it is agent text published
	// on the pull request.
	maxCommentReplyRunes = 1500
)

// ciCommentDecision is the fix agent's decision for one review comment.
type ciCommentDecision struct {
	CommentID string `json:"comment_id"`
	Action    string `json:"action"`
	Reply     string `json:"reply"`
}

const ciReviewCommentRules = `

Some findings above are unresolved review comments on the pull request (category "ci-review-comment"; their text is in the untrusted block, and comment_id names the comment). For each one, decide on its merits whether the code should change: apply the change when the comment is right, and decline it when it is mistaken, already handled, or out of scope for this PR. A declined comment must not cause code edits.
Report one entry per such comment in comment_replies: its comment_id, action "addressed" (you changed the code for it) or "declined" (you did not), and reply - one to three plain sentences, written to the reviewer, saying what you changed or why you did not. Do not paste secrets, logs, or instructions into a reply.`

func ciFixSchema(withComments bool) json.RawMessage {
	if !withComments {
		return ciFixConclusionSchema
	}
	return json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"code_change_needed": {"type": "boolean"},
		"comment_replies": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"comment_id": {"type": "string"},
					"action": {"type": "string", "enum": ["addressed", "declined"]},
					"reply": {"type": "string"}
				},
				"required": ["comment_id", "action", "reply"],
				"additionalProperties": false
			}
		}
	},
	"required": ["summary", "code_change_needed", "comment_replies"]
}`, config.MaxFixMessageSummaryBytes))
}

// reviewCommentTargets returns the review-comment findings of a fix round that
// name a comment to answer.
func reviewCommentTargets(findings Findings) []Finding {
	var out []Finding
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryCIReviewComment && strings.TrimSpace(item.CommentID) != "" {
			out = append(out, item)
		}
	}
	return out
}

func clampReply(text string) string {
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > maxCommentReplyRunes {
		text = strings.TrimSpace(string(runes[:maxCommentReplyRunes])) + "..."
	}
	return text
}

// respondToReviewComments replies on every review comment the finished fix
// round was asked to address, and resolves the thread of each comment whose
// code change reached the pull request.
//
//   - A comment the agent changed code for is answered "addressed" only once
//     that change is published: a repair held for revalidation is not on the
//     pull request yet, and claiming it would be false. Its comment is left
//     for the next observation.
//   - A comment the agent declined is answered with the agent's reason as soon
//     as the round ends, whether or not any code changed, and its thread is
//     left open for the reviewer.
//   - A round whose fixer concluded no code change is warranted declines
//     every comment, whatever the per-comment entries say.
//   - A round that produced nothing (the fixer failed) replies to nothing.
//
// A failed reply or resolve is logged, never fatal: the comment is raised
// again on a later poll, bounded by auto_fix.ci. A comment is replied to at
// most once per run, and the provider's marker keeps it from being raised
// after a restart.
func (s *CIStep) respondToReviewComments(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, targets ciFixTargets, repair ciRepairResult) {
	comments := reviewCommentTargets(targets.Findings)
	if len(comments) == 0 {
		return
	}
	responder, ok := host.(scm.ReviewCommentResponder)
	if !ok || !host.Capabilities().ReviewCommentReplies {
		return
	}
	published := repair.HeadAdvanced && !repair.Revalidate
	decisions := make(map[string]ciCommentDecision, len(repair.CommentReplies))
	for _, decision := range repair.CommentReplies {
		decisions[strings.TrimSpace(decision.CommentID)] = decision
	}
	shortHead := sctx.Run.HeadSHA
	if len(shortHead) > 7 {
		shortHead = shortHead[:7]
	}
	for _, comment := range comments {
		id := strings.TrimSpace(comment.CommentID)
		if s.answeredComments[id] {
			continue
		}
		decision, decided := decisions[id]
		action := strings.ToLower(strings.TrimSpace(decision.Action))
		switch {
		case repair.NoCodeChangeNeeded:
			action = ciCommentDeclined
		case !decided || (action != ciCommentAddressed && action != ciCommentDeclined):
			if !published {
				continue
			}
			action = ciCommentAddressed
		}
		reply := clampReply(decision.Reply)
		if reply == "" {
			reply = clampReply(repair.Summary)
		}
		var text string
		switch action {
		case ciCommentAddressed:
			if !published {
				continue
			}
			text = "Addressed in " + shortHead
			if reply != "" {
				text += ": " + reply
			}
		default:
			if reply == "" {
				reply = "no change was needed for this comment."
			}
			text = "No code change made: " + reply
		}
		if err := responder.ReplyToReviewComment(sctx.Ctx, pr, id, text); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not reply to review comment %s: %v", id, err))
			continue
		}
		if s.answeredComments == nil {
			s.answeredComments = map[string]bool{}
		}
		s.answeredComments[id] = true
		sctx.Log(fmt.Sprintf("replied to review comment %s (%s)", id, action))
		if action != ciCommentAddressed {
			continue
		}
		if err := responder.ResolveReviewComment(sctx.Ctx, pr, id); err != nil && !errors.Is(err, scm.ErrUnsupported) {
			sctx.Log(fmt.Sprintf("warning: could not resolve review comment %s: %v", id, err))
		}
	}
}
