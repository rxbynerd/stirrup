#!/usr/bin/env bash
# Succeeds when <image>@<digest> carries a build-provenance attestation
# from this repository's release signing workflow for <tag> (and, when
# given, <commit>), on a GitHub-hosted runner, whose subjects include
# <image> itself under <digest>.
#
#   verify-image-attestation.sh <image> <digest> <tag> [<commit>]
#
# gh matches an attestation by digest alone; the subject-name check
# stops one release image's attestation from vouching for the other
# image name. Needs gh (authenticated through GH_TOKEN) and
# GITHUB_REPOSITORY set to <owner>/<repo>.
set -euo pipefail

[ $# -eq 3 ] || [ $# -eq 4 ] || {
  echo "usage: verify-image-attestation.sh <image> <digest> <tag> [<commit>]" >&2
  exit 1
}
readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
image=$1 digest=$2 tag=$3 commit=${4:-}
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "verify-image-attestation.sh: '${digest}' is not a sha256 digest" >&2
  exit 1
}

args=(
  --repo "$REPO"
  --cert-identity "https://github.com/${REPO}/.github/workflows/_sign-release.yml@refs/tags/${tag}"
  --source-ref "refs/tags/${tag}"
  --deny-self-hosted-runners
  --format json
  --jq "[.[].verificationResult.statement.subject[] | select(.name == \"${image}\" and .digest.sha256 == \"${digest#sha256:}\")] | length"
)
if [ -n "$commit" ]; then
  args+=(--source-digest "$commit")
fi

matches=$(gh attestation verify "oci://${image}@${digest}" "${args[@]}" </dev/null)
if ! [[ "$matches" =~ ^[1-9][0-9]*$ ]]; then
  echo "verify-image-attestation.sh: the attestation for ${digest} does not name ${image}" >&2
  exit 1
fi
