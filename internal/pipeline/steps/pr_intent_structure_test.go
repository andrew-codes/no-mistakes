package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// wallOfTextIntent has the shape that read badly on Bitbucket: a long run of
// prose paragraphs, a statement made twice, long semicolon-joined sentences,
// unformatted identifiers, labels, a numbered series, and a pasted snippet.
const wallOfTextIntent = `We are replacing the hand-rolled retry loops in the sync worker with a shared backoff helper. Retries today are scattered across the service and each one picks its own delay.

The ask:

Move every retry loop onto ` + "`backoff.Run`" + `. The helper lives in lib/retry/backoff.ts and reads RETRY_MAX_ATTEMPTS from the environment; callers that pass maxAttempts explicitly keep their own value, and callers that pass nothing get the default of 5.

Move every retry loop onto ` + "`backoff.Run`" + `. The helper lives in lib/retry/backoff.ts. Keep the default at 5 attempts.

Do not retry on 4xx responses, because those never succeed on a second try; do retry on 429 and on any 5xx, using the --max-delay flag to cap the wait.

Constraints:

- Keep the public sync() signature unchanged.
- Do not touch the queue consumer.

The rollout:

Land it in three steps, in this order:

1. Add the helper and its tests in lib/retry/backoff.ts.
2. Switch the sync worker to it.
3. Delete the old loops, then remove the unused retry_count column.

Example call site:

import { run } from './backoff';
const result = await run(task, { maxAttempts: 3 });

Notes: the helper is reviewed/merged separately. This is intentionally small. Follow up on the metrics later.

| step | owner |
| --- | --- |
| helper | platform |

## Background

The old loops date from the first sync worker, and nobody owns them today.`

func TestStructureIntent_ReadsAsAGoalThenPoints(t *testing.T) {
	t.Parallel()
	out := structureIntent(wallOfTextIntent)

	blocks := strings.Split(out, "\n\n")
	if strings.HasPrefix(blocks[0], "- ") || len(blocks[0]) > intentGoalMax {
		t.Fatalf("the intent must open with a short goal statement, got %q", blocks[0])
	}
	if !strings.HasPrefix(blocks[0], "We are replacing the hand-rolled retry loops") {
		t.Fatalf("the goal is the intent's own first statement, got %q", blocks[0])
	}
	bullets := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- ") {
			bullets++
		}
	}
	if bullets < 10 {
		t.Fatalf("expected the asks and constraints as bullets, got %d:\n%s", bullets, out)
	}
	for _, label := range []string{"**The ask:**", "**Constraints:**", "**The rollout:**"} {
		if !strings.Contains(out, "\n"+label+"\n") {
			t.Errorf("missing bold label %s:\n%s", label, out)
		}
	}
}

// No paragraph, goal, bullet or list item may be a wall of text again.
func TestStructureIntent_NoLineExceedsALength(t *testing.T) {
	t.Parallel()
	out := structureIntent(wallOfTextIntent)
	const maxLine = 320
	inFence := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && len(line) > maxLine {
			t.Errorf("line is %d chars, over %d: %q", len(line), maxLine, line)
		}
	}
}

func TestStructureIntent_NoStatementIsRepeated(t *testing.T) {
	t.Parallel()
	out := structureIntent(wallOfTextIntent)
	seen := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		for _, sentence := range splitIntentSentences(strings.TrimPrefix(line, "- ")) {
			key, _ := intentWords(sentence)
			if key == "" {
				continue
			}
			if prev, dup := seen[key]; dup {
				t.Errorf("%q repeats %q", sentence, prev)
			}
			seen[key] = sentence
		}
	}
	// The verbatim repeat and the one fully contained in a longer statement are
	// the two the intent made twice; the sentence between them is new.
	for marker, want := range map[string]int{"Move every retry loop onto": 1, "The helper lives in": 1, "Keep the default at 5 attempts": 1} {
		if n := strings.Count(out, marker); n != want {
			t.Errorf("%q appears %d times, want %d:\n%s", marker, n, want, out)
		}
	}
}

