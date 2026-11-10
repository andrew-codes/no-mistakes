#!/usr/bin/env bash
# Resolves and validates the version fork-release.yml builds, and decides
# whether the run may ship.
#
#   release event      the tag of the release that was just published is the
#                      version (release-drafter already computed it); the run
#                      may ship.
#   anything else      a rehearsal: the tag of the current draft release is
#                      used (v0.0.0 when there is none) and the run never ships.
#
# The tag must be plain semver (vMAJOR.MINOR.PATCH), strictly greater than
# every other published release, and, for a release event, must point at the
# tip of the default branch. The last check is what catches a draft whose tag
# already existed at an older commit: publishing it would silently release that
# old tree under the new number. Git tags are deliberately not part of the
# ordering: the fork's line is independent of upstream's, so upstream v* tags
# pushed into the fork must not block a release.
#
# Environment:
#   EVENT_NAME        github.event_name
#   RELEASE_TAG_NAME  the published release's tag (release events)
#   GH_REPO           owner/repo, for `gh release list`
#   GITHUB_SHA        the commit the run was triggered on
#   DEFAULT_BRANCH    the repository's default branch
#   GITHUB_OUTPUT     receives version, tag and should_publish
set -euo pipefail
export LC_ALL=C

event="${EVENT_NAME:?EVENT_NAME is required}"
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

# sort_key renders vX.Y.Z as a fixed-width string that orders like semver.
sort_key() {
  local v="${1#v}" major minor patch
  IFS=. read -r major minor patch <<<"$v"
  printf '%012d%012d%012d' "$((10#$major))" "$((10#$minor))" "$((10#$patch))"
}

if [ "$event" = release ]; then
  tag="${RELEASE_TAG_NAME:?RELEASE_TAG_NAME is required for a release event}"
  should_publish=true
  placeholder=false
else
  should_publish=false
  placeholder=false
  tag="$(gh release list --limit 100 --json tagName,isDraft \
    --jq '[.[] | select(.isDraft)][0].tagName // empty')"
  if [ -z "$tag" ]; then
    tag=v0.0.0
    placeholder=true
    echo "::notice::No draft release to rehearse; using the placeholder ${tag}."
  fi
fi

if ! [[ "$tag" =~ $semver ]]; then
  echo "::error::release tag '${tag}' is not plain semver (vMAJOR.MINOR.PATCH, no prerelease or build suffix)"
  exit 1
fi

# Every other published release version. The "channels" release is not a version.
others="$(
  gh release list --exclude-drafts --limit 1000 --json tagName --jq '.[].tagName' \
    | grep -E "$semver" | grep -vxF "$tag" || true
)"
want="$(sort_key "$tag")"
# The v0.0.0 rehearsal placeholder is not a real version, so it is not ordered.
if [ "$placeholder" = false ]; then
  while IFS= read -r other; do
    [ -n "$other" ] || continue
    if ! [[ "$(sort_key "$other")" < "$want" ]]; then
      echo "::error::${tag} is not greater than the existing release or tag ${other}; every release must have a new, higher version"
      exit 1
    fi
  done <<<"$others"
fi

if [ "$should_publish" = true ]; then
  branch="${DEFAULT_BRANCH:?DEFAULT_BRANCH is required for a release event}"
  tip="$(git rev-parse "origin/${branch}")"
  head="$(git rev-parse "${GITHUB_SHA:?GITHUB_SHA is required for a release event}")"
  if [ "$tip" != "$head" ]; then
    echo "::error::${tag} points at ${head}, not the tip of ${branch} (${tip}). Either ${branch} moved after the release was published, or the tag already existed at an older commit. Delete the tag and publish the draft again."
    exit 1
  fi
fi

echo "Resolved ${tag}, should_publish=${should_publish}"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "version=${tag#v}"
    echo "tag=${tag}"
    echo "should_publish=${should_publish}"
  } >>"$GITHUB_OUTPUT"
fi
