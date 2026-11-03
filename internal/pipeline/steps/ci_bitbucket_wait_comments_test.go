package steps

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	bbNoStatuses      = `[]`

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

// bbWaitingContext is a CI step context for an open Bitbucket PR whose
// statuses are the given JSON and whose comments are the given thread list.
func bbWaitingContext(t *testing.T, statuses, comments string) *pipeline.StepContext {
	t.Helper()
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withStatuses(statuses).
		withComments(comments)
	dir, baseSHA, headSHA := setupGitRepo(t)
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	return sctx
}

// onePollThenCancel lets the monitor poll once and then end the step with the
// context error, so a monitor that would otherwise wait forever fails the test
// instead of hanging it.
func onePollThenCancel(sctx *pipeline.StepContext) *CIStep {
	ctx, cancel := context.WithCancel(sctx.Ctx)
	sctx.Ctx = ctx
	return &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}
}

// A comment that arrives while the monitor is waiting becomes an auto-fix
// finding on the next poll, whatever the wait is for.
func TestCIStep_BitbucketRaisesCommentsWhileWaiting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		statuses string
	}{
		{"checks still running", bbRunningStatuses},
		{"no checks registered yet", bbNoStatuses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx := bbWaitingContext(t, tc.statuses, bbOneRequestAndANotice)
			step := onePollThenCancel(sctx)

			outcome, err := driveCI(t, step, sctx)
			if err != nil {
				t.Fatalf("CI step error = %v", err)
			}
			if outcome == nil || outcome.CIReadyNoCI != nil || outcome.Findings == "" {
				t.Fatalf("outcome = %#v, want the comment raised while waiting", outcome)
			}
			if ids := commentFindingIDs(t, outcome.Findings); len(ids) != 1 || ids[0] != "2002" {
				t.Fatalf("raised comments = %v, want only the request (2002), never the notice", ids)
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
			sctx := bbWaitingContext(t, bbRunningStatuses, tc.comments)
			step := onePollThenCancel(sctx)

			outcome, err := driveCI(t, step, sctx)
			if err != context.Canceled {
				t.Fatalf("outcome = %#v, err = %v, want the monitor to keep waiting", outcome, err)
			}
		})
	}
}
