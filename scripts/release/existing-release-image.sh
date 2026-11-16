#!/usr/bin/env bash
# Prints the digest behind an already-published release image tag, or
# nothing when the tag does not exist yet.
#
#   existing-release-image.sh <ghcr-image> <mirror-image> <tag> <commit>
#
# A version tag is never rebuilt or moved, so an existing tag is only
# reusable when its image was built from <commit> and <mirror-image>
# serves the same digest under the same tag; anything else exits 1.
# Reading the mirror uses the Docker credential store, so the caller
# must already be logged in to it.
set -euo pipefail

[ $# -eq 4 ] || {
  echo "usage: existing-release-image.sh <ghcr-image> <mirror-image> <tag> <commit>" >&2
  exit 1
}
ghcr=$1 mirror=$2 tag=$3 commit=$4
registry=$(cd "$(dirname "$0")" && pwd)/registry.sh

rc=0
digest=$("$registry" digest "${ghcr}:${tag}") || rc=$?
case "$rc" in
  0) ;;
  3) exit 0 ;;
  *) exit "$rc" ;;
esac

revisions=$(docker buildx imagetools inspect "${ghcr}@${digest}" --format '{{json .Image}}' |
  jq -r '[.. | objects | select(has("config")) | .config.Labels["org.opencontainers.image.revision"]? // empty] | unique | join(" ")')
if [ "$revisions" != "$commit" ]; then
  echo "::error::${ghcr}:${tag} (${digest}) was built from revision '${revisions}', not ${commit}. Version tags are immutable; cut a new version instead of rebuilding this one." >&2
  exit 1
fi

mirror_digest=$(docker buildx imagetools inspect "${mirror}:${tag}" --format '{{json .Manifest}}' | jq -r '.digest') || mirror_digest=""
if [ "$mirror_digest" != "$digest" ]; then
  echo "::error::${ghcr}:${tag} is ${digest} but ${mirror}:${tag} is '${mirror_digest:-unreadable}'. Copy the GHCR image to the mirror by digest; the release workflow does not rebuild an existing version." >&2
  exit 1
fi

echo "::notice::${ghcr}:${tag} is already published at ${digest}; reusing it without a rebuild." >&2
printf '%s\n' "$digest"
