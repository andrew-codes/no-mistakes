package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// These tests pin the STRUCTURE of a Bitbucket Cloud description rather than
// its wording. Bitbucket takes the description as Markdown only (the REST
// field is a plain string whose summary.markup is "markdown", and twg's
// --description-file reads Markdown verbatim), so the document's flow has to
// come from headings, lists and blocks, and each of those has to survive
// Bitbucket's renderer: it recognizes a fence only in column 0 and does not
// fold anything.

type docSection struct {
	heading string
	lines   []string
}

// docSections splits a description at its level-2 headings. Text before the
// first heading has the empty heading.
func docSections(body string) []docSection {
	sections := []docSection{{}}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			sections = append(sections, docSection{heading: strings.TrimPrefix(line, "## ")})
			continue
		}
		last := &sections[len(sections)-1]
		last.lines = append(last.lines, line)
	}
	if len(sections[0].lines) == 0 && sections[0].heading == "" {
		sections = sections[1:]
	}
	return sections
}

func docSectionNamed(t *testing.T, body, heading string) docSection {
	t.Helper()
	for _, s := range docSections(body) {
		if s.heading == heading {
			return s
		}
	}
	t.Fatalf("description has no %q section:\n%s", heading, body)
	return docSection{}
}

func TestBitbucketDescription_LeadsWithWhatChangedThenIntent(t *testing.T) {
	t.Parallel()
	body := bitbucketDocBody(t, newBitbucketDocContext(t))

	var headings []string
	for _, s := range docSections(body) {
		headings = append(headings, s.heading)
	}
	want := []string{"What Changed", "Intent", "Testing", "Pipeline"}
	if strings.Join(headings, "|") != strings.Join(want, "|") {
		t.Fatalf("section order = %v, want %v:\n%s", headings, want, body)
	}
	if !strings.HasPrefix(body, "## What Changed\n\n- ") {
		t.Fatalf("the description must open with the what-changed summary, not the intent:\n%s", body)
	}
	// The intent is the author's own text and stays verbatim, paragraphs intact.
	intent := strings.Join(docSectionNamed(t, body, "Intent").lines, "\n")
	if !strings.Contains(intent, "The ask:\n\nWrite script guidelines for agents") {
		t.Fatalf("intent paragraphs were not preserved:\n%s", intent)
	}
}

func TestBitbucketDescription_PipelineIsAnOverviewListThenGroups(t *testing.T) {
	t.Parallel()
	body := bitbucketDocBody(t, newBitbucketDocContext(t))
	pipelineSection := docSectionNamed(t, body, "Pipeline")

	var overview []string
	firstHeading := -1
	for i, line := range pipelineSection.lines {
		if strings.HasPrefix(line, "### ") && firstHeading < 0 {
			firstHeading = i
		}
		if strings.HasPrefix(line, "- ") && firstHeading < 0 {
			overview = append(overview, line)
		}
	}
	steps := []string{"Intent", "Rebase", "Review", "Test", "Document", "Lint", "Push"}
	if len(overview) != len(steps) {
		t.Fatalf("overview has %d bullets, want one per step (%d):\n%s", len(overview), len(steps), strings.Join(pipelineSection.lines, "\n"))
	}
	for i, name := range steps {
		if !strings.Contains(overview[i], "**"+name+"** - ") {
			t.Errorf("overview bullet %d = %q, want step %s", i, overview[i], name)
		}
	}

	// A step's status line appears once, as a bullet or as the heading of its
	// own group, and never as a bare paragraph: those ran together as
	// consecutive one-line paragraphs, and every one after the Test group
	// belonged to that heading.
	for _, line := range pipelineSection.lines {
		if strings.Contains(line, "** - ") && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "### ") && !strings.HasPrefix(line, "**Run outcome:**") && !strings.Contains(line, "Accepted exception") {
			t.Errorf("step status line outside the list or a heading: %q", line)
		}
	}
	if firstHeading < 0 {
		t.Fatal("the Test step has a group of its own and must get a heading")
	}
	for _, line := range pipelineSection.lines[firstHeading:] {
		for _, later := range []string{"**Document**", "**Lint**", "**Push**"} {
			if strings.Contains(line, later) {
				t.Errorf("%s is listed under the Test group's heading: %q", later, line)
			}
		}
	}
}

