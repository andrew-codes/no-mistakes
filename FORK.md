# What this fork changes

This is [andrew-codes/no-mistakes](https://github.com/andrew-codes/no-mistakes), a fork of [kunchenguid/no-mistakes](https://github.com/kunchenguid/no-mistakes) (the upstream). It is maintained by Andrew Smith. It exists to carry work that upstream has not taken, mostly Bitbucket Cloud support through the `twg` CLI, plus a few pipeline fixes. Nothing here is pushed to or opened against upstream from this fork's PRs. The fork's PRs target the fork's `main` directly, because the no-mistakes pipeline always targets upstream.

This file lists only what differs from upstream. For how each feature works, follow the links to the docs pages that the fork edited.

**Last synced upstream commit:** `63c01829173f45ba3c755beaa64930244aec924e` ("fix(shellenv): scope macOS native completion inspection to its process group (#1753)"), merged into fork `main` on 2026-11-10 through fork PR #19. The signed annotated tag `upstream-synced` on the fork marks it (tag object `ffabf72c`, tagged 2026-11-10 09:50 -0500). Upstream `main` was at `7f7f5720` ("fix(github): keep stderr warnings out of CI JSON responses (#1754)") when this file was written, so one upstream commit is not yet synced. Run `git log upstream-synced..origin/main` for the current gap.

How this file was checked: the claims below come from the fork-only commits (`git log origin/main..fork/main`, 9 commits at the time of writing), the fork PR descriptions (#16 to #23), the diff against the synced upstream commit, and a read of the code on fork `main`. Nothing was run against a real Bitbucket workspace. Where a feature has only been tested against the fork's fake `twg`, the table says so.

## Summary

| Feature | What it does | Status | Where it lives |
| --- | --- | --- | --- |
| Bitbucket Cloud provider via `twg` | PR create/update/find, CI polling, failed-log fetch, merge readiness, base-branch retarget, merged-PR lookup | Tested against a fake `twg` only; unverified on real Bitbucket | `internal/scm/bitbucket/`, `internal/pipeline/steps/ci_bitbucket.go` |
| Bitbucket review comments | Unresolved comments become CI auto-fix findings; the step replies and resolves threads | Fake `twg` only | `internal/scm/bitbucket/review_comments.go`, `internal/pipeline/steps/ci_review_comments.go` |
| Bitbucket draft-then-publish | PR is a draft until the run proves it, then marked ready | Fake `twg` only | `internal/scm/bitbucket/bitbucket.go`, `internal/pipeline/steps/pr_draft.go` |
| Bitbucket exact head proof | PR step publishes only against a PR proven to be at the pushed head | Fake `twg` only | `internal/scm/bitbucket/pr_head.go`, `internal/pipeline/steps/pr_head.go` |
| Bitbucket stopped-pipeline re-run | Re-runs a `STOPPED` pipeline before the CI step escalates | Fake `twg` only; whether Bitbucket accepts `rerun-failed` on a stopped pipeline is unverified | `internal/scm/bitbucket/rerun.go` |
| Bitbucket evidence images | Run screenshots embedded in the PR description, opt-in | Fake `twg` only; image rendering and same-name re-upload unverified | `internal/scm/bitbucket/evidence.go`, `internal/pipeline/steps/evidence_bitbucket.go` |
| Merged-PR duplicate guard | No second PR when the branch's PR already merged | GitHub and GitLab hosts plus Bitbucket; fake CLIs | `internal/pipeline/steps/pr_merged.go`, `FindMergedPRs` in the three hosts |
| Better PR descriptions | Description refreshed at run end, Test exceptions explained, Intent as points, carried-over results | Stable (unit tested) | `pr_refresh.go`, `prsummary_test_exception.go`, `pr_intent_structure.go`, `prsummary_carryover.go` under `internal/pipeline/steps/` |
| PR intent anchoring | Description's Intent stays the original intent; `--replace-intent` changes it | Stable (unit tested) | `internal/pipeline/steps/pr_intent.go`, `internal/db/run.go`, `internal/cli/` |
| CI monitor resume after daemon restart | A clean stop leaves an idle CI monitor resumable | Stable in tests; not exercised against a live PR and real restart | `internal/pipeline/executor.go`, `internal/daemon/manager.go` |
| Upstream sync bookkeeping | `upstream-synced` tag and a merge-commit sync procedure | Process, not code | git tag, fork PRs #19 and #22 |

The Bitbucket features all depend on the `twg` CLI being installed and authenticated (`twg login`). no-mistakes never holds a Bitbucket token. The fork has no REST client; upstream's `internal/bitbucket` REST package is deleted here.

## Features

### Bitbucket Cloud through `twg`

Fork PR #16 ("Add Bitbucket Cloud support"). Upstream has its own REST-token Bitbucket client (`internal/bitbucket`); the fork replaced it with a host that shells out to `twg bb ...`, in the same posture as the `gh`, `glab` and `tea` hosts. Credentials stay in `twg` (`~/.config/twg/auth.conf`), so the old env-var tokens that leaked into agent subprocess environments are gone. An equivalent upstream-facing change is open as [kunchenguid/no-mistakes#1215](https://github.com/kunchenguid/no-mistakes/pull/1215).

What it covers, all in `internal/scm/bitbucket/bitbucket.go` unless noted:

- PR create, update and find. Every JSON read passes `--output-summary none` so `twg` output parses. `FindPR` requires the source repository to match, because `twg` matches by branch name alone.
- Pipeline status polling, failed-step logs with a bounded tail, and merged-PR lookup (`twg bb pull-requests query --state MERGED`).
- Merge readiness from Bitbucket's own merge checks (`twg bb pull-requests mergeability`): a failed `git_mergeability_check` is a conflict, and a failing blocking `maximum_commits_behind` takes the rebase-and-revalidate repair. Capabilities declared: `MergeableState`, `FailedCheckLogs`, `ReviewComments`, `ReviewCommentReplies`.
- Reading and changing a PR's destination branch (`pull-requests get`, `update --dest`), so `--base-branch` retargets the open PR.
- Read-only PR appendix verification (`pr_repository_content.go`), reimplemented on `twg` during the sync in PR #19 because upstream's version called the REST client.

Known limits: no review-bot check findings (Bitbucket statuses carry no app identity), no explicit issue closure, no pre-run (setup) failure detection, and no fork PR routing (a configured fork URL makes PR creation skip). The provider guide lists these under "What you don't get (yet)".

Docs: [Provider integration, Bitbucket Cloud](docs/src/content/docs/guides/provider-integration.md), plus installation and quick-start mentions of `twg`.

### Bitbucket review comments

PR #16. Each unresolved comment, human or bot, inline or general, read with `twg bb pull-requests comment query`, becomes a `ci-review-comment` finding (`types.FindingCategoryCIReviewComment`, new `Finding.CommentID` field). The CI fix agent addresses it, then the step replies with `comment create --reply-to` and resolves the thread if the code changed. A comment the agent declines gets a reply and stays open. Merge-on-green notices, bot summaries, the pipeline's own attestation and comments by the authenticated `twg` user are ignored, and a thread is never answered twice. New scm contract pieces: `Capabilities.ReviewCommentReplies` and the `ReviewCommentResponder` interface in `internal/scm/host.go`. Other providers keep upstream's review-bot-only, ask-user behavior.

Docs: [Provider integration](docs/src/content/docs/guides/provider-integration.md), [CI step in pipeline steps](docs/src/content/docs/reference/pipeline-steps.md).

### Bitbucket draft-then-publish

PR #16. The PR step opens a Bitbucket PR as a draft and puts an existing PR back to draft when a new run starts validating new content. A run that changed the PR marks it ready only when every step passed (no approval override), checks are green, no review comment awaits a reply, there is no merge conflict, and the description was regenerated for the final state. Bitbucket does not evaluate a draft's mergeability, so the CI monitor does not wait on it while the PR is a draft. New contract: the `PRDraftController` interface.

Configure: `providers.bitbucket.draft_pull_requests`, default `true` here (upstream's default is `false`, and upstream does not manage draft state). Set `false` to open PRs ready and never toggle. It is a global default that per-repo config can override. Docs: [global-config `providers.bitbucket.draft_pull_requests`](docs/src/content/docs/reference/global-config.md).

### Bitbucket exact head proof

PR #20. Upstream's rule that the PR step publishes only against a proven pushed head applies to providers implementing `scm.PRHeadVerifier`. The fork's Bitbucket host now does: `VerifyPRHead` does a fresh `twg bb pull-requests get` and passes only if the PR is open, in this repository, from the pushed branch, at a source commit that is a hex prefix (12 characters or more) of the pushed head. Bitbucket reports a 12-character abbreviation, so this is weaker than GitHub's full-ID check. A positively different commit returns `*scm.PRHeadChangedError` so the step waits out propagation; anything else fails the step with nothing published. A run with `--skip push` has no head to prove, so the Bitbucket PR step refuses to publish, as on GitHub. `sameCommit` in `internal/pipeline/steps/pr_head.go` was changed to accept the abbreviation.

### Bitbucket stopped-pipeline re-run

PR #21. `Host.RerunCheck` runs `twg bb pipeline rerun-failed --pipeline <build>`, so the existing `ci.rerun_transient` budget (trusted-only, default 0) applies to Bitbucket. `GetChecks` maps Bitbucket `STOPPED` to `CANCELLED` and reports the status link and last-changed time. A `FAILED` pipeline is a verdict on the code and is never re-run. A status that names no pipeline is refused without running anything. A refused rerun is logged and the check escalates as before, so the worst case is the old behavior. Unverified: whether Bitbucket re-runs a stopped pipeline through `rerun-failed` on a real pipeline. Docs: [`ci.rerun_transient`](docs/src/content/docs/reference/repo-config.md), [auto-fix](docs/src/content/docs/concepts/auto-fix.md).

### Bitbucket evidence images

PR #23. With `test.evidence.attach_media: true` (default off for Bitbucket, unlike GitHub), the PR description gets an `## Evidence` section with the run's screenshots. Images go through `twg bb pull-requests create|update --image` into the repository's Downloads, so they are readable by everyone who can read the repository. Only PNG, JPEG, GIF and WebP up to 10 MiB, at most ten images; video and over-limit files are listed as not attached, or linked when `test.evidence.link_base_url` is set. Upload names are `nm-<content digest>-<name>` so repeated writes reuse names. A PR is created text-first and images follow in an update, so a failed upload cannot create a second PR; a refused upload leaves the text-only description. Note that `attach_media` can be set in repository config for Bitbucket (as for GitLab), so a pushed branch can switch uploads on. Unverified against a real repository: image rendering, and whether a same-name re-upload replaces or duplicates a Downloads file. Docs: `test.evidence.attach_media` in [global-config](docs/src/content/docs/reference/global-config.md).

### Merged-PR duplicate guard

PR #16. A forge that merges on green can merge a PR before the pipeline's PR step runs, after which `FindPR` (open PRs only) sees nothing and a second PR is opened. New `scm.MergedPRFinder` interface; `FindMergedPRs` is implemented on the GitHub, GitLab and Bitbucket hosts, and `findCoveringMergedPR` in `pr_merged.go` skips PR creation when a merged PR already delivered the run's head to the run's base. Lookup failures are errors, never an empty result. Docs: PR step in [pipeline steps](docs/src/content/docs/reference/pipeline-steps.md).

### Better PR descriptions

PR #16, and the description pieces of #18. All provider-neutral unless noted.

- The description is refreshed from the run's final state after CI and repair rounds (`RunFinalizer` interface in `internal/pipeline/pipeline.go`, implemented by `PRStep.FinalizeRun` in `pr_refresh.go`).
- An approved Test exception (a configured test command that failed, or live validation ended no-go or inconclusive, then approved anyway) is explained instead of rendered as a plain error (`prsummary_test_exception.go`).
- The Intent section is rewritten into scannable points (`pr_intent_structure.go`).
- A rerun that only refreshes a description carries over the Test and other results of the run that did the work (`prsummary_carryover.go`).
- Pipeline and Testing sections describe the current run's steps only, so failures from an earlier run disappear once a later run passes (pinned by `TestPRStep_RerunDescriptionReflectsOnlyTheCurrentPipelineState`). The within-run "1 issue found, auto-fixed" narrative is unchanged.

### PR intent anchoring and `--replace-intent`

PR #18. When a PR is re-run after a correction (behind main, a conflict, a failing test), upstream's Intent section took on the correction text. The fork anchors the published Intent to the intent the PR was opened with: the earliest run bound to the PR URL that carried one (`db.GetOriginalPRIntent`, applied by `pinPublishedIntent`). The pin lasts only for the PR step, so a later run's own intent still drives its review, test and fix prompts. `pr.publish_intent` and `--no-publish-intent` still apply and can only remove the section.

`--replace-intent` on `no-mistakes axi run` (and on `no-mistakes rerun`, which requires `--intent`) is the only way to change the anchor. The run is recorded with intent source `replace`, and the most recent such run for the PR URL is the anchor. It travels as the push option `no-mistakes.replace-intent` and as `ReplaceIntent` on the push and rerun IPC params. It is refused with `--launch-nonce` and when reattaching to an active run. An older daemon ignores it, which keeps the anchor. The generated agent skill text and `axi run` help tell agents to pass the original intent on corrections. Docs: [Replacing the PR intent in cli reference](docs/src/content/docs/reference/cli.md) and pipeline steps.

### CI monitor resume after a daemon restart

PR #17. Upstream ends a run whose CI step was only watching an open PR as `ci_monitor_interrupted` after a crash, and as `failed` after a clean stop. In the fork, a clean stop (`daemon stop`, `daemon restart`, `update`) leaves an idle CI monitor (CI step `running`, no agent, PR recorded) in place, and startup resumes it with `Executor.ResumeCIMonitor` as a fresh observation of the live PR. Fix rounds already recorded still count against `auto_fix.ci`. It resumes only when the worktree exists and its head equals the run's pushed head; a fix round in flight, a recorded agent, a missing worktree or a moved head still end as `ci_monitor_interrupted`, so an unpushed CI fix is never adopted (`ValidateRecoveredCIMonitor`). This interacts with upstream's custody fix for interrupted CI (#1742): resumed runs are in the preserved set, and runs that cannot resume release push ownership. Not exercised: a real daemon restart against a live PR. Docs: [daemon crash recovery](docs/src/content/docs/concepts/daemon.md) and pipeline steps; note in the `ci-monitor` skill.

### Upstream sync bookkeeping

PRs #19 and #22. The fork syncs with a real merge commit, never a squash or rebase, and records the last synced upstream commit with the signed annotated tag `upstream-synced`. PR #22 shows why: a squash-merged sync PR leaves upstream's commit outside fork history, so #22 added an ancestry-only `git merge -s ours --no-ff 63c01829` (no file changes, tree hash equal to fork main) so `git log fork/main..origin/main` lists only unsynced upstream work. Merge such PRs with "Create a merge commit". See the maintenance section for the procedure.

## Behavior that differs from upstream and can conflict on sync

PR #19 had 17 conflicted files. Expect trouble in the same places. In each, keep both upstream's intent and the fork's.

- **Bitbucket package location.** `internal/bitbucket/` (REST client, host, PR content, repository content) is deleted and replaced by `internal/scm/bitbucket/`. Upstream changes to the old files show up as modify/delete conflicts; port the intent to `twg` instead of restoring them. A new upstream Bitbucket capability needs a `twg` implementation (as `GetPRRepositoryContent` and `VerifyPRHead` needed). Upstream's REST-token Bitbucket tests are dropped. `internal/scm/host.go` also changes the Bitbucket provider CLI name from `bb` to `twg` (`twg whoami` as the auth probe).
- **Rerun and push plumbing.** `internal/daemon/manager.go`, `internal/daemon/daemon.go`, `internal/cli/rerun.go`, `internal/cli/axi_drive.go`, `internal/cli/axi.go`, `internal/cli/daemon_cmd.go`, `internal/ipc/protocol.go`. The fork threads `ReplaceIntent` through `HandleRerun`/`handleRerun`/`startRunWithIntentSource` and the push-option parser, so upstream signature changes to these functions need the extra parameter carried through. The strict launch-receipt path must keep refusing `replace_intent`.
- **`internal/pipeline/executor.go`.** `ValidateRecoveredCIMonitor` and `ResumeCIMonitor` sit beside upstream's `SetCommitIdentity` and recovery code; the clean-shutdown path leaves an idle CI monitor. Re-check any upstream change to shutdown, recovery or CI custody against that.
- **`internal/pipeline/steps/pr.go` and the PR-body files** (`prsummary.go`, `pr_template.go`, `pr_refresh.go`, `pr_intent.go`, `pr_head.go`). Upstream's `verifyPublishedPR` runs first, then the fork's `pinPublishedIntent` and draft-hold logic. `buildPipelineSectionWith` keeps the fork's `final` flag and renders Testing before Pipeline. `buildPRTestingSummary` takes the fork's `ciPassed` and `carried` plus upstream's `evidenceLinkBase`. `sameCommit` accepts a 12-character abbreviation. `providerEvidenceSection` replaces upstream's `gitlabEvidenceSection` call site in `pr.go`.
- **CI step.** `internal/pipeline/steps/ci.go`, `ci_findings.go`, `ci_fix.go`, `ci_bitbucket.go` carry the review-comment findings, the transient re-run changes and the Bitbucket merge-readiness reading.
- **Shared scm contract.** `internal/scm/host.go` and `scm.go` add `ReviewCommentReplies`, `ReviewCommentResponder`, `PRDraftController` and `MergedPRFinder`. `internal/scm/github/github.go` and `internal/scm/gitlab/gitlab.go` were edited for `FindMergedPRs` (GitHub split `FindPR` into a shared `listPRs`). `internal/types/findings.go` adds `FindingCategoryCIReviewComment` and `Finding.CommentID`, which `Finding.UnmarshalJSON` and `findingWire` must also carry (upstream's `AGENTS.md` invariant).
- **Config defaults.** `providers.bitbucket.draft_pull_requests` defaults to `true` in `internal/config/config.go`, and `test.evidence.attach_media` also resolves to `BitbucketAttachMedia` (default off). Upstream defaults differ.
- **Generated skill.** The fork edits `internal/skill/skill.go` for the `--replace-intent` guidance, so `skills/no-mistakes/SKILL.md` must be regenerated after resolving, never hand-merged (`make lint` checks drift).
- **Test harness.** `internal/pipeline/fakecli/main.go` gained a fake `twg` (and `gh pr list --state`), `internal/pipeline/steps/helpers_test.go` was reworked around it, and `internal/pipeline/steps/internal/stepstest/helpers.go` changed. Pipeline-step tests that upstream writes against a REST Bitbucket fake need adapting to the fake `twg`.
- **CI workflow.** `.github/workflows/ci.yml` sets `GOFLAGS: -timeout=20m` for a slow shard and adds a comment about the Bitbucket shard on Windows. Upstream edits to that workflow may conflict.
- **Docs.** `concepts/daemon.md`, `concepts/auto-fix.md`, `reference/pipeline-steps.md`, `reference/cli.md`, `reference/global-config.md`, `reference/repo-config.md` and `guides/provider-integration.md` carry fork text beside upstream's.

## Maintaining this file

- When a feature is added to or removed from the fork, update this file in the same PR: the summary table, the feature section, and the conflict-hotspot list if it touches upstream-owned code. Update the status column when a feature is verified on a real service.
- After each upstream sync, review this file, then move the tag: `git tag -f -s -a upstream-synced <synced-upstream-sha> -m "..."` and `git push -f fork upstream-synced`. Update the "Last synced" line above with the new SHA and date. Never push to or open anything against upstream.
- Sync with a real merge commit (never squash or rebase) and merge the sync PR with "Create a merge commit". If a sync PR was squashed, record the ancestry with `git merge -s ours --no-ff <upstream-sha>` as PR #22 did.
- Do not copy feature documentation into this file. Link to the docs pages instead.
