package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Review feedback is a request the moment it lands. The monitor used to read
// it only once the checks had settled (red or all green), so a comment left
// while checks were still running, while mergeability was still being
// computed, or on a draft whose checks had not registered sat unread for as
// long as the wait lasted. These tests pin the read to the poll cadence of the
// wait itself.

const (
	bbRunningStatuses = `[{"name":"build","state":"INPROGRESS"}]`
	bbGreenStatuses   = `[{"name":"build","state":"SUCCESSFUL"}]`
	bbNoStatuses      = `[]`
	bbPassedGitCheck  = `[{"type":"git_mergeability_check","status":"PASSED","blocking":false}]`

	// One request from a person, one notice the monitor must ignore.
	bbOneRequestAndANotice = `[
 {"id":2000,"content":{"raw":"[Mergebot](https://example.com/mergebot) auto-merge is enabled."},
  "user":{"type":"user","display_name":"example-merge-bot","nickname":"example-merge-bot","account_id":"a1b2c3d4"}},
 {"id":2002,"content":{"raw":"Please also cover the empty-account case."},
  "user":{"type":"user","display_name":"Reviewer Rae","nickname":"Reviewer Rae","account_id":"712020:rae"}}
]`

	// The same request, already answered by a reply carrying the marker.
	bbOneRequestAlreadyAnswered = `[
 {"id":2002,"content":{"raw":"Please also cover the empty-account case."},
  "user":{"type":"user","display_name":"Reviewer Rae","nickname":"Reviewer Rae","account_id":"712020:rae"}},
 {"id":2003,"parent":{"id":2002},"content":{"raw":"No code change made: covered. ` + bitbucket.ReplyMarker + `"},
  "user":{"type":"user","display_name":"Fake User","account_id":"712020:fake-self"}}
]`
)

func commentFindingIDs(t *testing.T, findings string) []string {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(findings)
	if err != nil {
		t.Fatalf("parse findings %q: %v", findings, err)
	}
	var ids []string
	for _, item := range parsed.Items {
		if item.Category != types.FindingCategoryCIReviewComment {
			t.Fatalf("finding %+v is not a review comment: waiting must raise only the feedback", item)
		}
		ids = append(ids, item.CommentID)
	}
	return ids
}

// A comment that arrives while the monitor is waiting becomes an auto-fix
// finding on the next poll, whatever the wait is for and whether or not the PR
// is a draft.
func TestCIStep_BitbucketRaisesCommentsWhileWaiting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		draft        bool
		statuses     string
		mergeability string
	}{
		{"checks still running", false, bbRunningStatuses, bbPassedGitCheck},
		{"draft with checks still running", true, bbRunningStatuses, bbDraftPendingGitCheck},
		{"mergeability still pending on a ready PR", false, bbGreenStatuses, bbDraftPendingGitCheck},
		{"draft with no checks registered yet", true, bbNoStatuses, bbDraftPendingGitCheck},
		{"ready PR with no checks registered yet", false, bbNoStatuses, bbPassedGitCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := bbDraftAPI(t, tc.mergeability).withStatuses(tc.statuses).withComments(bbOneRequestAndANotice)
			sctx, logs := bbCIDraftContext(t, api, tc.draft)
			step := onePollThenCancel(sctx)

			outcome, err := driveCI(t, step, sctx)
			if err != nil {
				t.Fatalf("CI step error = %v\nlogs:\n%s", err, strings.Join(*logs, "\n"))
			}
			if outcome == nil || outcome.CIReadyNoCI != nil || outcome.Findings == "" {
				t.Fatalf("outcome = %#v, want the comment raised while waiting\nlogs:\n%s", outcome, strings.Join(*logs, "\n"))
			}
			if ids := commentFindingIDs(t, outcome.Findings); len(ids) != 1 || ids[0] != "2002" {
				t.Fatalf("raised comments = %v, want only the request (2002), never the notice", ids)
			}
			if state := api.readState(t).Draft; state != tc.draft {
				t.Fatalf("draft = %v after the poll, want it untouched (%v): publication is the end of the run", state, tc.draft)
			}
		})
	}
}

