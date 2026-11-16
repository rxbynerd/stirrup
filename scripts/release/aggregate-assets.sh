#!/usr/bin/env bash
# Collects the release assets into one directory and writes the two
# manifests the release signs:
#
#   IMAGE_DIGESTS  <image>:<tag>@<digest>, one line per published image
#   SHA256SUMS     sha256sum output over every other file, bare names
#
#   aggregate-assets.sh <artifacts-dir> <release-dir> <tag> <image>@<digest>...
set -euo pipefail

[ $# -ge 4 ] || {
  echo "usage: aggregate-assets.sh <artifacts-dir> <release-dir> <tag> <image>@<digest>..." >&2
  exit 1
}
artifacts=$1 release=$2 tag=$3
shift 3

mkdir -p "$release"
find "$artifacts" -type f \( \
  -name 'stirrup-*.tar.gz' -o \
  -name 'stirrup-*.spdx.json' -o \
  -name 'stirrup-*.cdx.json' \
  \) -exec cp -v {} "$release/" \;

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
files=()
while IFS= read -r -d '' f; do
  files+=("${f#./}")
done < <(find . -maxdepth 1 -type f ! -name SHA256SUMS ! -name 'SHA256SUMS.*' -print0 | LC_ALL=C sort -z)
[ ${#files[@]} -gt 1 ] || {
  echo "aggregate-assets.sh: no release assets found under ${artifacts}" >&2
  exit 1
}
sha256sum "${files[@]}" >SHA256SUMS
echo "--- IMAGE_DIGESTS ---"
cat IMAGE_DIGESTS
echo "--- SHA256SUMS ---"
cat SHA256SUMS
sha256sum -c SHA256SUMS
