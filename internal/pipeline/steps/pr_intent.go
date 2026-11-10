package steps

import (
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// publicPRIntent returns the intent text published on the PR body, or "" when
// publication is suppressed. It is the single publication gate: every PR-body
// render path (ordinary drafting, fallback content, and template appendix)
// goes through it, so caller-side omission and the repository's trusted
// policy compose here and nowhere else.
//
// The control is tighten-only in both directions:
//
//   - The repository's trusted pr.publish_intent (default true) is the
//     ceiling. A caller can never publish intent on a repository whose
//     trusted config disabled it.
//   - The caller-side decision recorded on the run (runs.omit_intent: the
//     axi run --no-publish-intent flag, or the operator's global
//     intent.publish_intent: false default, folded together at run start)
//     can only remove the section, never restore it.
//
// The repository policy never touches the intent that reaches step prompts.
// The caller-side omission is different: it also WITHHOLDS the intent from the
// PR-drafting turns (prDraftIntentPromptSection), because a PR title or body
// drafted with the intent in context can paraphrase it into the public text,
// and there is deliberately no output filter or prose scanner to catch that.
// Review, test, document, lint, and CI-fix prompts keep the full intent
// under either signal.
func publicPRIntent(sctx *pipeline.StepContext) string {
	return publicPRIntentFor(sctx, resolvedProviderForBody(sctx))
}

// publicPRIntentFor is publicPRIntent rendered for provider. A configured
// brief replaces the cleaned intent outright; otherwise Bitbucket Cloud gets
// the intent restructured into scannable points (structureIntent) and every
// other host publishes the cleaned intent as written.
func publicPRIntentFor(sctx *pipeline.StepContext, provider scm.Provider) string {
	source := publicPRIntentSource(sctx, provider)
	if source == "" {
		return ""
	}
	if sctx.Config != nil && strings.EqualFold(strings.TrimSpace(sctx.Config.PR.Intent.Format), "brief") {
		return strings.TrimSpace(sctx.PublicIntentBrief)
	}
	return source
}

func publicPRIntentSource(sctx *pipeline.StepContext, provider scm.Provider) string {
	if sctx != nil && sctx.Config != nil && !sctx.Config.PR.PublishesIntent() {
		return ""
	}
	if runOmitsIntent(sctx) {
		return ""
	}
	cleaned := cleanedUserIntent(sctx)
	if prBodyFlavorFor(provider) == prBodyMarkdown {
		return structureIntent(cleaned)
	}
	return cleaned
}

// runOmitsIntent reports whether this run was started with the caller-side,
// tighten-only request to keep the generated Intent section out of the PR
// body. The decision is stamped on the run row at start, so it survives
// daemon restarts and reruns.
func runOmitsIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Run != nil && sctx.Run.OmitIntent
}

// prDraftIntentPromptSection is the intent section for the PR-drafting turns
// (ordinary narrative, title-only fallback, and repository-template
// narrative). Under the caller-side omission it is empty, so those turns
// draft from the diff and commit messages only and no intent text can reach
// the public PR through a paraphrase. Every other step prompt keeps
// userIntentPromptSection unchanged.
func prDraftIntentPromptSection(sctx *pipeline.StepContext) string {
	if runOmitsIntent(sctx) {
		return ""
	}
	return userIntentPromptSection(sctx)
}

// pinPublishedIntent anchors the PR-facing intent to the one the PR was
// opened with. A run that updates an existing PR carries its own intent - the
// agent's restatement of the latest request ("rebase onto main", "fix the
// failing test") - and that is right for the run's review, test and fix
// prompts, but it is a correction to the work, not the purpose of the PR.
// Rendering it would overwrite the reviewer-facing Intent section with the
// last thing that went wrong, so the PR step swaps the intent back to the
// earliest one recorded against this PR for the duration of the step.
//
// Precedence: the first intent recorded for the PR wins over every later run's,
// whether that later intent was inferred, supplied with `axi run --intent`, or
// inherited by a rerun. Nothing in the pipeline can tell a changed goal from an
// agent describing a correction, so the change is a deliberate signal instead:
// a run started with `--replace-intent` (intent source db.RunIntentSourceReplace)
// publishes its own intent and, from then on, is the anchor that later plain
// runs pin to. Tighten-only publication controls (runs.omit_intent,
// pr.publish_intent) are untouched and still read from the current run.
//
// The anchor lookup failing keeps the run's own intent, matching a PR that
// predates the anchor. The returned func restores the context and is safe to
// call when nothing was pinned.
func pinPublishedIntent(sctx *pipeline.StepContext, pr *scm.PR) func() {
	noop := func() {}
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || pr == nil {
		return noop
	}
	// The run that decided to replace the anchor has not been recorded against
	// the PR yet when it first publishes; it publishes its own intent.
	if sctx.IntentSource == db.RunIntentSourceReplace && strings.TrimSpace(sctx.UserIntent) != "" {
		return noop
	}
	prURL := strings.TrimSpace(pr.URL)
	if prURL == "" {
		prURL = runPRURL(sctx)
	}
	if prURL == "" {
		return noop
	}
	anchor, err := sctx.DB.GetOriginalPRIntent(sctx.Repo.ID, prURL)
	if err != nil {
		slog.Warn("failed to read original PR intent; publishing this run's intent", "run", sctx.Run.ID, "err", err)
		return noop
	}
	if anchor == nil || strings.TrimSpace(anchor.Summary) == "" {
		return noop
	}
	prevIntent, prevSource := sctx.UserIntent, sctx.IntentSource
	if strings.TrimSpace(prevIntent) != strings.TrimSpace(anchor.Summary) && sctx.Log != nil {
		sctx.Log("keeping the pull request's original intent in its description; this run's intent applies to its own steps only")
	}
	sctx.UserIntent, sctx.IntentSource = anchor.Summary, anchor.Source
	return func() {
		sctx.UserIntent, sctx.IntentSource = prevIntent, prevSource
	}
}