func TestBitbucketDescription_TestEvidenceAppearsOnce(t *testing.T) {
	t.Parallel()
	body := bitbucketDocBody(t, newBitbucketDocContext(t))
	for _, marker := range []string{
		"**Accepted exception**",
		"Live validation:",
		"| Scenario | Result | Live | Evidence |",
		"Ported tests are discovered and pass under standard Jest",
	} {
		if n := strings.Count(body, marker); n != 1 {
			t.Errorf("%q appears %d times, want once (Testing carries it, Pipeline must not repeat it):\n%s", marker, n, body)
		}
	}
	// What the Testing section does not list stays where it was: in compact
	// mode the tested commands are only in the Pipeline's Test group.
	group := strings.Join(docSectionNamed(t, body, "Pipeline").lines, "\n")
	for _, want := range []string{
		"- `yarn exec jest scripts/build-tools` (7 suites, 56 tests, all passing)\n",
		"- Ran `bash <<'SCRIPT'` (5-line script)\n",
	} {
		if !strings.Contains(group, want) {
			t.Errorf("Pipeline lost the tested command %q:\n%s", want, group)
		}
	}
}

func TestBitbucketDescription_NoFenceIsNestedUnderAListItem(t *testing.T) {
	t.Parallel()
	body := bitbucketDocBody(t, newBitbucketDocContext(t))
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "```") && !strings.HasPrefix(line, "```") {
			t.Errorf("fence is not in column 0, so Bitbucket renders it as a broken inline code span: %q", line)
		}
	}
	if strings.Contains(body, "Ran:\n") {
		t.Errorf("a tested command is still introduced by a bare \"Ran:\" label:\n%s", body)
	}
}

// With no Testing section in the description (none was produced, or it was
// the one the body budget dropped), the Pipeline's Test group is the only home
// of the verdict and scenario table and keeps them.
func TestBitbucketPipeline_TestGroupKeepsEvidenceWhenTestingDoesNotCoverIt(t *testing.T) {
	t.Parallel()
	ctx := newBitbucketDocContext(t)
	steps, err := ctx.DB.GetStepsByRun(ctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds := map[string][]*db.StepRound{}
	for _, sr := range steps {
		r, err := ctx.DB.GetRoundsByStep(sr.RoundsStepID())
		if err != nil {
			t.Fatal(err)
		}
		rounds[sr.ID] = r
	}

	render := func(covers testingCoverage) string {
		md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket, pipelineAttestationPolicy{Final: true, CIPassed: true, TestingCovers: covers})
		return md
	}
	uncovered := render(testingCoverage{})
	for _, want := range []string{"**Accepted exception**", "Live validation:", "| Scenario | Result | Live | Evidence |", "(5-line script)"} {
		if !strings.Contains(uncovered, want) {
			t.Errorf("uncovered Test group is missing %q:\n%s", want, uncovered)
		}
	}
	covered := render(testingCoverage{Verdict: true})
	for _, gone := range []string{"**Accepted exception**", "Live validation:", "| Scenario |"} {
		if strings.Contains(covered, gone) {
			t.Errorf("covered Test group repeats %q:\n%s", gone, covered)
		}
	}
	if !strings.Contains(covered, "(5-line script)") {
		t.Errorf("tested commands are not covered by the Testing section and must stay:\n%s", covered)
	}
	if got := render(testingCoverage{Verdict: true, Commands: true}); strings.Contains(got, "5-line script") {
		t.Errorf("commands the Testing section lists must not repeat:\n%s", got)
	}
}

