#!/usr/bin/env bash
# Keeps a main commit's sha-<7 hex> image tag pushed once, with its
# digest recorded as an image-digest/<image name> commit status that
# check-image-pins.sh compares against the registry.
#
#   commit-tag.sh check <image> <commit>
#       Prints GITHUB_OUTPUT lines: publish=true when the tag is absent
#       and unrecorded, publish=false when it exists. An existing tag
#       with no recorded digest also prints unrecorded=<digest>, so the
#       caller records the digest the registry serves now. A recorded
#       tag missing from the registry, or a failed lookup, exits 1.
#   commit-tag.sh record <image> <commit> <digest>
#       Writes the commit status.
#
# Needs gh (authenticated through GH_TOKEN with statuses: write) and
# GITHUB_REPOSITORY set to <owner>/<repo>.
set -euo pipefail

readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
registry=$(cd "$(dirname "$0")" && pwd)/registry.sh

usage() {
  echo "usage: commit-tag.sh {check <image> <commit> | record <image> <commit> <digest>}" >&2
  exit 1
}

[ $# -ge 3 ] || usage
command=$1 image=$2 commit=$3
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || {
  echo "commit-tag.sh: '${commit}' is not a full commit SHA" >&2
  exit 1
}
tag="sha-${commit::7}"
context="image-digest/${image##*/}"

case "$command" in
  check)
    [ $# -eq 3 ] || usage
    rc=0
    digest=$("$registry" digest "${image}:${tag}") || rc=$?
    case "$rc" in
      0)
        echo "publish=false"
        echo "::notice::${image}:${tag} already points at ${digest}; this build moves :main and :latest only." >&2
        recorded=$(gh api --paginate "repos/${REPO}/commits/${commit}/statuses" \
          --jq ".[] | select(.context == \"${context}\") | .description")
        if [ -z "$recorded" ]; then
          echo "::notice::${image}:${tag} has no ${context} status; recording the digest the registry serves now." >&2
          echo "unrecorded=${digest}"
        fi
        ;;
      3)
        recorded=$(gh api --paginate "repos/${REPO}/commits/${commit}/statuses" \
          --jq ".[] | select(.context == \"${context}\") | .description")
        if [ -n "$recorded" ]; then
          echo "::error::${image}:${tag} is recorded as $(head -n 1 <<<"$recorded") but is missing from the registry; it is not pushed again." >&2
          exit 1
        fi
        echo "publish=true"
        ;;
      *) exit 1 ;;
    esac
    ;;
  record)
    [ $# -eq 4 ] || usage
    digest=$4
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "::error::refusing to record '${digest}' for ${image}:${tag}: not a sha256 digest" >&2
      exit 1
    }
    gh api "repos/${REPO}/statuses/${commit}" \
      -f state=success \
      -f context="$context" \
      -f description="$digest" \
      -f target_url="${GITHUB_SERVER_URL:-https://github.com}/${REPO}/actions/runs/${GITHUB_RUN_ID:-}" >/dev/null
    ;;
  *) usage ;;
esac
