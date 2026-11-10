# Releasing the fork

This is the maintainer procedure for releasing `andrew-codes/no-mistakes`. For what the fork does differently from upstream, see FORK.md. Everything here lives in new files (`fork-*` workflows, `.github/fork-release-drafter.yml`, `scripts/fork-release-version.sh`), so an upstream sync never conflicts with it. Upstream's `release.yml`, `publish-channels.yml` and `docs.yml` stay disabled.

The fork has its own semver line (`v1.0.0`, `v1.1.0`, ...) that is independent of upstream's version numbers. It follows the same model as ps5-mqtt: a draft release is kept up to date as PRs merge, and publishing the draft is the release.

## How a release happens

1. A PR merges to `main`. `fork-draft-release` runs release-drafter, which updates the one draft release: the next version (from the PR labels) and the release notes.
2. You review the draft and publish it. Publishing creates the tag.
3. `fork-release` starts on the published release. It validates the version, builds the six platform archives, attaches them and `checksums.txt` to that same release, and refreshes the update channel manifest so `no-mistakes update` finds it.
4. If any part of step 3 fails, the release is reverted to a draft and its tag is deleted, so a failed run leaves no published release without binaries. Fix the cause and publish the draft again.

You never create a tag or a release by hand, and nothing releases on its own: only publishing the draft ships.

## How labels pick the version

Every PR needs one label that resolves to a bump. The `fork-pr-release-label` check applies it from the PR title and fails the PR when none applies, so you rarely label anything yourself. Use conventional commit titles:

| PR title starts with | Label added | Bump | Release notes section |
| --- | --- | --- | --- |
| `feat:` or `feat(scope):` | `new-feature` | minor | New features |
| `perf:` | `performance` | minor | New features |
| `fix:` | `bugfix` | patch | Bug fixes |
| `docs:` | `documentation` | patch | Documentation |
| `chore:`, `ci:`, `refactor:`, `test:`, `build:`, `style:` | `maintenance` | patch | Maintenance |
| any type with `!` (for example `feat!:`), `breaking:`, or `BREAKING CHANGE` in the body | `breaking-change` | major | Breaking changes |
| a title or branch that names an upstream sync | `upstream-sync` | patch | Synced with upstream |

The highest bump among all PRs merged since the last release wins. A PR with no matching label counts as a patch.

You can add a label by hand at any time. `major`, `minor` and `patch` set the bump without adding a release notes section, and `enhancement`, `dependencies`, `refactor` and `ci` work as well (see `.github/fork-release-drafter.yml` for the full list).

### Upstream syncs

A merged range of upstream commits arrives as one fork PR, so it shows up as a single line under "Synced with upstream" (for example `Merge upstream kunchenguid/no-mistakes main (63c01829) into fork main (#19)`). It never lists the upstream commits or PR titles. The label is detected from a title like `Merge upstream ...` or `Record upstream sync ...`, or from a branch named `sync/...` or `upstream/...`. Add `upstream-sync` by hand if a sync PR is titled differently.

A sync counts as a patch. When it brings upstream features you want reflected in the fork version, add `minor` (or `major`) to the sync PR.

## Editing the draft

The draft is a normal GitHub release, so you can edit its notes before publishing. Keep these in mind:

- The next push to `main` (or a manual run of `fork-draft-release`) regenerates the draft and overwrites hand edits to the notes. Edit the notes right before you publish.
- To change the version, add a `major`, `minor` or `patch` label to a merged PR and run `fork-draft-release` on `main`, rather than editing the tag. If you do edit the tag, it must be plain semver (`vMAJOR.MINOR.PATCH`) and higher than every existing release and tag, or `fork-release` refuses it.
- Leave "Set as the latest release" on. The update channel's stable slot follows GitHub's latest release. Marking a release as a pre-release keeps it on the beta channel only.

## The first release

The fork has no release yet, and release-drafter drafts `v0.0.1` when there is no previous published release. Until one exists, `fork-draft-release` pins the draft to `v1.0.0` (`FORK_FIRST_VERSION` in that workflow). The first draft has no comparison baseline, so it lists no changes and carries a release-drafter warning. Replace the body with your own notes, then publish. Later drafts compare against the previous release and need nothing special.

## Rehearsing a release

Run **Actions > fork-release > Run workflow** to exercise the pipeline without shipping. It validates the version (taken from the current draft release, or `v0.0.0` when there is none), builds all six archives, and checksums them. It never uploads to a release, creates a tag or touches the channel manifest. The archives are available as workflow artifacts.

## One-time repository setup

- Settings > Actions > General > Workflow permissions: allow read and write. The workflows request `contents: write` (and `pull-requests: write` for labelling), which a read-only default would cap.
- Confirm `fork-draft-release`, `fork-pr-release-label` and `fork-release` show as enabled under the Actions tab (new workflow files normally start enabled). Leave `release.yml`, `publish-channels.yml` and `docs.yml` disabled.
- To make the label check required, add `Require a version-resolvable label` as a required status check on `main`. `fork-pr-release-label` also runs on PRs from forks, so it uses `pull_request_target` and never checks out or runs PR code.
- No secrets or environments are needed.

## Installing a fork build

Fork binaries update themselves from the fork: `no-mistakes update` reads `https://github.com/andrew-codes/no-mistakes/releases/download/channels/channels.json` because the release workflow stamps `buildinfo.UpdateRepo`. For a first install, download the archive for your platform from the release, or use the upstream installer with the repository swapped:

```sh
curl -fsSL https://raw.githubusercontent.com/andrew-codes/no-mistakes/main/docs/install.sh | sed 's#^REPO=.*#REPO="andrew-codes/no-mistakes"#' | sh
```

macOS and Windows binaries are unsigned. `no-mistakes update` clears the macOS quarantine attribute itself; a hand-downloaded archive may need `xattr -d com.apple.quarantine`.
