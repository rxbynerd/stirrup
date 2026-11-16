#!/usr/bin/env bash
# Decides whether a release run may upload its assets, comparing the
# SHA256SUMS it signed with the one already published for <tag>.
#
#   compare-published-release.sh <tag> <release-dir>
#
# Prints "upload" when the tag has no release, the release has no
# SHA256SUMS yet, or the published SHA256SUMS is byte-identical (a
# re-run completing an interrupted upload). Prints "keep" when only the
# SBOM lines differ: SBOMs carry a timestamp and a random document ID,
# so a rebuild never reproduces them, and the published, signed set
# stays as it is. Exits 1 when an archive or IMAGE_DIGESTS line differs,
# when a kept release lacks an asset, or when the lookup itself fails.
#
# Needs gh (authenticated through GH_TOKEN) and GITHUB_REPOSITORY set to
# <owner>/<repo>.
set -euo pipefail

[ $# -eq 2 ] || {
  echo "usage: compare-published-release.sh <tag> <release-dir>" >&2
  exit 1
}
readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
tag=$1 release=$2
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

rc=0
gh api --include "repos/${REPO}/releases/tags/${tag}" >"$workdir/response" 2>"$workdir/stderr" || rc=$?
status=$(awk 'NR == 1 { print $2; exit }' "$workdir/response")
if [ "$rc" -ne 0 ]; then
  if [ "$status" = 404 ]; then
    echo "No release for ${tag} yet." >&2
    echo upload
    exit 0
  fi
  echo "::error::looking up the release for ${tag} failed (HTTP ${status:-none}): $(head -c 300 "$workdir/stderr")" >&2
  exit 1
fi
if [ "$status" != 200 ]; then
  echo "::error::looking up the release for ${tag} returned HTTP ${status:-none}" >&2
  exit 1
fi
awk 'body { print } /^\r?$/ { body = 1 }' "$workdir/response" >"$workdir/release.json"
if ! jq -e '.assets | type == "array"' "$workdir/release.json" >/dev/null 2>&1; then
  echo "::error::the release lookup for ${tag} returned no assets list" >&2
  exit 1
fi
jq -r '.assets[].name' "$workdir/release.json" >"$workdir/published-names"

if ! grep -qx SHA256SUMS "$workdir/published-names"; then
  echo "Release ${tag} has no SHA256SUMS yet." >&2
  echo upload
  exit 0
fi

gh release download "$tag" --repo "$REPO" --pattern SHA256SUMS --dir "$workdir/published"
if cmp -s "$workdir/published/SHA256SUMS" "$release/SHA256SUMS"; then
  echo "Release ${tag} already publishes this SHA256SUMS." >&2
  echo upload
  exit 0
fi

without_sboms() {
  awk '$2 !~ /\.(spdx|cdx)\.json$/' "$1"
}
if ! cmp -s <(without_sboms "$workdir/published/SHA256SUMS") <(without_sboms "$release/SHA256SUMS"); then
  echo "::error::release ${tag} already publishes a SHA256SUMS whose archives or IMAGE_DIGESTS differ from this run's; published assets are never replaced" >&2
  diff "$workdir/published/SHA256SUMS" "$release/SHA256SUMS" >&2 || true
  exit 1
fi

absent=0
for asset in "$release"/*; do
  name=${asset##*/}
  if ! grep -qxF "$name" "$workdir/published-names"; then
    echo "::error::release ${tag} is missing ${name}, and this run's SBOMs differ from the published ones, so it cannot add it. Re-run the failed jobs of the run that published the release." >&2
    absent=$((absent + 1))
  fi
done
[ "$absent" -eq 0 ] || exit 1

echo "::notice::release ${tag} already publishes these archives and images; only the regenerated SBOMs differ, so the published assets are kept." >&2
echo keep