// Merging is only for a statement another one contains in full: a different
// number, identifier or negation is a different requirement.
func TestStructureIntent_DistinctRequirementsAreNeverMerged(t *testing.T) {
	t.Parallel()
	in := `Tune the retry policy for the sync worker as described below.

Set the maximum retry delay for the sync worker queue to 30 seconds when load is high.

Set the maximum retry delay for the sync worker queue to 60 seconds when load is high.

Do retry the sync worker queue on a 429 response status code from the upstream service.

Do not retry the sync worker queue on a 429 response status code from the upstream service.

Use the src/queue/consumer.ts retry delay for the sync worker queue when load is high.

Use the src/queue/producer.ts retry delay for the sync worker queue when load is high.`
	out := structureIntent(in)
	for _, want := range []string{"30 seconds", "60 seconds", "Do retry", "Do not retry", "consumer.ts", "producer.ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("a distinct requirement was merged away, missing %q:\n%s", want, out)
		}
	}
}

// Nothing is dropped and nothing is reordered: every word of the intent is still
// there, and the statements keep the order they were written in.
func TestStructureIntent_KeepsEveryRequirementInOrder(t *testing.T) {
	t.Parallel()
	_, inWords := intentWords(wallOfTextIntent)
	_, outWords := intentWords(strings.ReplaceAll(structureIntent(wallOfTextIntent), "`", ""))
	for w := range inWords {
		if !outWords[w] {
			t.Errorf("word %q of the intent is missing from the structured intent", w)
		}
	}

	out := structureIntent(wallOfTextIntent)
	plain := strings.NewReplacer("`", "", "**", "").Replace(out)
	last := -1
	for _, marker := range []string{
		"hand-rolled retry loops",
		"Retries today are scattered",
		"backoff.Run",
		"RETRY_MAX_ATTEMPTS",
		"Do not retry on 4xx",
		"--max-delay",
		"public sync() signature",
		"queue consumer",
		"1. Add the helper",
		"2. Switch the sync worker",
		"3. Delete the old loops",
		"Example call site",
		"maxAttempts: 3",
		"Follow up on the metrics",
		"| helper | platform |",
		"The old loops date from",
	} {
		at := strings.Index(plain, marker)
		if at < 0 {
			t.Errorf("missing %q:\n%s", marker, out)
			continue
		}
		if at < last {
			t.Errorf("%q moved ahead of an earlier statement:\n%s", marker, out)
		}
		last = at
	}
}

func TestStructureIntent_BlocksStayIntact(t *testing.T) {
	t.Parallel()
	out := structureIntent(wallOfTextIntent)
	for _, want := range []string{
		// an ordered list stays ordered and in order
		"1. Add the helper and its tests in `lib/retry/backoff.ts`.\n2. Switch the sync worker to it.\n3. Delete the old loops, then remove the unused `retry_count` column.",
		// a pasted snippet becomes a column-0 code block, line for line
		"```text\nimport { run } from './backoff';\nconst result = await run(task, { maxAttempts: 3 });\n```",
		// a table is kept as written
		"| step | owner |\n| --- | --- |\n| helper | platform |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing block:\n%s\nin:\n%s", want, out)
		}
	}
	// A heading inside the intent must not become a section of the description.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") {
			t.Errorf("a heading survived into the intent: %q", line)
		}
	}
	if !strings.Contains(out, "**Background**") {
		t.Errorf("the heading should have become a bold label:\n%s", out)
	}
}

func TestStructureIntent_FormatsIdentifiersAsCode(t *testing.T) {
	t.Parallel()
	out := structureIntent(wallOfTextIntent)
	for _, want := range []string{
		"`lib/retry/backoff.ts`", "`RETRY_MAX_ATTEMPTS`", "`maxAttempts`", "`--max-delay`", "`sync()`", "`retry_count`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("identifier not in code formatting: %s\n%s", want, out)
		}
	}
	// "reviewed/merged" is prose, not a path.
	if strings.Contains(out, "`reviewed/merged`") {
		t.Errorf("a prose word pair was formatted as code:\n%s", out)
	}
}

