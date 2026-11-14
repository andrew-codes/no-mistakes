# What this fork gives you

This is [andrew-codes/no-mistakes](https://github.com/andrew-codes/no-mistakes), a fork of [kunchenguid/no-mistakes](https://github.com/kunchenguid/no-mistakes), maintained by Andrew Smith. It does everything upstream does, plus the capabilities below.

Last synced with upstream: `67c4cfa2` (2026-11-13).

## What you gain

| Capability | What it gives you |
| --- | --- |
| Bitbucket Cloud support | Run the full pipeline on Bitbucket Cloud repositories, with no Bitbucket token handed to no-mistakes |
| Bitbucket review comments | Reviewer comments on your PR get fixed and answered for you |
| Draft until proven | Bitbucket PRs stay drafts until the run has proven them ready |
| Pushed-head proof on Bitbucket | A PR description is only published for the exact commit you pushed |
| Re-run of stopped pipelines | A stopped Bitbucket pipeline is re-run before it bothers you |
| Warnings do not break Bitbucket reads | A notice from the Atlassian CLI (an update or deprecation warning) no longer stops PR lookups or CI polling on a healthy run |
| Evidence images on Bitbucket | Test screenshots show up in the PR description |
| No duplicate PRs | A branch whose PR already merged does not get a second PR |
| Accurate PR descriptions | The description reflects how the run actually ended |
| A PR keeps its original intent | Corrections and reruns do not overwrite why the PR was opened |
| Monitoring survives restarts | PR monitoring carries on after the daemon restarts or updates |

## Bitbucket Cloud

Bitbucket Cloud works as a first-class provider. You sign in once with the Atlassian `twg` CLI (`twg login`), and no-mistakes uses that login, so it never reads or stores a Bitbucket token itself.

- **Pull requests.** It creates and updates PRs, follows CI, fetches the logs of failed pipeline steps for the auto-fix step, and can retarget an existing PR to a different base branch.
- **Merge readiness.** It watches Bitbucket's own merge checks. A merge conflict, or a branch Bitbucket says is too far behind its destination, is repaired by rebasing and revalidating.
- **Review comments.** Every unresolved review comment, from people or bots, becomes a finding the auto-fix agent addresses. It then replies on the comment, and resolves the thread when the code changed. A comment it decides not to act on gets a reply explaining why and stays open.
- **Drafts.** New PRs open as drafts. A PR is marked ready only when every step passed, checks are green, no comment is waiting for a reply, there is no merge conflict, and the description reflects the final state. Otherwise it stays a draft.
- **Head proof.** The PR step checks that the PR is open, from your branch, at the commit you pushed, and refuses to publish if it cannot confirm that.
- **Stopped pipelines.** A pipeline Bitbucket reports as stopped is re-run for the same commit instead of being escalated to you. A failed pipeline is a verdict on your code and is never re-run.
- **Evidence images.** Test screenshots can be embedded in the PR description. This is off by default because the images are uploaded to the repository's Downloads, where anyone who can read the repository can see them.

Not available on Bitbucket: findings from review-bot checks, closing issues from a PR, and opening a PR from a personal fork.

## Better pull requests, on every provider

- **No duplicate PRs.** If the branch's PR was already merged (for example by a merge-on-green rule) before the PR step ran, no second PR is opened.
- **A description that matches the final result.** The description is refreshed when the run ends, so failures that were later fixed do not linger. A rerun that only refreshes the description keeps the results of the run that did the work.
- **Approved test exceptions are explained.** If you approved a run over a failing test command or an inconclusive live check, the description says so plainly instead of showing a bare error.
- **A scannable Intent section.** The intent is shown as a short goal followed by one bullet per ask or constraint, instead of a wall of text.

## A PR keeps its original intent

When a PR is re-run after a correction (the branch fell behind, a merge conflict, a failing test, review feedback), the description's Intent section stays on what the PR was opened for. The correction still drives that run's own review, test and fix work, but it never rewrites the description. If you genuinely change direction, say so with `--replace-intent` (see below).

## Monitoring survives restarts

Upstream stops watching a PR when the daemon restarts or updates. Here, a run that was only watching an open PR picks up where it left off after a restart and keeps repairing conflicts, failing checks and review feedback until the PR merges or closes. A run that was in the middle of a fix is not resumed, so an unpushed fix is never adopted by accident.

## New configuration

The fork adds one new flag and changes how three existing settings behave. There are no new configuration keys.

### New: `--replace-intent`

**What it is.** A flag on `no-mistakes axi run` and `no-mistakes rerun` (the latter also needs `--intent`).

**What it controls.** By default, a PR's description keeps the intent the PR was opened with, whatever intent later runs pass. `--replace-intent` marks the `--intent` you pass as a deliberate change of direction, and makes it the new published intent. Later runs keep the new one until another run uses the flag. It is refused together with `--launch-nonce`, and does not apply when reattaching to a run that is already active.

**Example.**

```sh
# First run: the intent the PR is opened with
no-mistakes axi run --intent "Add a CSV export for widget reports"

# Resubmit after a rebase: the description still says "Add a CSV export..."
no-mistakes axi run --intent "Add a CSV export for widget reports"

# The goal really changed: replace the published intent
no-mistakes axi run --intent "Export widget reports as Parquet instead of CSV" --replace-intent
```

### Changed: `providers.bitbucket.draft_pull_requests`

Defaults to `true` here (upstream: `false`), and now means the full draft-until-proven behavior described above. Set it to `false`, globally or per repository, to open Bitbucket PRs ready for review and never toggle their draft state.

```yaml
# ~/.no-mistakes/config.yaml, or the repository's .no-mistakes.yaml
providers:
  bitbucket:
    draft_pull_requests: false
```

### Changed: `test.evidence.attach_media`

Also controls Bitbucket evidence images, where it defaults to `false` (GitHub keeps `true`). Set it to `true` to embed screenshots (PNG, JPEG, GIF or WebP, up to 10 MiB each, at most ten) in the Bitbucket PR description. Videos are not supported on Bitbucket.

```yaml
test:
  evidence:
    attach_media: true
```

### Changed: `ci.rerun_transient`

Now applies to Bitbucket too: it sets how many times a stopped pipeline is re-run before it is escalated. It defaults to `0`, which means never re-run. It is read from the repository's default branch only.

```yaml
ci:
  rerun_transient: 1
```
