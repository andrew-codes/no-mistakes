package bitbucket

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	selfAccountID = "712020:self"
	whoamiJSON    = `{"apiVersion":"v2","data":{"accountId":"712020:self","name":"Pipeline Owner"}}`
)

// routedCmdFactory answers each twg invocation with the first route whose key
// the full command line contains, and records every command line.
func routedCmdFactory(rec *[]string, routes []route) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		*rec = append(*rec, line)
		response := bbTestResponse{stderr: "unexpected command: " + line, code: 1}
		for _, r := range routes {
			if strings.Contains(line, r.contains) {
				response = r.response
				break
			}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBBHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"BB_TEST_HELPER=1",
			"BB_TEST_STDOUT="+response.stdout,
			"BB_TEST_STDERR="+response.stderr,
		)
		if response.code != 0 {
			cmd.Env = append(cmd.Env, "BB_TEST_EXIT_CODE=1")
		}
		return cmd
	}
}

type route struct {
	contains string
	response bbTestResponse
}

func commentsHost(rec *[]string, commentsJSON string) *Host {
	return New(routedCmdFactory(rec, []route{
		{"whoami", bbTestResponse{stdout: whoamiJSON}},
		{"comment query", bbTestResponse{stdout: commentsJSON}},
		{"comment create", bbTestResponse{stdout: `{"id":900}`}},
		{"comment resolve", bbTestResponse{stdout: `{}`}},
	}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
}

// A trimmed copy of the shapes `twg bb pull-requests comment query` returned
// for a real pull request: a merge-on-green notice from a pipelines bot, inline
// comments and a general summary from the Rovo Dev app, and a general report
// from a person.
const realShapedComments = `[
 {"id":1,"deleted":false,"pending":false,"created_on":"2026-10-19T19:13:12.791453+00:00",
  "content":{"raw":"[Greenflow](http://go/greenflow) acceleration merge on green is enabled for this repo as default."},
  "user":{"type":"user","display_name":"iap-pipelines-bot","nickname":"iap-pipelines-bot","account_id":"638fe69b"},
  "inline":null,"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42/_/diff#comment-1"}}},
 {"id":2,"deleted":false,"pending":false,"created_on":"2026-10-19T19:19:03.672450+00:00",
  "content":{"raw":"##### Code Bugs | Major\n\nWhen the user-scoped Path does not exist, TrimEnd errors out."},
  "user":{"type":"app_user","display_name":"Rovo Dev","account_id":"712020:app","ai_agent":true,"kind":"atlassian_app"},
  "inline":{"from":null,"to":8,"path":"workbench/steps/006-tools-bin-path.sh","start_from":null,"start_to":null},
  "links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42/_/diff#comment-2"}}},
 {"id":3,"deleted":false,"pending":false,
  "content":{"raw":"## :mag: Review Summary\n\n### Comments by severity"},
  "user":{"type":"app_user","display_name":"Rovo Dev","account_id":"712020:app","ai_agent":true},
  "inline":null},
 {"id":4,"deleted":false,"pending":false,"created_on":"2026-10-19T20:00:00+00:00",
  "content":{"raw":"Please also cover the empty-account case."},
  "user":{"type":"user","display_name":"Reviewer Rae","nickname":"Reviewer Rae","account_id":"712020:rae"},
  "inline":null,"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42/_/diff#comment-4"}}}
]`

func TestGetReviewCommentsKeepsInlineAndHumanCommentsAndSkipsNotices(t *testing.T) {
	var rec []string
	h := commentsHost(&rec, realShapedComments)
	comments, err := h.GetReviewComments(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want the bot's inline comment and the person's general one: %+v", len(comments), comments)
	}
	inline, general := comments[0], comments[1]
	if inline.ID != "2" || inline.Path != "workbench/steps/006-tools-bin-path.sh" || inline.Line != 8 || inline.Author != "Rovo Dev" {
		t.Fatalf("inline comment = %+v", inline)
	}
	if !strings.Contains(inline.Body, "TrimEnd errors out") {
		t.Fatalf("inline body = %q", inline.Body)
	}
	if general.ID != "4" || general.Path != "" || general.Line != 0 || general.Author != "Reviewer Rae" {
		t.Fatalf("general comment = %+v", general)
	}
	if general.URL != "https://bitbucket.org/ws/repo/pull-requests/42/_/diff#comment-4" || general.CreatedAt.IsZero() {
		t.Fatalf("general comment metadata = %+v", general)
	}
	query := rec[len(rec)-1]
	for _, want := range []string{"bb pull-requests comment query 42", "--limit 500", "--workspace ws --repo repo", "-o json --output-summary none"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query = %q, want %q", query, want)
		}
	}
}

func TestGetReviewCommentsSkipsEverythingThatIsNotOpenFeedback(t *testing.T) {
	const raw = `{"values":[
 {"id":10,"content":{"raw":"resolved already"},"user":{"type":"user","display_name":"Rae","account_id":"a"},"resolution":{"type":"comment_resolution"}},
 {"id":11,"deleted":true,"content":{"raw":"deleted"},"user":{"type":"user","display_name":"Rae","account_id":"a"}},
 {"id":12,"pending":true,"content":{"raw":"unpublished draft"},"user":{"type":"user","display_name":"Rae","account_id":"a"}},
 {"id":13,"content":{"raw":"a reply, not a request"},"parent":{"id":14},"user":{"type":"user","display_name":"Rae","account_id":"a"}},
 {"id":14,"content":{"raw":"the thread root that is open"},"user":{"type":"user","display_name":"Rae","account_id":"a"},"inline":{"path":"a.go","to":3}},
 {"id":15,"content":{"raw":"my own note"},"user":{"type":"user","display_name":"Pipeline Owner","account_id":"712020:self"}},
 {"id":16,"content":{"raw":"<!-- no-mistakes-pipeline-attestation:v1 {} -->"},"user":{"type":"user","display_name":"Rae","account_id":"a"}},
 {"id":17,"content":{"raw":"   "},"user":{"type":"user","display_name":"Rae","account_id":"a"}},
 {"id":18,"content":{"raw":"a null resolution is open"},"resolution":null,"user":{"type":"user","display_name":"Rae","account_id":"a"},"inline":{"path":"b.go","from":7}}
]}`
	var rec []string
	h := commentsHost(&rec, raw)
	comments, err := h.GetReviewComments(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	var ids []string
	for _, c := range comments {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, ","); got != "14,18" {
		t.Fatalf("comment ids = %s, want 14,18", got)
	}
	if comments[1].Line != 7 {
		t.Fatalf("a comment anchored only to the source side keeps that line, got %+v", comments[1])
	}
}

func TestGetReviewCommentsNeverReturnsAThreadWeAlreadyAnsweredOrAnswersOnce(t *testing.T) {
	// Comment 20 holds a reply with our marker, 21 only a human reply, and 22's
	// marked reply answers a reply (a nested chain), which still answers 22.
	raw := `[
 {"id":20,"content":{"raw":"first"},"user":{"type":"user","display_name":"Rae","account_id":"a"},"inline":{"path":"a.go","to":1}},
 {"id":23,"content":{"raw":"Addressed in abc1234\n\n` + ReplyMarker + `"},"parent":{"id":20},"user":{"type":"user","display_name":"Pipeline Owner","account_id":"712020:self"}},
 {"id":21,"content":{"raw":"second"},"user":{"type":"user","display_name":"Rae","account_id":"a"},"inline":{"path":"a.go","to":2}},
 {"id":24,"content":{"raw":"I disagree"},"parent":{"id":21},"user":{"type":"user","display_name":"Sam","account_id":"s"}},
 {"id":22,"content":{"raw":"third"},"user":{"type":"user","display_name":"Rae","account_id":"a"},"inline":{"path":"a.go","to":3}},
 {"id":25,"content":{"raw":"hm"},"parent":{"id":22},"user":{"type":"user","display_name":"Sam","account_id":"s"}},
 {"id":26,"content":{"raw":"done\n\n` + ReplyMarker + `"},"parent":{"id":25},"user":{"type":"user","display_name":"Pipeline Owner","account_id":"712020:self"}}
]`
	var rec []string
	comments, err := commentsHost(&rec, raw).GetReviewComments(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "21" {
		t.Fatalf("comments = %+v, want only the thread nobody from the pipeline answered", comments)
	}
}

func TestGetReviewCommentsFailsClosed(t *testing.T) {
	pr := &scm.PR{Number: "42"}
	repo := RepoRef{Workspace: "ws", RepoSlug: "repo"}
	for name, routes := range map[string][]route{
		"unreadable whoami":     {{"whoami", bbTestResponse{stdout: `{"data":{}}`}}, {"comment query", bbTestResponse{stdout: "[]"}}},
		"failing whoami":        {{"whoami", bbTestResponse{stderr: "boom", code: 1}}, {"comment query", bbTestResponse{stdout: "[]"}}},
		"failing query":         {{"whoami", bbTestResponse{stdout: whoamiJSON}}, {"comment query", bbTestResponse{stderr: "boom", code: 1}}},
		"non-JSON query":        {{"whoami", bbTestResponse{stdout: whoamiJSON}}, {"comment query", bbTestResponse{stdout: "summary: too large"}}},
		"object without values": {{"whoami", bbTestResponse{stdout: whoamiJSON}}, {"comment query", bbTestResponse{stdout: `{"x":1}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			var rec []string
			h := New(routedCmdFactory(&rec, routes), func() bool { return true }, repo, false)
			if comments, err := h.GetReviewComments(context.Background(), pr); err == nil {
				t.Fatalf("GetReviewComments() = %+v, want an error, never an empty answer", comments)
			}
		})
	}
	h := New(nil, nil, repo, false)
	if _, err := h.GetReviewComments(context.Background(), nil); err == nil {
		t.Fatal("a missing PR must be an error")
	}
}

func TestReplyToReviewCommentPostsAMarkedReplyToTheComment(t *testing.T) {
	var rec []string
	h := commentsHost(&rec, "[]")
	if err := h.ReplyToReviewComment(context.Background(), &scm.PR{Number: "42"}, "2", "  Addressed in abc1234: guarded the null.  "); err != nil {
		t.Fatalf("ReplyToReviewComment() error = %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("recorded %v, want exactly the create call", rec)
	}
	want := []string{"bb pull-requests comment create", "--pull-request 42", "--reply-to 2", "--workspace ws --repo repo", "-o json --output-summary none"}
	for _, w := range want {
		if !strings.Contains(rec[0], w) {
			t.Fatalf("command = %q, want %q", rec[0], w)
		}
	}
	if !strings.Contains(rec[0], "--text Addressed in abc1234: guarded the null.\n\n"+ReplyMarker) {
		t.Fatalf("command = %q, want the trimmed text followed by the reply marker", rec[0])
	}
}

func TestReplyAndResolveRefuseIncompleteIdentity(t *testing.T) {
	var rec []string
	h := commentsHost(&rec, "[]")
	ctx := context.Background()
	for name, err := range map[string]error{
		"reply no pr":      h.ReplyToReviewComment(ctx, nil, "2", "x"),
		"reply no comment": h.ReplyToReviewComment(ctx, &scm.PR{Number: "42"}, " ", "x"),
		"reply empty text": h.ReplyToReviewComment(ctx, &scm.PR{Number: "42"}, "2", "  "),
		"resolve no pr":    h.ResolveReviewComment(ctx, &scm.PR{}, "2"),
		"resolve no id":    h.ResolveReviewComment(ctx, &scm.PR{Number: "42"}, ""),
	} {
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(rec) != 0 {
		t.Fatalf("no twg call may run for incomplete input, got %v", rec)
	}
}

func TestResolveReviewCommentResolvesTheThread(t *testing.T) {
	var rec []string
	h := commentsHost(&rec, "[]")
	if err := h.ResolveReviewComment(context.Background(), &scm.PR{Number: "42"}, "2"); err != nil {
		t.Fatalf("ResolveReviewComment() error = %v", err)
	}
	if len(rec) != 1 || !strings.Contains(rec[0], "bb pull-requests comment resolve --pull-request 42 --comment 2 --workspace ws --repo repo -o json --output-summary none") {
		t.Fatalf("commands = %v", rec)
	}
}

func TestWriteFailuresSurface(t *testing.T) {
	var rec []string
	h := New(routedCmdFactory(&rec, []route{{"comment", bbTestResponse{stderr: "forbidden", code: 1}}}), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
	if err := h.ReplyToReviewComment(context.Background(), &scm.PR{Number: "42"}, "2", "x"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("reply error = %v", err)
	}
	if err := h.ResolveReviewComment(context.Background(), &scm.PR{Number: "42"}, "2"); err == nil || errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("resolve error = %v", err)
	}
}

func TestCapabilitiesDeclareReviewCommentReplies(t *testing.T) {
	caps := New(nil, nil, RepoRef{}, false).Capabilities()
	if !caps.ReviewComments || !caps.ReviewCommentReplies {
		t.Fatalf("capabilities = %+v, want review comments and replies", caps)
	}
}
