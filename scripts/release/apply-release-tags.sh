#!/usr/bin/env bash
# Points a release's image tags at the digest the release signed, in
# GHCR and in the mirror.
#
#   apply-release-tags.sh <ghcr-image> <mirror-image> <digest> <tag> <commit> [<moving-tag>...]
#
# <tag> is the immutable version tag. It is created where it is absent
# and left alone where it already names <digest>; if either registry
# holds it under any other digest, nothing is written. It is only applied
# to a digest that carries the release signing workflow's provenance
# attestation for <tag> at <commit>, so a version tag never names an
# unsigned image. Each <moving-tag> (vX.Y, latest) is then pointed at
# <digest> in both registries. Re-running after a partial failure
# finishes the job.
#
# GHCR_USERNAME and GHCR_PASSWORD, and MIRROR_USERNAME and
# MIRROR_PASSWORD, authenticate to each registry's token endpoint;
# GH_TOKEN and GITHUB_REPOSITORY are used for the attestation check.
set -euo pipefail

[ $# -ge 5 ] || {
  echo "usage: apply-release-tags.sh <ghcr-image> <mirror-image> <digest> <tag> <commit> [<moving-tag>...]" >&2
  exit 1
}
ghcr=$1 mirror=$2 digest=$3 tag=$4 commit=$5
shift 5
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}"
registry=$(cd "$(dirname "$0")" && pwd)/registry.sh

[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "::error::'${digest}' is not a sha256 digest" >&2
  exit 1
}
for moving in "$@"; do
  if [[ "$moving" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ || "$moving" == sha-* ]]; then
    echo "::error::'${moving}' has the shape of an immutable tag and cannot be moved" >&2
    exit 1
  fi
done

# as_registry <image> <command>... runs registry.sh with the credentials
# for <image>'s registry.
as_registry() {
  local image=$1
  shift
  if [ "$image" = "$ghcr" ]; then
    REGISTRY_USERNAME=${GHCR_USERNAME:-} REGISTRY_PASSWORD=${GHCR_PASSWORD:-} "$registry" "$@"
  else
    REGISTRY_USERNAME=${MIRROR_USERNAME:-} REGISTRY_PASSWORD=${MIRROR_PASSWORD:-} "$registry" "$@"
  fi
}

missing=()
for image in "$ghcr" "$mirror"; do
  rc=0
  current=$(as_registry "$image" digest "${image}:${tag}") || rc=$?
  case "$rc" in
    0)
      if [ "$current" != "$digest" ]; then
        echo "::error::${image}:${tag} already names ${current}, not the signed ${digest}. Version tags are never moved; cut a new version." >&2
        exit 1
      fi
      echo "${image}:${tag} already names ${digest}" >&2
      ;;
    3) missing+=("$image") ;;
    *)
      echo "::error::could not read ${image}:${tag}; nothing was tagged" >&2
      exit 1
      ;;
  esac
done

if ! "$(dirname "$registry")/verify-image-attestation.sh" "$ghcr" "$digest" "$tag" "$commit"; then
  echo "::error::${ghcr}@${digest} has no verifiable provenance attestation from the release signing workflow for ${tag} at ${commit}; nothing was tagged. After an API error, re-run this job." >&2
  exit 1
fi

for image in ${missing[@]+"${missing[@]}"}; do
  as_registry "$image" tag "${image}@${digest}" "$tag"
done
for moving in "$@"; do
  for image in "$ghcr" "$mirror"; do
    as_registry "$image" tag "${image}@${digest}" "$moving"
  done
done
