package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// heredocTestCommand is the shape observed on rovo-chat-desktop PRs: a
// configured test command that is a whole indented heredoc script.
const heredocTestCommand = "bash <<'SCRIPT'\n" +
	"set -uo pipefail\n" +
	"lock_dir=\"${NM_TEST_LOCK_DIR:-/tmp/lock}\"\n" +
	"lock_stale() {\n" +
	"  local holder dir=\"${1:-$lock_dir}\"\n" +
	"  if [ -z \"$holder\" ]; then\n" +
	"    [ -n \"$(find \"$dir\" -maxdepth 0 -mmin +1)\" ]\n" +
	"  fi\n" +
	"}\n" +
	"until mkdir \"$lock_dir\" 2>/dev/null; do\n" +
	"  sleep 5\n" +
	"done\n" +
	"SCRIPT"

func testStepWithTested(t *testing.T, tested ...string) ([]*db.StepResult, map[string][]*db.StepRound) {
	t.Helper()
	raw, err := json.Marshal(types.Findings{Tested: tested, Summary: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	findings := string(raw)
	return []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}},
		map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 100}}}
}

// assertBitbucketScriptBullet checks the Bitbucket Cloud rules for a script in
// a list item: nothing but the marker's label on the bullet line, a blank
// line, then an indented fence whose every script line keeps real newlines
// and its own indentation.
func assertBitbucketScriptBullet(t *testing.T, md string) {
	t.Helper()
	if strings.Contains(md, "- ```") {
		t.Fatalf("fence must not share a line with the bullet marker:\n%s", md)
	}
	if !strings.Contains(md, "- Ran:\n\n    ```bash\n    bash <<'SCRIPT'\n    set -uo pipefail\n") {
		t.Fatalf("expected an indented bash fence with real newlines under the bullet, got:\n%s", md)
	}
	// Indentation inside the script survives (4 list spaces + 2 script spaces).
	if !strings.Contains(md, "\n      local holder dir=") || !strings.Contains(md, "\n        sleep 5\n") && !strings.Contains(md, "\n      sleep 5\n") {
		t.Fatalf("script indentation was lost:\n%s", md)
	}
	if !strings.Contains(md, "\n    SCRIPT\n    ```\n") {
		t.Fatalf("heredoc terminator must stay on its own line inside the fence:\n%s", md)
	}
	if strings.Contains(md, "bash <<'SCRIPT' set -uo") {
		t.Fatalf("heredoc was joined onto one line:\n%s", md)
	}
	// No bare language word outside a fence.
	for _, line := range strings.Split(md, "\n") {
		if strings.TrimSpace(line) == "text" || strings.TrimSpace(line) == "bash" {
			t.Fatalf("stray language word %q outside a fence:\n%s", line, md)
		}
	}
}

func TestBitbucketPipelineSummary_HeredocScriptIsAnIndentedFencedBullet(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand, "`bash scripts/test-workbench-nvm.sh`")
	md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)
	assertBitbucketScriptBullet(t, md)
	if !strings.Contains(md, "- `bash scripts/test-workbench-nvm.sh`\n") {
		t.Fatalf("single-line commands stay inline code bullets:\n%s", md)
	}
}

func TestBitbucketTestingSummary_HeredocScriptIsAnIndentedFencedBullet(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand)
	md := buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: prBodyMarkdown, includeTestedDetails: true})
	assertBitbucketScriptBullet(t, md)
}

func TestTestedDetailRenderingKeepsHTMLProvidersOnInlineCode(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand)
	md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub)
	if strings.Contains(md, "- Ran:") || !strings.Contains(md, "<code>bash &lt;&lt;&#39;SCRIPT&#39;&#10;set -uo pipefail&#10;") {
		t.Fatalf("html providers must keep the <code> rendering, got:\n%s", md)
	}
}

func TestTestedDetailLanguage(t *testing.T) {
	t.Parallel()
	for detail, want := range map[string]string{
		heredocTestCommand:             "bash",
		"yarn jest\n  --ci":            "bash",
		"#!/bin/sh\necho hi":           "bash",
		"Checked the `login` page\nok": "text",
	} {
		if got := testedDetailLanguage(detail); got != want {
			t.Errorf("testedDetailLanguage(%q) = %q, want %q", detail, got, want)
		}
	}
}

func TestSanitizeTestedDetailKeepsIndentationAndNewlines(t *testing.T) {
	t.Parallel()
	got := sanitizeTestedDetail("\r\nif x; then  \r\n\techo hi\r\nfi\r\n\r\n")
	if want := "if x; then\n    echo hi\nfi"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