// Nothing to answer means nothing changes: the monitor keeps waiting, and a
// thread that already holds a marked reply is never raised again.
func TestCIStep_BitbucketWaitingIgnoresWhatIsNotFeedback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		comments string
	}{
		{"no comments", `[]`},
		{"only a notice", `[{"id":2000,"content":{"raw":"[Mergebot](https://example.com/mergebot) auto-merge is enabled."},"user":{"type":"user","display_name":"example-merge-bot","account_id":"a1b2c3d4"}}]`},
		{"the pipeline's own attestation", `[{"id":2000,"content":{"raw":"<!-- no-mistakes-pipeline-attestation:v1 {} -->"},"user":{"type":"user","display_name":"Rae","account_id":"z"}}]`},
		{"the authenticated user's own comment", `[{"id":2000,"content":{"raw":"a note from the author"},"user":{"type":"user","display_name":"Fake User","account_id":"712020:fake-self"}}]`},
		{"a thread already answered", bbOneRequestAlreadyAnswered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := bbDraftAPI(t, bbDraftPendingGitCheck).withStatuses(bbRunningStatuses).withComments(tc.comments)
			sctx, _ := bbCIDraftContext(t, api, true)
			step := onePollThenCancel(sctx)

			outcome, err := driveCI(t, step, sctx)
			if err != context.Canceled {
				t.Fatalf("outcome = %#v, err = %v, want the monitor to keep waiting", outcome, err)
			}
		})
	}
}

// The whole round while checks are still running, on a draft: the fixer is
// asked once, the thread is answered once, the draft stays a draft, and the
// monitor's later polls do not raise or answer it again.
func TestCIStep_BitbucketAnswersACommentOnceWhileChecksRun(t *testing.T) {
	for _, draft := range []bool{false, true} {
		name := "ready PR"
		if draft {
			name = "draft PR"
		}
		t.Run(name, func(t *testing.T) {
			f := newBBCommentRepairFixture(t, false, bbOneRequestAndANotice, `{"summary":"cover the empty account","code_change_needed":true,"comment_replies":[
				{"comment_id":"2002","action":"addressed","reply":"Covered the empty-account case."}]}`, true)
			f.api.withStatuses(bbRunningStatuses).withMergeability(bbDraftPendingGitCheck)
			f.api.updateState(func(s *bbFakeStateForTest) { s.Draft = draft })
			f.sctx.Env = f.api.Env() // the fake's statuses and merge checks ride the environment
			f.sctx.Config.Providers.Bitbucket.DraftPullRequests = draft

			// Poll until the round has run and a few quiet polls have followed.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.sctx.Ctx = ctx
			polls := 0
			step := &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
				polls++
				if polls >= 4 {
					cancel()
				}
				return ctx.Err()
			}}
			if _, err := driveCI(t, step, f.sctx); err != nil && err != context.Canceled {
				t.Fatalf("CI step error = %v\nlog:\n%s", err, f.log())
			}

			if *f.calls != 1 {
				t.Fatalf("fix agent ran %d times, want exactly one round\nlog:\n%s", *f.calls, f.log())
			}
			if f.remoteHead(t) == f.headSHA {
				t.Fatalf("the repair was not published while checks were running\nlog:\n%s", f.log())
			}
			reply := f.reply(t, 2002)
			if reply == nil || !strings.Contains(reply.Content.Raw, bitbucket.ReplyMarker) {
				t.Fatalf("reply to 2002 = %+v, want one marked reply", reply)
			}
			if got := f.api.logCount("comment create"); got != 1 {
				t.Fatalf("posted %d replies, want exactly one for the thread", got)
			}
			if !f.resolved(2002) {
				t.Fatal("the addressed thread was not resolved")
			}
			if f.reply(t, 2000) != nil {
				t.Fatal("a notice must never be answered")
			}
			if got := f.api.readState(t).Draft; got != draft {
				t.Fatalf("draft = %v after the round, want it unchanged (%v)", got, draft)
			}
			if agentPrompt := *f.prompt; !strings.Contains(agentPrompt, "empty-account") {
				t.Fatalf("fix prompt lacks the comment:\n%s", agentPrompt)
			}
		})
	}
}
