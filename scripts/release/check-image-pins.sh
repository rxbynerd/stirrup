#!/usr/bin/env bash
# Fails when the digest behind a published version tag or sha-<commit>
# tag differs from the digest recorded when the tag was first pushed.
#
# Version tags are checked against each GitHub Release's IMAGE_DIGESTS
# asset, after verifying that asset against the release's signed
# SHA256SUMS and the image against its provenance attestation.
# sha-<commit> tags are checked against the commit status ci.yml writes
# on the commit when it first pushes the tag: the oldest such status is
# the record, and a context holding more than one digest, or a status not
# written by GitHub Actions, fails. Tags without a status are counted and
# reported, not failed.
#
# Needs gh (authenticated through GH_TOKEN), jq, curl and cosign, and
# GITHUB_REPOSITORY set to <owner>/<repo>.
set -euo pipefail

readonly REPO=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be <owner>/<repo>}
readonly IMAGES=("ghcr.io/${REPO}" "ghcr.io/${REPO}-sandbox")
readonly OIDC_ISSUER=https://token.actions.githubusercontent.com
readonly SEMVER_TAG='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'
readonly DIGEST='^sha256:[0-9a-f]{64}$'
readonly BATCH=50
readonly STATUS_WRITER='github-actions[bot]'

registry=$(cd "$(dirname "$0")" && pwd)/registry.sh
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

failures=0
checked=0
unrecorded=0

fail() {
  echo "::error::$*"
  failures=$((failures + 1))
}

# compare_digest <ref> <expected> checks the registry's current digest.
compare_digest() {
  local ref=$1 want=$2 got rc=0
  got=$("$registry" digest "$ref") || rc=$?
  case "$rc" in
    0)
      if [ "$got" = "$want" ]; then
        checked=$((checked + 1))
      else
        fail "${ref} moved: recorded ${want}, registry now serves ${got}"
      fi
      ;;
    3) fail "${ref} no longer exists (recorded ${want})" ;;
    *) fail "could not read the digest of ${ref}" ;;
  esac
}

