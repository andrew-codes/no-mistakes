package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	pushedHead  = "0123456789abcdef0123456789abcdef01234567"
	otherHead   = "fedcba9876543210fedcba9876543210fedcba98"
	headPRHref  = "https://bitbucket.org/ws/repo/pull-requests/42"
	headPRState = "OPEN"
)

// headPR renders a `twg bb pull-requests get` payload. Each override replaces
// one field wholesale so a test names exactly what it breaks.
func headPR(t *testing.T, overrides map[string]string) string {
	t.Helper()
	parts := map[string]string{
		"id":     `42`,
		"state":  `"` + headPRState + `"`,
		"links":  fmt.Sprintf(`{"html":{"href":%q}}`, headPRHref),
		"source": fmt.Sprintf(`{"branch":{"name":"feature"},"repository":{"full_name":"ws/repo"},"commit":{"hash":%q}}`, pushedHead[:12]),
	}
	for k, v := range overrides {
		if v == "" {
			delete(parts, k)
			continue
		}
		parts[k] = v
	}
	var fields []string
	for k, v := range parts {
		fields = append(fields, fmt.Sprintf("%q:%s", k, v))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

func verifyHead(response bbTestResponse, branch, head string) (error, []capturedCmd) {
	var rec []capturedCmd
	h := baseHost(&rec, response)
	err := h.VerifyPRHead(context.Background(), &scm.PR{Number: "42", URL: headPRHref}, branch, head)
	return err, rec
}

func TestVerifyPRHeadProvesTheAbbreviatedAndTheFullSourceCommit(t *testing.T) {
	for name, hash := range map[string]string{
		"twg's 12 character abbreviation": pushedHead[:12],
		"a longer prefix":                 pushedHead[:20],
		"the full object ID":              pushedHead,
		"upper case hex":                  strings.ToUpper(pushedHead[:12]),
	} {
		t.Run(name, func(t *testing.T) {
			source := fmt.Sprintf(`{"branch":{"name":"feature"},"repository":{"full_name":"ws/repo"},"commit":{"hash":%q}}`, hash)
			err, rec := verifyHead(bbTestResponse{stdout: headPR(t, map[string]string{"source": source})}, "feature", pushedHead)
			if err != nil {
				t.Fatalf("VerifyPRHead() = %v, want a positive proof", err)
			}
			if want := "bb pull-requests get 42 --workspace ws --repo repo -o json --output-summary none"; len(rec) != 1 || !strings.Contains(joined(rec[0]), want) {
				t.Fatalf("commands = %v, want exactly one fresh read by PR number (%q)", rec, want)
			}
		})
	}
}

// A source commit that is not the pushed head is reported as a head change, so
// the step can wait out the forge's propagation delay before it refuses.
func TestVerifyPRHeadReportsAMovedHeadAsAHeadChange(t *testing.T) {
	source := fmt.Sprintf(`{"branch":{"name":"feature"},"repository":{"full_name":"ws/repo"},"commit":{"hash":%q}}`, otherHead[:12])
	err, _ := verifyHead(bbTestResponse{stdout: headPR(t, map[string]string{"source": source})}, "feature", pushedHead)
	var changed *scm.PRHeadChangedError
	if !errors.As(err, &changed) || !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("VerifyPRHead() = %v, want a *PRHeadChangedError", err)
	}
	if changed.HeadSHA != otherHead[:12] {
		t.Fatalf("live head = %q, want the abbreviation the forge returned", changed.HeadSHA)
	}
}

// Everything the forge does not positively state is unproven, never assumed.
func TestVerifyPRHeadIsNeverProvenFromIncompleteEvidence(t *testing.T) {
	src := func(branch, repo, commit string) string {
		parts := []string{}
		if branch != "" {
			parts = append(parts, fmt.Sprintf(`"branch":{"name":%q}`, branch))
		}
		if repo != "" {
			parts = append(parts, fmt.Sprintf(`"repository":{"full_name":%q}`, repo))
		}
		if commit != "" {
			parts = append(parts, fmt.Sprintf(`"commit":{"hash":%q}`, commit))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	tests := []struct {
		name     string
		response bbTestResponse
		branch   string
		head     string
	}{
		{"no source object", bbTestResponse{stdout: headPR(t, map[string]string{"source": ""})}, "feature", pushedHead},
		{"no source commit", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "ws/repo", "")})}, "feature", pushedHead},
		{"blank source commit", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "ws/repo", "   ")})}, "feature", pushedHead},
		{"source commit too short to identify anything", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "ws/repo", pushedHead[:7])})}, "feature", pushedHead},
		{"source commit that is not hex", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "ws/repo", "zzzzzzzzzzzz")})}, "feature", pushedHead},
		{"source commit longer than the pushed head", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "ws/repo", pushedHead+"00")})}, "feature", pushedHead},
		{"no source branch", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("", "ws/repo", pushedHead[:12])})}, "feature", pushedHead},
		{"other source branch", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("other", "ws/repo", pushedHead[:12])})}, "feature", pushedHead},
		{"no source repository", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "", pushedHead[:12])})}, "feature", pushedHead},
		{"source repository of another workspace", bbTestResponse{stdout: headPR(t, map[string]string{"source": src("feature", "someone/repo", pushedHead[:12])})}, "feature", pushedHead},
		{"merged PR", bbTestResponse{stdout: headPR(t, map[string]string{"state": `"MERGED"`})}, "feature", pushedHead},
		{"declined PR", bbTestResponse{stdout: headPR(t, map[string]string{"state": `"DECLINED"`})}, "feature", pushedHead},
		{"no state", bbTestResponse{stdout: headPR(t, map[string]string{"state": ""})}, "feature", pushedHead},
		{"different PR number", bbTestResponse{stdout: headPR(t, map[string]string{"id": "7"})}, "feature", pushedHead},
		{"no PR number", bbTestResponse{stdout: headPR(t, map[string]string{"id": ""})}, "feature", pushedHead},
		{"no web URL", bbTestResponse{stdout: headPR(t, map[string]string{"links": ""})}, "feature", pushedHead},
		{"web URL of another repository", bbTestResponse{stdout: headPR(t, map[string]string{"links": `{"html":{"href":"https://bitbucket.org/ws/other/pull-requests/42"}}`})}, "feature", pushedHead},
		{"web URL of another PR", bbTestResponse{stdout: headPR(t, map[string]string{"links": `{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/7"}}`})}, "feature", pushedHead},
		{"twg failure", bbTestResponse{stderr: "twg bb: the API is down", code: 1}, "feature", pushedHead},
		{"twg output summary instead of JSON", bbTestResponse{stdout: "summary: output too large to print inline"}, "feature", pushedHead},
		{"malformed JSON", bbTestResponse{stdout: "{nope"}, "feature", pushedHead},
		{"empty expected branch", bbTestResponse{stdout: headPR(t, nil)}, "", pushedHead},
		{"abbreviated pushed head", bbTestResponse{stdout: headPR(t, nil)}, "feature", pushedHead[:12]},
		{"empty pushed head", bbTestResponse{stdout: headPR(t, nil)}, "feature", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err, _ := verifyHead(tt.response, tt.branch, tt.head)
			if err == nil {
				t.Fatal("VerifyPRHead() = nil, want the head to stay unproven")
			}
			var changed *scm.PRHeadChangedError
			if errors.As(err, &changed) {
				t.Fatalf("VerifyPRHead() = %v: only a source commit that positively differs is a head change", err)
			}
		})
	}
}

func TestVerifyPRHeadRefusesAnIncompletePR(t *testing.T) {
	var rec []capturedCmd
	h := baseHost(&rec, bbTestResponse{stdout: headPR(t, nil)})
	for name, pr := range map[string]*scm.PR{
		"nil":            nil,
		"no number":      {URL: headPRHref},
		"bad number":     {Number: "abc"},
		"negative":       {Number: "-4"},
		"zero":           {Number: "0"},
		"fractional one": {Number: "4.2"},
	} {
		if err := h.VerifyPRHead(context.Background(), pr, "feature", pushedHead); err == nil {
			t.Fatalf("%s: VerifyPRHead() = nil, want a refusal", name)
		}
	}
	if len(rec) != 0 {
		t.Fatalf("an unusable identity ran %d twg commands, want none", len(rec))
	}
}

func TestBitbucketHostVerifiesThePushedHead(t *testing.T) {
	var _ scm.PRHeadVerifier = (*Host)(nil)
	if _, ok := any(New(nil, nil, RepoRef{}, false)).(scm.PRHeadVerifier); !ok {
		t.Fatal("the Bitbucket host must implement scm.PRHeadVerifier so PR bodies can state a proven head")
	}
}