// Findings are not Testing-section content: covering the verdict must never
// hide a real, unaccepted failure from the Pipeline.
func TestBitbucketPipeline_CoveredTestStillShowsUnacceptedFindings(t *testing.T) {
	t.Parallel()
	steps, rounds := unresolvedFailureTestStep()
	md, _ := buildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket, pipelineAttestationPolicy{Final: true, TestingCovers: testingCoverage{Verdict: true, Commands: true}})
	for _, want := range []string{"### ", "**Test** - 1 error", "🚨 " + exceptionCondition} {
		if !strings.Contains(md, want) {
			t.Fatalf("an unaccepted failure must stay in the Pipeline, missing %q:\n%s", want, md)
		}
	}
}

func TestPlaceIntentSection(t *testing.T) {
	t.Parallel()
	const intent = "## Intent\n\nwhy"
	body := "## What Changed\n\n- a\n\n## Risk Assessment\n\nlow\n\n## Pipeline\n\n- x"
	for _, tc := range []struct {
		name     string
		provider scm.Provider
		body     string
		want     string
	}{
		{"github leads", scm.ProviderGitHub, body, intent + "\n\n" + body},
		{"bitbucket follows the narrative", scm.ProviderBitbucket, body, "## What Changed\n\n- a\n\n" + intent + "\n\n## Risk Assessment\n\nlow\n\n## Pipeline\n\n- x"},
		{"bitbucket narrative only", scm.ProviderBitbucket, "## What Changed\n\n- a", "## What Changed\n\n- a\n\n" + intent},
		{"bitbucket no narrative", scm.ProviderBitbucket, "## Pipeline\n\n- x", intent + "\n\n## Pipeline\n\n- x"},
		{"bitbucket empty", scm.ProviderBitbucket, "  \n", intent},
	} {
		if got := placeIntentSection(tc.body, intent, tc.provider); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The end-of-run refresh strips every generated section from the live body and
// rebuilds it. The new order must survive that round trip unchanged.
func TestBitbucketDescription_RefreshRebuildsTheSameDocument(t *testing.T) {
	t.Parallel()
	sctx := newBitbucketDocContext(t)
	body := bitbucketDocBody(t, sctx)

	narrative := stripGeneratedSections(body)
	if narrative != bitbucketDocWhatChanged {
		t.Fatalf("stripping the generated sections must leave only the narrative, got:\n%s", narrative)
	}
	pipelineMD, riskLine, testingMD := (&PRStep{}).buildPipelineSectionWith(sctx, scm.ProviderBitbucket, false, true)
	if again := assembleDraftPRBody(sctx, narrative, riskLine, testingMD, pipelineMD, 0, scm.ProviderBitbucket); again != body {
		t.Fatalf("rebuilding from the stripped body changed the document:\n--- first\n%s\n--- again\n%s", body, again)
	}
}

// Under a body budget the order holds and the Pipeline section still survives.
func TestBitbucketDescription_OrderHoldsUnderABodyBudget(t *testing.T) {
	t.Parallel()
	sctx := newBitbucketDocContext(t)
	pipelineMD, riskLine, testingMD := (&PRStep{}).buildPipelineSectionWith(sctx, scm.ProviderBitbucket, false, true)
	full := assembleDraftPRBody(sctx, bitbucketDocWhatChanged, riskLine, testingMD, pipelineMD, 0, scm.ProviderBitbucket)
	limit := scm.PRBodyLen(full) - scm.PRBodyLen(testingMD) + 10
	body := assembleDraftPRBody(sctx, bitbucketDocWhatChanged, riskLine, testingMD, pipelineMD, limit, scm.ProviderBitbucket)
	if scm.PRBodyLen(body) > limit {
		t.Fatalf("body is %d chars, over its %d budget", scm.PRBodyLen(body), limit)
	}
	var headings []string
	for _, s := range docSections(body) {
		headings = append(headings, s.heading)
	}
	if got, want := strings.Join(headings, "|"), "What Changed|Intent|Pipeline"; got != want {
		t.Fatalf("section order under budget = %q, want %q (Testing is the section shed first):\n%s", got, want, body)
	}
}
