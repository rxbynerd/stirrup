#!/usr/bin/env bash
# Creates the Go module tags <module>/<tag> for the importable modules at
# the release commit, so `go get github.com/<owner>/<repo>/<module>@<tag>`
# resolves to a version instead of a pseudo-version. A tag that already
# points at the commit is left alone; one that points elsewhere fails.
#
#   tag-go-modules.sh <tag> <commit> <module>...
#
# Needs gh (authenticated through GH_TOKEN with contents: write) and
# GITHUB_REPOSITORY set to <owner>/<repo>.
set -euo pipefail

[ $# -ge 3 ] || {
  echo "usage: tag-go-modules.sh <tag> <commit> <module>..." >&2
  exit 1
}
readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
tag=$1 commit=$2
shift 2
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || {
  echo "tag-go-modules.sh: '${commit}' is not a full commit SHA" >&2
  exit 1
}

# ref_commit <ref> prints the commit an existing tag ref points at, or
# nothing when the ref does not exist.
ref_commit() {
  local ref=$1 json type sha
  json=$(gh api "repos/${REPO}/git/matching-refs/${ref#refs/}" --jq "map(select(.ref == \"${ref}\")) | .[0] // empty")
  [ -n "$json" ] || return 0
  type=$(jq -r '.object.type' <<<"$json")
  sha=$(jq -r '.object.sha' <<<"$json")
  if [ "$type" = tag ]; then
    sha=$(gh api "repos/${REPO}/git/tags/${sha}" --jq '.object.sha')
  fi
  printf '%s\n' "$sha"
}

for module in "$@"; do
  [[ "$module" =~ ^[a-z][a-z0-9-]*$ ]] || {
    echo "tag-go-modules.sh: invalid module directory '${module}'" >&2
    exit 1
  }
  ref="refs/tags/${module}/${tag}"
  existing=$(ref_commit "$ref")
  if [ -z "$existing" ]; then
    gh api "repos/${REPO}/git/refs" -f ref="$ref" -f sha="$commit" >/dev/null
    echo "created ${ref} at ${commit}"
  elif [ "$existing" = "$commit" ]; then
    echo "${ref} already points at ${commit}"
  else
    echo "::error::${ref} already points at ${existing}, not the release commit ${commit}; module tags are immutable"
    exit 1
  fi
done
