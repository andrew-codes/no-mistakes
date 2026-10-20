package bitbucket

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const prWithDestination = `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/42"}},"source":{"branch":{"name":"feature"},"repository":{"full_name":"ws/repo"}},"destination":{"branch":{"name":"develop"}}}`

func baseHost(rec *[]capturedCmd, response bbTestResponse) *Host {
	return New(capturingCmdFactory(rec, response), func() bool { return true }, RepoRef{Workspace: "ws", RepoSlug: "repo"}, false)
}

func joined(c capturedCmd) string { return strings.Join(c.args, " ") }

func TestFindPRReportsTheLiveBaseBranch(t *testing.T) {
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: "[" + prWithDestination + "]"})
	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil || pr == nil {
		t.Fatalf("FindPR() = %v, %v", pr, err)
	}
	if pr.BaseBranch != "develop" {
		t.Fatalf("BaseBranch = %q, want the destination the listing reported", pr.BaseBranch)
	}
}

func TestGetPRBaseBranchReadsTheDestination(t *testing.T) {
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: prWithDestination})
	base, err := h.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"})
	if err != nil || base != "develop" {
		t.Fatalf("GetPRBaseBranch() = %q, %v, want develop", base, err)
	}
	if want := "bb pull-requests get 42 --workspace ws --repo repo -o json --output-summary none"; !strings.Contains(joined(rec[0]), want) {
		t.Fatalf("command = %q, want %q", joined(rec[0]), want)
	}
}

func TestGetPRBaseBranchFailsRatherThanReturningAnEmptyBase(t *testing.T) {
	for name, response := range map[string]bbTestResponse{
		"no destination":  {stdout: `{"id":42,"state":"OPEN"}`},
		"blank name":      {stdout: `{"id":42,"destination":{"branch":{"name":"  "}}}`},
		"cli failure":     {stderr: "boom", code: 1},
		"not JSON":        {stdout: "summary: large"},
		"malformed JSON":  {stdout: "{nope"},
		"destination nil": {stdout: `{"id":42,"destination":null}`},
	} {
		t.Run(name, func(t *testing.T) {
			var rec []capturedCmd
			h := baseHost(&rec, response)
			if base, err := h.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatalf("GetPRBaseBranch() = %q, want an error", base)
			}
		})
	}
	var rec []capturedCmd
	if _, err := baseHost(&rec, bbTestResponse{}).GetPRBaseBranch(context.Background(), &scm.PR{}); err == nil {
		t.Fatal("a PR without a number must be refused")
	}
	if len(rec) != 0 {
		t.Fatalf("no twg call may run without a PR number, got %v", rec)
	}
}

func TestSetPRBaseBranchUpdatesOnlyTheDestination(t *testing.T) {
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: strings.Replace(prWithDestination, "develop", "main", 1)})
	if err := h.SetPRBaseBranch(context.Background(), &scm.PR{Number: "42"}, " main "); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
	want := "bb pull-requests update --pull-request 42 --dest main --workspace ws --repo repo -o json --output-summary none"
	if len(rec) != 1 || !strings.Contains(joined(rec[0]), want) {
		t.Fatalf("commands = %v, want %q", rec, want)
	}
	for _, unwanted := range []string{"--title", "--description"} {
		if strings.Contains(joined(rec[0]), unwanted) {
			t.Fatalf("a retarget must not touch the title or description: %q", joined(rec[0]))
		}
	}
}

func TestSetPRBaseBranchRefusesWhatItCannotConfirm(t *testing.T) {
	ctx := context.Background()
	pr := &scm.PR{Number: "42"}

	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: prWithDestination}) // still develop
	if err := h.SetPRBaseBranch(ctx, pr, "main"); err == nil || !strings.Contains(err.Error(), "still targets develop") {
		t.Fatalf("a PR that kept its old destination must not read as retargeted, got %v", err)
	}

	h = baseHost(&rec, bbTestResponse{stderr: "branch not found", code: 1})
	if err := h.SetPRBaseBranch(ctx, pr, "main"); err == nil || !strings.Contains(err.Error(), "branch not found") {
		t.Fatalf("a failed update must surface twg's message, got %v", err)
	}

	// An echo that names no destination is accepted: twg confirmed by exit code.
	h = baseHost(&rec, bbTestResponse{stdout: `{"id":42}`})
	if err := h.SetPRBaseBranch(ctx, pr, "main"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}

	rec = nil
	for name, err := range map[string]error{
		"no pr":     h.SetPRBaseBranch(ctx, nil, "main"),
		"no number": h.SetPRBaseBranch(ctx, &scm.PR{}, "main"),
		"no branch": h.SetPRBaseBranch(ctx, pr, "  "),
	} {
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(rec) != 0 {
		t.Fatalf("no twg call may run for incomplete input, got %v", rec)
	}
}
