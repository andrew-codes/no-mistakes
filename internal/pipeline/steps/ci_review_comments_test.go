package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const bbReviewPRURL = "https://bitbucket.org/test/repo/pull-requests/42"

// Comments shaped like the ones twg returns for a real Bitbucket PR: a
// merge-on-green notice and an app review summary the monitor must ignore, and
// two requests it must answer - one inline, one general from a person.
const bbReviewCommentsJSON = `[
 {"id":2000,"content":{"raw":"[Greenflow](http://go/greenflow) acceleration merge on green is enabled for this repo as default."},
  "user":{"type":"user","display_name":"iap-pipelines-bot","nickname":"iap-pipelines-bot","account_id":"638fe69b"}},
 {"id":2001,"content":{"raw":"Guard the null path before TrimEnd."},
  "user":{"type":"app_user","display_name":"Rovo Dev","account_id":"712020:app","ai_agent":true},
  "inline":{"from":null,"to":8,"path":"feature.txt"}},
 {"id":2002,"content":{"raw":"Please also cover the empty-account case."},
  "user":{"type":"user","display_name":"Reviewer Rae","nickname":"Reviewer Rae","account_id":"712020:rae"}},
 {"id":2003,"content":{"raw":"## :mag: Review Summary"},
  "user":{"type":"app_user","display_name":"Rovo Dev","account_id":"712020:app","ai_agent":true}}
]`

// bbCommentRepairFixture is a Bitbucket PR whose checks are green but which
// carries unresolved review comments, with a fix agent that returns the given
// conclusion after optionally editing the worktree.
type bbCommentRepairFixture struct {
	*ciRepairFixture
	api    *fakeBitbucketAPI
	prompt *string
	calls  *int
}

