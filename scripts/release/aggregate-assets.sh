#!/usr/bin/env bash
# Collects the release assets into one directory and writes the two
# manifests the release signs:
#
#   IMAGE_DIGESTS  <image>:<tag>@<digest>, one line per published image
#   SHA256SUMS     sha256sum output over every other file, bare names
#
#   aggregate-assets.sh <artifacts-dir> <release-dir> <tag> <image>@<digest>...
#
# The asset set is fixed: each expected file must appear exactly once
# under <artifacts-dir>, and nothing else is copied.
set -euo pipefail

[ $# -ge 4 ] || {
  echo "usage: aggregate-assets.sh <artifacts-dir> <release-dir> <tag> <image>@<digest>..." >&2
  exit 1
}
artifacts=$1 release=$2 tag=$3
shift 3

expected=()
for platform in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  expected+=("stirrup-${tag}-${platform}.tar.gz")
done
expected+=("stirrup-${tag}.spdx.json" "stirrup-${tag}.cdx.json")

mkdir -p "$release"
for name in "${expected[@]}"; do
  matches=()
  while IFS= read -r -d '' f; do
    matches+=("$f")
  done < <(find "$artifacts" -type f -name "$name" -print0)
  if [ ${#matches[@]} -ne 1 ]; then
    echo "aggregate-assets.sh: expected exactly one ${name} under ${artifacts}, found ${#matches[@]}" >&2
    exit 1
  fi
  cp -v "${matches[0]}" "$release/$name"
done

: >"$release/IMAGE_DIGESTS"
for pinned in "$@"; do
  image=${pinned%@*}
  digest=${pinned##*@}
  if ! [[ "$image" =~ ^[a-z0-9.-]+/[a-z0-9._/-]+$ && "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "aggregate-assets.sh: malformed image pin '${pinned}'" >&2
    exit 1
  fi
  printf '%s:%s@%s\n' "$image" "$tag" "$digest" >>"$release/IMAGE_DIGESTS"
done

cd "$release"
sha256sum IMAGE_DIGESTS "${expected[@]}" | LC_ALL=C sort -k2 >SHA256SUMS
echo "--- IMAGE_DIGESTS ---"
cat IMAGE_DIGESTS
echo "--- SHA256SUMS ---"
cat SHA256SUMS
sha256sum -c SHA256SUMS
