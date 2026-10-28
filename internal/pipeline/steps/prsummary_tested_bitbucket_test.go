package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// heredocTestCommand is a shape seen on real PRs: a
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

// assertScriptIsLabelled checks that a recorded script is reduced to a short
// label and never dumped: no fence, no script body, just what ran and how long
// the script was.
func assertScriptIsLabelled(t *testing.T, md string) {
	t.Helper()
	if !strings.Contains(md, "- Ran `bash <<'SCRIPT'` (13-line script)\n") {
		t.Fatalf("expected a one-line label with the script length, got:\n%s", md)
	}
	for _, leaked := range []string{"```", "set -uo pipefail", "lock_stale()", "sleep 5", "SCRIPT\n", "&#10;"} {
		if strings.Contains(md, leaked) {
			t.Fatalf("the script body must not be dumped into the description (found %q):\n%s", leaked, md)
		}
	}
}

func TestBitbucketPipelineSummary_HeredocScriptIsAShortLabel(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand, "`bash scripts/test-setup.sh`")
	md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)
	assertScriptIsLabelled(t, md)
	if !strings.Contains(md, "- `bash scripts/test-setup.sh`\n") {
		t.Fatalf("single-line commands stay inline code bullets:\n%s", md)
	}
}

func TestBitbucketTestingSummary_HeredocScriptIsAShortLabel(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand)
	md := buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: prBodyMarkdown, includeTestedDetails: true})
	assertScriptIsLabelled(t, md)
}

func TestTestedDetailRenderingLabelsScriptsForHTMLProvidersToo(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithTested(t, heredocTestCommand)
	md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub)
	assertScriptIsLabelled(t, md)
}

func TestTestedDetailLabel(t *testing.T) {
	t.Parallel()
	long := "yarn jest " + strings.Repeat("--flag ", 40)
	for name, tc := range map[string]struct {
		detail    string
		wantLabel string
		wantLines int
	}{
		"heredoc opener":              {heredocTestCommand, "bash <<'SCRIPT'", 13},
		"skips shebang and set lines": {"#!/bin/bash\nset -euo pipefail\n\nyarn jest --ci\nyarn lint", "yarn jest --ci", 5},
		"backticks become quotes":     {"echo `date`\nmake test", "echo 'date'", 2},
		"long first line is bounded":  {long + "\nnext", strings.TrimSpace(long)[:maxTestedLabelBytes] + "...", 2},
	} {
		label, lines := testedDetailLabel(tc.detail)
		if lines != tc.wantLines || label != tc.wantLabel {
			t.Errorf("%s: testedDetailLabel = (%q, %d), want (%q, %d)", name, label, lines, tc.wantLabel, tc.wantLines)
		}
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
