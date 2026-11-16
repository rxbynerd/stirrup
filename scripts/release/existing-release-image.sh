#!/usr/bin/env bash
# Prints the digest behind an already-published release image tag, or
# nothing when the tag exists in neither registry.
#
#   existing-release-image.sh <ghcr-image> <mirror-image> <tag> <commit>
#
# A version tag is never rebuilt or moved, so an existing tag is only
# reusable when both registries serve the same digest under it and that
# digest already carries a provenance attestation from this repository's
# release signing workflow for <tag> at <commit>. Every other state
# exits 1. The release applies version tags only after signing
# (apply-release-tags.sh), so a tag left by an earlier run of the same
# release always passes, except one applied to a single registry by an
# interrupted tagging job; re-running that job completes it.
#
# MIRROR_USERNAME and MIRROR_PASSWORD authenticate to the mirror's token
# endpoint; GH_TOKEN and GITHUB_REPOSITORY are used for the attestation
# lookup.
set -euo pipefail

[ $# -eq 4 ] || {
  echo "usage: existing-release-image.sh <ghcr-image> <mirror-image> <tag> <commit>" >&2
  exit 1
}
ghcr=$1 mirror=$2 tag=$3 commit=$4
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}"
registry=$(cd "$(dirname "$0")" && pwd)/registry.sh

ghcr_rc=0
ghcr_digest=$("$registry" digest "${ghcr}:${tag}") || ghcr_rc=$?
mirror_rc=0
mirror_digest=$(REGISTRY_USERNAME=${MIRROR_USERNAME:-} REGISTRY_PASSWORD=${MIRROR_PASSWORD:-} \
  "$registry" digest "${mirror}:${tag}") || mirror_rc=$?

case "${ghcr_rc}:${mirror_rc}" in
  3:3) exit 0 ;;
  0:0) ;;
  *)
    echo "::error::${tag} is in an inconsistent state (GHCR lookup exit ${ghcr_rc}, mirror lookup exit ${mirror_rc}; 3 means absent, other codes are lookup failures). After a failed lookup, re-run this job. When one registry lacks the tag, an earlier run's tagging job stopped part-way: re-run the failed jobs of that run, whose 'Tag release images' job adds the missing tag. Version tags are never rebuilt." >&2
    exit 1
    ;;
esac

if [ "$ghcr_digest" != "$mirror_digest" ]; then
  echo "::error::${ghcr}:${tag} is ${ghcr_digest} but ${mirror}:${tag} is ${mirror_digest}. Version tags are never rebuilt; cut a new version." >&2
  exit 1
fi

if ! "$(dirname "$registry")/verify-image-attestation.sh" "$ghcr" "$ghcr_digest" "$tag" "$commit"; then
  echo "::error::${ghcr}:${tag} (${ghcr_digest}) has no provenance attestation from this release at ${commit}, so it is not reused. If the verification above failed on an API error, re-run this job. Otherwise the tag was not written by the release workflow, which tags only signed digests: investigate it, and cut a new version." >&2
  exit 1
fi

echo "::notice::${ghcr}:${tag} is already published at ${ghcr_digest}; reusing it without a rebuild." >&2
printf '%s\n' "$ghcr_digest"
