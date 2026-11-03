package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A Test step can finish over an approved exception: the configured test
// command failed (or live validation ended no-go or inconclusive) and an
// operator reviewed that and approved the run anyway. The step's findings still
// carry the failure, so rendering them as an ordinary error told a reader of
// the PR that the tests failed and nothing was done about it. The exception is
// rendered instead as what it is: a failure that was reviewed and accepted,
// with the recorded reason and, once known, the final CI result.
//
// The attestation is untouched. It keeps reading StepResult.OverrideReason so a
// consumer's policy decision never depends on this prose.

const (
	maxTestExceptionConditionBytes = 240
	maxTestExceptionReasonBytes    = 480
	maxTestExceptionDetailBytes    = 160
	maxTestExceptionDetails        = 3

	// testExceptionApprovedMarker is where db.StepResult.TestOverrideReason
	// joins the failure condition to the operator's reason.
	testExceptionApprovedMarker = "\nTest exception approved: "
	// noOperatorReason is the placeholder db.StepResult.TestOverrideReason
	// records when the approval carried no reason text.
	noOperatorReason = "no operator reason supplied"
)

// testException is a completed Test step's approved exception.
type testException struct {
	// Condition is what failed, as recorded when the gate parked.
	Condition string
	// Reason is why the failure was accepted; empty when none was recorded.
	Reason string
	// Details are findings that name what failed (suites, tests) beyond the
	// condition itself.
	Details []string
}

// testExceptionFor reports sr's approved exception. It is the Test step's
// completed-over-an-unresolved-condition marker (db.StepResult.TestOverrideReason),
// so a Test step that failed with no approval is never one.
func testExceptionFor(sr *db.StepResult, rounds []*db.StepRound) (testException, bool) {
	if sr == nil {
		return testException{}, false
	}
	full := sr.TestOverrideReason()
	if full == "" {
		return testException{}, false
	}
	condition, reason := full, ""
	if i := strings.Index(full, testExceptionApprovedMarker); i >= 0 {
		condition, reason = full[:i], full[i+len(testExceptionApprovedMarker):]
	}
	ex := testException{
		Condition: exceptionText(condition, maxTestExceptionConditionBytes),
		Reason:    exceptionText(reason, maxTestExceptionReasonBytes),
	}
	if strings.TrimSpace(reason) == noOperatorReason {
		ex.Reason = ""
	}
	if ex.Condition == "" {
		ex.Condition = "the Test step did not pass"
	}
	ex.Details = testExceptionDetails(sr, rounds, ex.Condition)
	return ex, true
}

// exceptionText flattens recorded free text to one bounded line.
func exceptionText(s string, max int) string {
	s = neutralizePRControls(sanitizePromptText(s))
	if len(s) > max {
		s = s[:utf8BoundaryBefore(s, max)] + "..."
	}
	return s
}

// testExceptionDetails lists what the step's findings add to the condition,
// such as the failing suite or test names, from the step's final findings or
// the latest round's.
func testExceptionDetails(sr *db.StepResult, rounds []*db.StepRound, condition string) []string {
	raw := sr.FindingsJSON
	if (raw == nil || strings.TrimSpace(*raw) == "") && len(rounds) > 0 {
		raw = rounds[len(rounds)-1].FindingsJSON
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return nil
	}
	var details []string
	for _, f := range findings.Items {
		detail := exceptionText(f.Description, maxTestExceptionDetailBytes)
		if detail == "" || strings.Contains(condition, detail) || strings.Contains(detail, condition) {
			continue
		}
		duplicate := false
		for _, have := range details {
			duplicate = duplicate || have == detail
		}
		if duplicate {
			continue
		}
		if len(details) == maxTestExceptionDetails {
			break
		}
		details = append(details, detail)
	}
	return details
}

// testExceptionNote renders the one-line explanation of an accepted exception.
// ciPassed adds the final CI result, only when the caller already knows it.
func testExceptionNote(ex testException, ciPassed bool, flavor prBodyFlavor) string {
	var b strings.Builder
	b.WriteString("⚠️ **Accepted exception** - ")
	b.WriteString(escapePRText(ex.Condition, flavor))
	b.WriteString(".")
	for _, detail := range ex.Details {
		b.WriteString(" Failing: ")
		b.WriteString(escapePRText(detail, flavor))
		b.WriteString(".")
	}
	b.WriteString(" The failure was reviewed and accepted.")
	if ex.Reason != "" {
		b.WriteString(` Reason: "`)
		b.WriteString(escapePRText(ex.Reason, flavor))
		b.WriteString(`".`)
	} else {
		b.WriteString(" No reason was recorded.")
	}
	if ciPassed {
		b.WriteString(" CI on the PR head passed.")
	}
	return b.String()
}

// testExceptionStatusLine is the Test step's one-line status when it carries
// an accepted exception: a pass, but never a silent one.
func testExceptionStatusLine(name string) string {
	return fmt.Sprintf("⚠️ **%s** - passed with an accepted exception", name)
}

// testedDetailLabel shortens a multi-line tested command (a whole script) to
// the line that says what ran: the first one that is not blank, a comment or a
// shell option line. lines is the script's length.
func testedDetailLabel(clean string) (label string, lines int) {
	all := strings.Split(clean, "\n")
	lines = len(all)
	for _, line := range all {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "set -") || strings.HasPrefix(line, "set +") {
			continue
		}
		label = line
		break
	}
	if label == "" {
		label = strings.TrimSpace(all[0])
	}
	label = strings.ReplaceAll(label, "`", "'")
	if len(label) > maxTestedLabelBytes {
		label = label[:utf8BoundaryBefore(label, maxTestedLabelBytes)] + "..."
	}
	return label, lines
}

const maxTestedLabelBytes = 120