func newBBCommentRepairFixture(t *testing.T, revalidate bool, comments, conclusion string, edit bool) *bbCommentRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, revalidate, nil)
	api := newFakeBitbucketAPI(t, 42, bbReviewPRURL).
		withPRState("OPEN").
		withSourceCommit(f.headSHA).
		withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`).
		withComments(comments)
	f.sctx.Env = api.Env()
	prURL := bbReviewPRURL
	f.sctx.Run.PRURL = &prURL
	f.sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	var prompt string
	calls := 0
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		prompt = opts.Prompt
		if edit {
			writeCIFix(opts.CWD)
		}
		return &agent.Result{Output: []byte(conclusion)}, nil
	}}
	return &bbCommentRepairFixture{ciRepairFixture: f, api: api, prompt: &prompt, calls: &calls}
}

func (f *bbCommentRepairFixture) reply(t *testing.T, parent int) *bbCommentForTest {
	t.Helper()
	for _, c := range f.api.comments() {
		c := c
		if c.Parent != nil && c.Parent.ID == parent {
			return &c
		}
	}
	return nil
}

func (f *bbCommentRepairFixture) resolved(id int) bool {
	for _, c := range f.api.comments() {
		if c.ID == id {
			return len(c.Resolution) > 0 && string(c.Resolution) != "null"
		}
	}
	return false
}

func TestCIStep_BitbucketAddressesAndRepliesToReviewComments(t *testing.T) {
	f := newBBCommentRepairFixture(t, false, bbReviewCommentsJSON, `{"summary":"guard the null path","code_change_needed":true,"comment_replies":[
		{"comment_id":"2001","action":"addressed","reply":"Guarded the null path before TrimEnd."},
		{"comment_id":"2002","action":"declined","reply":"The empty-account case is covered by the existing account test."}]}`, true)

	outcome, err := f.run(t)
	if err != nil && err != context.Canceled {
		t.Fatalf("CI step error = %v\nlog:\n%s", err, f.log())
	}
	_ = outcome
	if *f.calls != 1 {
		t.Fatalf("fix agent ran %d times, want exactly one round\nlog:\n%s", *f.calls, f.log())
	}
	if f.remoteHead(t) == f.headSHA {
		t.Fatalf("the comment repair was not published\nlog:\n%s", f.log())
	}
	short := f.localHead(t)[:7]

	addressed := f.reply(t, 2001)
	if addressed == nil {
		t.Fatalf("no reply was posted to comment 2001: %+v", f.api.comments())
	}
	if want := "Addressed in " + short + ": Guarded the null path before TrimEnd."; !strings.HasPrefix(addressed.Content.Raw, want) {
		t.Fatalf("addressed reply = %q, want prefix %q", addressed.Content.Raw, want)
	}
	if !strings.Contains(addressed.Content.Raw, bitbucket.ReplyMarker) {
		t.Fatalf("reply %q lacks the marker that stops it being answered twice", addressed.Content.Raw)
	}
	if !f.resolved(2001) {
		t.Fatal("the addressed comment's thread was not resolved")
	}

	declined := f.reply(t, 2002)
	if declined == nil || !strings.HasPrefix(declined.Content.Raw, "No code change made: The empty-account case is covered by the existing account test.") {
		t.Fatalf("declined reply = %+v, want the reason", declined)
	}
	if f.resolved(2002) {
		t.Fatal("a declined comment's thread must stay open for the reviewer")
	}

	for _, ignored := range []int{2000, 2003} {
		if f.reply(t, ignored) != nil {
			t.Fatalf("comment %d is a notice and must never be answered", ignored)
		}
	}
	if got := f.api.logCount("comment create"); got != 2 {
		t.Fatalf("posted %d replies, want exactly one per handled comment (monitoring must not reply again)", got)
	}
	if !strings.Contains(*f.prompt, "untrusted") || !strings.Contains(*f.prompt, `"comment_id":"2001"`) {
		t.Fatalf("fix prompt must frame the comment text as untrusted and name its comment_id:\n%s", *f.prompt)
	}
	if !strings.Contains(*f.prompt, "Guard the null path before TrimEnd.") || strings.Contains(*f.prompt, "Greenflow") {
		t.Fatalf("fix prompt must carry the human/bot comments and not the notice:\n%s", *f.prompt)
	}
}

func TestCIStep_BitbucketHeldRepairDoesNotClaimItIsAddressed(t *testing.T) {
	f := newBBCommentRepairFixture(t, true, bbReviewCommentsJSON, `{"summary":"guard the null path","code_change_needed":true,"comment_replies":[
		{"comment_id":"2001","action":"addressed","reply":"Guarded the null path."},
		{"comment_id":"2002","action":"declined","reply":"Already covered."}]}`, true)

	outcome, err := f.run(t)
	assertCIRestartsValidation(t, outcome, err)
	if f.remoteHead(t) != f.headSHA {
		t.Fatal("a held repair must not be published")
	}
	if f.reply(t, 2001) != nil || f.resolved(2001) {
		t.Fatalf("a repair that is not on the PR yet must not be reported as addressed: %+v", f.api.comments())
	}
	if declined := f.reply(t, 2002); declined == nil || !strings.HasPrefix(declined.Content.Raw, "No code change made: Already covered.") {
		t.Fatalf("a declined comment is answered as soon as the decision is made, got %+v", declined)
	}
}

func TestCIStep_BitbucketNoCodeChangeDeclinesEveryCommentAndParks(t *testing.T) {
	f := newBBCommentRepairFixture(t, false, bbReviewCommentsJSON, `{"summary":"both comments describe behavior that is already correct","code_change_needed":false,"comment_replies":[
		{"comment_id":"2001","action":"addressed","reply":"Handled."},
		{"comment_id":"2002","action":"declined","reply":"Already covered by the account test."}]}`, false)

	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("CI step error = %v\nlog:\n%s", err, f.log())
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("a no-change conclusion parks for a decision, got %+v", outcome)
	}
	for _, id := range []int{2001, 2002} {
		reply := f.reply(t, id)
		if reply == nil || !strings.HasPrefix(reply.Content.Raw, "No code change made: ") {
			t.Fatalf("comment %d reply = %+v, want a decline: nothing changed, so nothing was addressed", id, reply)
		}
		if f.resolved(id) {
			t.Fatalf("comment %d was resolved although no code changed", id)
		}
	}
	parked, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range parked.Items {
		if item.Category == types.FindingCategoryCIReviewComment && item.ActionOrDefault() != types.ActionAskUser {
			t.Fatalf("parked comment finding %+v must be ask-user", item)
		}
	}
}

func TestCIStep_BitbucketRepliesOnlyAfterTheFixerProducedAnything(t *testing.T) {
	// code_change_needed is true but the agent changed nothing: the fix failed,
	// so nothing may be claimed on the PR and the comments stay open.
	f := newBBCommentRepairFixture(t, false, bbReviewCommentsJSON, `{"summary":"tried","code_change_needed":true,"comment_replies":[
		{"comment_id":"2001","action":"addressed","reply":"Guarded."}]}`, false)
	if _, err := f.run(t); err != nil && err != context.Canceled {
		t.Fatalf("CI step error = %v", err)
	}
	if got := f.api.logCount("comment create"); got != 0 {
		t.Fatalf("posted %d replies for a round that changed nothing", got)
	}
}

func TestCIStep_BitbucketIgnoresNoticesAndOwnRecords(t *testing.T) {
	const notices = `[
 {"id":1,"content":{"raw":"[Greenflow](http://go/greenflow) merge on green is enabled"},"user":{"type":"user","display_name":"iap-pipelines-bot","account_id":"x"}},
 {"id":2,"content":{"raw":"## :mag: Review Summary"},"user":{"type":"app_user","display_name":"Rovo Dev","account_id":"y"}},
 {"id":3,"content":{"raw":"<!-- no-mistakes-pipeline-attestation:v1 {} -->"},"user":{"type":"user","display_name":"Rae","account_id":"z"}},
 {"id":4,"content":{"raw":"a note from the author"},"user":{"type":"user","display_name":"Fake User","account_id":"712020:fake-self"}}
]`
	f := newBBCommentRepairFixture(t, false, notices, `{"summary":"x","code_change_needed":false,"comment_replies":[]}`, false)
	if _, err := f.run(t); err != nil && err != context.Canceled {
		t.Fatalf("CI step error = %v", err)
	}
	if *f.calls != 0 {
		t.Fatalf("the fix agent ran %d times over comments that are only notices", *f.calls)
	}
	if got := f.api.logCount("comment create"); got != 0 {
		t.Fatalf("posted %d replies to notices", got)
	}
}

func TestCIStep_BitbucketUnreadableCommentsParkInsteadOfPassing(t *testing.T) {
	f := newBBCommentRepairFixture(t, false, `[]`, `{}`, false)
	f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_BB_COMMENTS_FAIL=1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx
	step := &CIStep{waitForNextPoll: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	outcome, err := driveCI(t, step, f.sctx)
	if err != nil {
		t.Fatalf("CI step error = %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("an unreadable comment list must park, not report checks-passed: %+v", outcome)
	}
	parked, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(parked.Items) == 0 {
		t.Fatalf("parked findings = %+v, %v", parked, err)
	}
	if got := parked.Items[0]; got.Category != types.FindingCategoryCIReviewComment || !strings.Contains(got.Description, "review comments could not be read") {
		t.Fatalf("parked finding = %+v, want the review-comment read failure", got)
	}
}

func TestCIObservationFindingsTurnsReviewCommentsIntoAutoFixFindings(t *testing.T) {
	findings := ciObservationFindings(ciIssues{
		provider: scm.ProviderBitbucket,
		reviewComments: []scm.ReviewComment{
			{ID: "7", Author: "Rae", Path: "a.go", Line: 12, Body: "  rename this  "},
			{ID: "7", Author: "Rae", Body: "duplicate id is dropped"},
			{ID: "", Author: "Rae", Body: "no identity cannot be answered"},
			{ID: "8", Author: "Sam", Body: "general remark"},
		},
	})
	if len(findings.Items) != 2 {
		t.Fatalf("items = %+v, want one per distinct comment", findings.Items)
	}
	first := findings.Items[0]
	if first.Category != types.FindingCategoryCIReviewComment || first.Action != types.ActionAutoFix || first.CommentID != "7" ||
		first.File != "a.go" || first.Line != 12 || first.Description != "Rae: rename this" {
		t.Fatalf("first finding = %+v", first)
	}
	if findings.Items[1].CommentID != "8" || findings.Items[1].File != "" {
		t.Fatalf("second finding = %+v", findings.Items[1])
	}
	if findings.Summary != "2 unresolved review comments to address" {
		t.Fatalf("summary = %q", findings.Summary)
	}
	outcome := ciObservationOutcome(findings)
	if !outcome.AutoFixable {
		t.Fatalf("comment findings enter the auto-fix loop: %+v", outcome)
	}

	// CommentID must survive the JSON round trip the executor puts findings through.
	raw, _ := json.Marshal(findings)
	parsed, err := types.ParseFindingsJSON(string(raw))
	if err != nil || parsed.Items[0].CommentID != "7" {
		t.Fatalf("round trip lost the comment id: %+v, %v", parsed.Items, err)
	}
}

func TestReviewCommentFindingsAreBounded(t *testing.T) {
	var comments []scm.ReviewComment
	for i := 1; i <= maxReviewBotCommentFindings+10; i++ {
		comments = append(comments, scm.ReviewComment{ID: fmt.Sprint(i), Author: "Rae", Body: "fix " + fmt.Sprint(i)})
	}
	items := reviewCommentFindings(comments)
	if len(items) != maxReviewBotCommentFindings+1 {
		t.Fatalf("got %d items, want the bound plus one omission marker", len(items))
	}
	marker := items[len(items)-1]
	if marker.CommentID != "" || marker.Action != types.ActionAskUser || !strings.Contains(marker.Description, "10 more unresolved review comments") {
		t.Fatalf("omission marker = %+v, want an unanswerable ask-user note", marker)
	}
	long := reviewCommentFindings([]scm.ReviewComment{{ID: "1", Body: strings.Repeat("x", maxReviewBotCommentBytes*2)}})
	if len(long[0].Description) > maxReviewBotCommentBytes+64 {
		t.Fatalf("a long comment must be truncated, got %d bytes", len(long[0].Description))
	}
}

func TestReviewCommentSchemaRequiresDecisionsOnlyForCommentRounds(t *testing.T) {
	var plain, withComments struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(ciFixSchema(false), &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ciFixSchema(true), &withComments); err != nil {
		t.Fatal(err)
	}
	if strings.Join(plain.Required, ",") != "summary,code_change_needed" {
		t.Fatalf("non-comment schema changed: %v", plain.Required)
	}
	if strings.Join(withComments.Required, ",") != "summary,code_change_needed,comment_replies" {
		t.Fatalf("comment schema = %v", withComments.Required)
	}
}

// Providers without review-comment replies keep the review-bot-only behavior:
// no comment read, no finding, no reply.
func TestUnansweredReviewCommentsIsInertWithoutTheCapability(t *testing.T) {
	step := &CIStep{}
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	comments, err := step.unansweredReviewComments(sctx, noReplyHost{}, &scm.PR{Number: "1"})
	if err != nil || comments != nil {
		t.Fatalf("unansweredReviewComments() = %v, %v, want nothing", comments, err)
	}
	// A repair round over comment findings against such a host replies to nothing.
	step.respondToReviewComments(sctx, noReplyHost{}, &scm.PR{Number: "1"}, ciFixTargets{Findings: Findings{Items: []Finding{{
		Category: types.FindingCategoryCIReviewComment, CommentID: "9", Action: types.ActionAutoFix,
	}}}}, ciRepairResult{HeadAdvanced: true})
}

type noReplyHost struct{ scm.Host }

func (noReplyHost) Capabilities() scm.Capabilities { return scm.Capabilities{ReviewComments: true} }