check_release() {
  local tag=$1 dir="$workdir/release-$1" line ref want image found
  mkdir -p "$dir"
  if ! gh release download "$tag" --repo "$REPO" --dir "$dir" \
    --pattern SHA256SUMS --pattern SHA256SUMS.sigstore.json --pattern IMAGE_DIGESTS; then
    fail "release ${tag}: could not download SHA256SUMS, its signature bundle and IMAGE_DIGESTS"
    return
  fi
  for f in SHA256SUMS SHA256SUMS.sigstore.json IMAGE_DIGESTS; do
    [ -f "$dir/$f" ] || {
      fail "release ${tag}: asset ${f} is missing"
      return
    }
  done

  if ! cosign verify-blob \
    --bundle "$dir/SHA256SUMS.sigstore.json" \
    --certificate-identity "https://github.com/${REPO}/.github/workflows/_sign-release.yml@refs/tags/${tag}" \
    --certificate-oidc-issuer "$OIDC_ISSUER" \
    "$dir/SHA256SUMS"; then
    fail "release ${tag}: the SHA256SUMS signature does not verify"
    return
  fi
  if ! (cd "$dir" && grep -E '^[0-9a-f]{64}  IMAGE_DIGESTS$' SHA256SUMS | sha256sum -c -); then
    fail "release ${tag}: IMAGE_DIGESTS does not match the signed SHA256SUMS"
    return
  fi

  for image in "${IMAGES[@]}"; do
    found=false
    while IFS= read -r line <&3; do
      ref=${line%@*}
      want=${line##*@}
      [ "$ref" = "${image}:${tag}" ] || continue
      found=true
      if ! [[ "$want" =~ $DIGEST ]]; then
        fail "release ${tag}: malformed IMAGE_DIGESTS entry '${line}'"
        continue
      fi
      compare_digest "$ref" "$want"
      gh attestation verify "oci://${image}@${want}" \
        --repo "$REPO" \
        --cert-identity "https://github.com/${REPO}/.github/workflows/_sign-release.yml@refs/tags/${tag}" \
        --source-ref "refs/tags/${tag}" \
        --deny-self-hosted-runners >/dev/null ||
        fail "release ${tag}: no verifiable provenance attestation for ${image}@${want}"
    done 3<"$dir/IMAGE_DIGESTS"
    [ "$found" = true ] || fail "release ${tag}: IMAGE_DIGESTS has no entry for ${image}:${tag}"
  done
}

# recorded_digest <commit> <context> prints the digest the oldest status
# in <context> records, after checking every status in that context
# names the same digest and was written by GitHub Actions.
recorded_digest() {
  local commit=$1 context=$2 statuses
  statuses=$(gh api --paginate "repos/${REPO}/commits/${commit}/statuses" |
    jq -sc --arg c "$context" '[.[][] | select(.context == $c) | {d: .description, w: .creator.login}]') || return 1
  if [ "$(jq -r '[.[].d] | unique | length' <<<"$statuses")" != 1 ]; then
    echo "${context} on ${commit} records more than one digest: $(jq -c '[.[].d] | unique' <<<"$statuses")" >&2
    return 1
  fi
  if [ "$(jq -r --arg w "$STATUS_WRITER" '[.[] | select(.w != $w)] | length' <<<"$statuses")" != 0 ]; then
    echo "${context} on ${commit} has a status not written by ${STATUS_WRITER}" >&2
    return 1
  fi
  jq -r '.[-1].d' <<<"$statuses"
}

# check_commit_tags <image> compares every sha-<commit> tag with the
# digest recorded in the image-digest/<image name> commit status.
check_commit_tags() {
  local image=$1 context="image-digest/${1##*/}" list="$workdir/tags" tags=() batch query i j short want latest commit
  "$registry" tags "$image" >"$list" || {
    fail "could not list the tags of ${image}"
    return
  }
  while IFS= read -r tag; do
    tags+=("$tag")
  done < <(grep -E '^sha-[0-9a-f]{7}$' "$list" || true)

  for ((i = 0; i < ${#tags[@]}; i += BATCH)); do
    batch=("${tags[@]:i:BATCH}")
    # shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
    query='query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {'
    for j in "${!batch[@]}"; do
      short=${batch[j]#sha-}
      query+=" c${j}: object(expression: \"${short}\") { ... on Commit { oid status { context(name: \"${context}\") { description } } } }"
    done
    query+=' } }'
    if ! gh api graphql -f query="$query" -f owner="${REPO%/*}" -f name="${REPO#*/}" >"$workdir/statuses.json"; then
      fail "could not read the recorded digests for ${image}"
      return
    fi
    for j in "${!batch[@]}"; do
      latest=$(jq -r --arg k "c${j}" '.data.repository[$k].status.context.description // empty' "$workdir/statuses.json")
      if [ -z "$latest" ]; then
        unrecorded=$((unrecorded + 1))
        continue
      fi
      commit=$(jq -r --arg k "c${j}" '.data.repository[$k].oid' "$workdir/statuses.json")
      if ! want=$(recorded_digest "$commit" "$context"); then
        fail "${image}:${batch[j]}: the recorded digest is not trustworthy (see above)"
      elif [[ "$want" =~ $DIGEST ]]; then
        compare_digest "${image}:${batch[j]}" "$want"
      else
        fail "${image}:${batch[j]}: malformed recorded digest '${want}'"
      fi
    done
  done
}

gh release list --repo "$REPO" --exclude-drafts --limit 1000 --json tagName --jq '.[].tagName' >"$workdir/releases" ||
  fail "could not list releases"
while IFS= read -r tag <&3; do
  if [[ "$tag" =~ $SEMVER_TAG ]]; then
    check_release "$tag"
  fi
done 3<"$workdir/releases"

for image in "${IMAGES[@]}"; do
  check_commit_tags "$image"
done

summary="Image tag pins: ${checked} tag(s) match their recorded digest, ${failures} failure(s), ${unrecorded} sha- tag(s) with no recorded digest."
echo "$summary"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "$summary" >>"$GITHUB_STEP_SUMMARY"
fi
[ "$failures" -eq 0 ]
