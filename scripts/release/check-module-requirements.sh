#!/usr/bin/env bash
# Fails unless every requirement one released Go module has on another
# released module names the version being released.
#
#   check-module-requirements.sh <tag> <module>...
#
# The modules are tagged <module>/<tag> at one commit. A local replace
# directive is ignored when the module is used from outside the
# repository, so a requirement such as gen's on types must already name
# <tag> in the tagged commit, or `go get .../gen@<tag>` cannot resolve
# types. Run from the repository root with GITHUB_REPOSITORY set to
# <owner>/<repo>.
set -euo pipefail

[ $# -ge 2 ] || {
  echo "usage: check-module-requirements.sh <tag> <module>..." >&2
  exit 1
}
readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
tag=$1
shift
released=("$@")

# requirements <go.mod> prints "<path> <version>" for each require entry,
# from both the single-line and the block form.
requirements() {
  awk '
    /^[ \t]*\/\// { next }
    /^require[ \t]*\(/ { block = 1; next }
    block && /^[ \t]*\)/ { block = 0; next }
    block && NF >= 2 { print $1, $2; next }
    /^require[ \t]+[^( \t]/ { print $2, $3 }
  ' "$1"
}

failures=0
for module in "${released[@]}"; do
  gomod="${module}/go.mod"
  [ -f "$gomod" ] || {
    echo "::error::${gomod} does not exist" >&2
    exit 1
  }
  while read -r path version; do
    for dependency in "${released[@]}"; do
      [ "$path" = "github.com/${REPO}/${dependency}" ] || continue
      if [ "$version" != "$tag" ]; then
        echo "::error::${gomod} requires ${path} ${version}, but this release tags ${dependency}/${tag}. Commit \`go mod edit -require=${path}@${tag}\` in ${module}/ and tag that commit." >&2
        failures=$((failures + 1))
      else
        echo "${gomod} requires ${path} ${tag}"
      fi
    done
  done < <(requirements "$gomod")
done
[ "$failures" -eq 0 ]
