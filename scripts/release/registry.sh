#!/usr/bin/env bash
# Reads manifest digests and tag lists from an OCI registry over the
# distribution API, telling an absent tag apart from a failed lookup so
# callers can fail closed.
#
#   registry.sh digest <registry>/<repository>:<tag>
#       Prints the tag's manifest digest (sha256:<hex>).
#       Exits 3 when the registry reports the tag as unknown.
#   registry.sh tags <registry>/<repository>
#       Prints every tag in the repository, one per line.
#
# Any other failure exits 1. Token requests are anonymous unless
# REGISTRY_USERNAME and REGISTRY_PASSWORD are set, in which case they are
# sent as basic credentials to the registry's token endpoint.
set -euo pipefail

readonly EXIT_ABSENT=3
readonly MANIFEST_ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
readonly CURL_OPTS=(--silent --show-error --connect-timeout 10 --max-time 60 --retry 3 --retry-delay 2 --proto '=https' --max-filesize 4194304)
readonly MAX_PAGES=100

die() {
  echo "registry.sh: $*" >&2
  exit 1
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# header_value <headers-file> <name> prints the last value of a response
# header, case-insensitively, without the trailing carriage return.
header_value() {
  tr -d '\r' <"$1" | awk -v want="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" '
    {
      split($0, kv, ":")
      if (tolower(kv[1]) == want) {
        sub(/^[^:]*:[ \t]*/, "")
        value = $0
      }
    }
    END { print value }'
}

# challenge_param <www-authenticate value> <param> extracts a quoted
# parameter such as realm="..." from a Bearer challenge.
challenge_param() {
  printf '%s' "$1" | sed -n "s/.*$2=\"\([^\"]*\)\".*/\1/p"
}

# credentials_config prints a curl config carrying the basic
# credentials, so the password never appears on a command line.
credentials_config() {
  local user pass
  if [ -n "${REGISTRY_USERNAME:-}" ] && [ -n "${REGISTRY_PASSWORD:-}" ]; then
    user=${REGISTRY_USERNAME//\\/\\\\}
    user=${user//\"/\\\"}
    pass=${REGISTRY_PASSWORD//\\/\\\\}
    pass=${pass//\"/\\\"}
    printf 'user = "%s:%s"\n' "$user" "$pass"
  fi
}

# auth_header <registry> <repository> prints an Authorization header
# for a pull of the repository, or nothing when the registry does not
# challenge anonymous requests.
auth_header() {
  local registry=$1 repository=$2 headers challenge realm service token
  headers=$(mktemp)
  curl "${CURL_OPTS[@]}" -o /dev/null -D "$headers" "https://${registry}/v2/" ||
    die "cannot reach https://${registry}/v2/"
  challenge=$(header_value "$headers" www-authenticate)
  rm -f "$headers"
  [ -n "$challenge" ] || return 0
  case "$challenge" in
    Bearer\ * | bearer\ *) ;;
    *) die "unsupported authentication challenge from ${registry}: ${challenge}" ;;
  esac
  realm=$(challenge_param "$challenge" realm)
  service=$(challenge_param "$challenge" service)
  [ -n "$realm" ] || die "no realm in the challenge from ${registry}"
  case "$realm" in
    https://*) ;;
    *) die "refusing non-HTTPS token realm '${realm}'" ;;
  esac
  token=$(credentials_config | curl "${CURL_OPTS[@]}" --config - --fail -G "$realm" \
    --data-urlencode "service=${service}" \
    --data-urlencode "scope=repository:${repository}:pull" | jq -r '.token // .access_token // empty') ||
    die "token request to ${realm} failed"
  [ -n "$token" ] || die "token response from ${realm} carried no token"
  printf 'Authorization: Bearer %s' "$token"
}

# split_name <registry>/<repository> sets REGISTRY and REPOSITORY.
split_name() {
  case "$1" in
    */*) ;;
    *) die "image name '$1' has no registry host" ;;
  esac
  REGISTRY=${1%%/*}
  REPOSITORY=${1#*/}
  [[ "$REPOSITORY" =~ ^[a-z0-9]+([._/-][a-z0-9]+)*$ ]] || die "invalid repository '${REPOSITORY}'"
}

cmd_digest() {
  local ref=$1 name tag auth workdir status digest advertised
  [[ "$ref" == *:* && "${ref##*/}" == *:* ]] || die "reference '${ref}' has no tag"
  name=${ref%:*}
  tag=${ref##*:}
  [[ "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid tag '${tag}'"
  split_name "$name"
  auth=$(auth_header "$REGISTRY" "$REPOSITORY")

  workdir=$(mktemp -d)
  trap 'rm -rf "$workdir"' RETURN
  status=$(curl "${CURL_OPTS[@]}" -o "$workdir/body" -D "$workdir/headers" -w '%{http_code}' \
    -H "Accept: ${MANIFEST_ACCEPT}" ${auth:+-H "$auth"} \
    "https://${REGISTRY}/v2/${REPOSITORY}/manifests/${tag}") ||
    die "manifest request for ${ref} failed"
  case "$status" in
    200) ;;
    404) return "$EXIT_ABSENT" ;;
    *) die "manifest request for ${ref} returned HTTP ${status}: $(head -c 300 "$workdir/body")" ;;
  esac

  digest="sha256:$(sha256_hex "$workdir/body")"
  advertised=$(header_value "$workdir/headers" docker-content-digest)
  if [ -n "$advertised" ] && [ "$advertised" != "$digest" ]; then
    die "${ref}: registry advertised ${advertised} but the manifest hashes to ${digest}"
  fi
  printf '%s\n' "$digest"
}

cmd_tags() {
  local name=$1 auth workdir path status next pages
  split_name "$name"
  auth=$(auth_header "$REGISTRY" "$REPOSITORY")

  workdir=$(mktemp -d)
  trap 'rm -rf "$workdir"' RETURN
  path="/v2/${REPOSITORY}/tags/list?n=1000"
  pages=0
  while [ -n "$path" ]; do
    pages=$((pages + 1))
    [ "$pages" -le "$MAX_PAGES" ] || die "tag list for ${name} exceeds ${MAX_PAGES} pages"
    status=$(curl "${CURL_OPTS[@]}" -o "$workdir/body" -D "$workdir/headers" -w '%{http_code}' \
      ${auth:+-H "$auth"} "https://${REGISTRY}${path}") ||
      die "tag list request for ${name} failed"
    [ "$status" = 200 ] || die "tag list request for ${name} returned HTTP ${status}: $(head -c 300 "$workdir/body")"
    jq -r '.tags // [] | .[]' "$workdir/body"
    next=$(header_value "$workdir/headers" link | sed -n 's/^<\([^>]*\)>;.*rel="next".*/\1/p')
    case "$next" in
      "" | /v2/*) path=$next ;;
      *) die "unexpected pagination link from ${REGISTRY}: ${next}" ;;
    esac
  done
}

case "${1:-}" in
  digest)
    [ $# -eq 2 ] || die "usage: registry.sh digest <registry>/<repository>:<tag>"
    cmd_digest "$2"
    ;;
  tags)
    [ $# -eq 2 ] || die "usage: registry.sh tags <registry>/<repository>"
    cmd_tags "$2"
    ;;
  *)
    die "usage: registry.sh {digest <ref> | tags <name>}"
    ;;
esac
