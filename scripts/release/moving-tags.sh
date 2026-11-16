#!/usr/bin/env bash
# Prints the moving image tags a release should take, one per line,
# given the repository's release tags on stdin.
#
#   git ls-remote --tags --refs origin 'v*' | sed 's|.*refs/tags/||' | moving-tags.sh <tag>
#
# A pre-release takes none. A release takes vX.Y when it is the highest
# vX.Y.* release, and latest when it is the highest release overall, so
# a patch for an older line never moves either tag backwards.
set -euo pipefail

[ $# -eq 1 ] || {
  echo "usage: moving-tags.sh <tag> < release-tags" >&2
  exit 1
}
tag=$1
readonly STABLE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

[[ "$tag" =~ $STABLE ]] || exit 0
major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}

newest_line=true
newest_overall=true
while IFS= read -r other; do
  [[ "$other" =~ $STABLE ]] || continue
  o_major=${BASH_REMATCH[1]} o_minor=${BASH_REMATCH[2]} o_patch=${BASH_REMATCH[3]}
  if ((o_major > major || (o_major == major && o_minor > minor) ||
    (o_major == major && o_minor == minor && o_patch > patch))); then
    newest_overall=false
  fi
  if ((o_major == major && o_minor == minor && o_patch > patch)); then
    newest_line=false
  fi
done

if [ "$newest_line" = true ]; then
  echo "v${major}.${minor}"
fi
if [ "$newest_overall" = true ]; then
  echo latest
fi