func TestCodeFormatIntent(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Edit src/app/main.ts and nx.json.":                    "Edit `src/app/main.ts` and `nx.json`.",
		"Set MAX_RETRIES or retry_count.":                      "Set `MAX_RETRIES` or `retry_count`.",
		"Call fetchUser() then parseResponse.":                 "Call `fetchUser()` then `parseResponse`.",
		"Pass --dry-run and run yarn test:unit.":               "Pass `--dry-run` and run yarn `test:unit`.",
		"Already `src/a.ts` stays.":                            "Already `src/a.ts` stays.",
		"See [the docs/guide.md page](https://x.test/a/b.md).": "See [the docs/guide.md page](https://x.test/a/b.md).",
		"Open https://example.test/a/b.json now.":              "Open https://example.test/a/b.json now.",
		"Use and/or pass/fail, JavaScript and GitHub.":         "Use and/or pass/fail, JavaScript and GitHub.",
		"Runs on Node.js and the team's tooling.":              "Runs on Node.js and the team's tooling.",
		"Add .cjs and .mjs tests.":                             "Add `.cjs` and `.mjs` tests.",
	} {
		if got := codeFormatIntent(in); got != want {
			t.Errorf("codeFormatIntent(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestStructureIntent_ShortIntentIsOnlyCodeFormatted(t *testing.T) {
	t.Parallel()
	in := "Rename the retry_count column to attempts. Nothing else changes."
	if got, want := structureIntent(in), "Rename the `retry_count` column to attempts. Nothing else changes."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := structureIntent("   \n"); got != "" {
		t.Errorf("blank intent = %q", got)
	}
}

func TestSplitIntentSentences(t *testing.T) {
	t.Parallel()
	got := splitIntentSentences("Use a helper, e.g. the shared one. It lives in v1.2/lib.ts and reads `a. b`. Done (see step 2. then 3.) now! project.json changes too.")
	want := []string{
		"Use a helper, e.g. the shared one.",
		"It lives in v1.2/lib.ts and reads `a. b`.",
		"Done (see step 2. then 3.) now!",
		"project.json changes too.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A long statement is split only at a place that cannot change its meaning; one
// without such a place stays whole rather than becoming fragments.
func TestSplitIntentLong(t *testing.T) {
	t.Parallel()
	long := "Retry transient failures for the sync worker with a capped exponential delay between attempts and a small random jitter so that workers do not stampede; give up after the configured maximum and surface the last error to the caller of the helper."
	got := splitIntentLong(long)
	if len(got) != 2 || !strings.HasSuffix(got[0], "stampede") || !strings.HasPrefix(got[1], "Give up after") {
		t.Errorf("semicolon split = %q", got)
	}
	compound := "The package.json scripts test:lint-extra and test:smoke-extra, and the pipeline steps that called them in the bitbucket pipelines file, are removed so that nothing runs twice in the pipeline on any branch at all."
	if got := splitIntentLong(compound); len(got) != 2 || !strings.HasPrefix(got[1], "so that") {
		// the only safe split is the ", so " consequence clause; the compound subject stays whole
		if len(got) != 1 && !strings.Contains(got[0], "and the pipeline steps that called them") {
			t.Errorf("a compound subject was torn apart: %q", got)
		}
	}
	noSafeSplit := strings.Repeat("a long clause with no safe place to break it, ", 8) + "end."
	if got := splitIntentLong(noSafeSplit); len(got) != 1 {
		t.Errorf("a statement with no safe split must stay whole, got %d parts", len(got))
	}
}

// Only Bitbucket Cloud restructures the intent; every other host publishes the
// cleaned intent as written.
func TestPublicPRIntentFor_OnlyBitbucketRestructures(t *testing.T) {
	t.Parallel()
	sctx := newBitbucketDocContext(t)
	if got := publicPRIntentFor(sctx, scm.ProviderGitHub); got != bitbucketDocIntent {
		t.Errorf("github intent was changed:\n%s", got)
	}
	bb := publicPRIntentFor(sctx, scm.ProviderBitbucket)
	if bb == bitbucketDocIntent || !strings.Contains(bb, "\n- ") {
		t.Errorf("bitbucket intent was not restructured:\n%s", bb)
	}
}
