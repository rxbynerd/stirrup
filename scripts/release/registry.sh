#!/usr/bin/env bash
# Reads manifest digests and tag lists from an OCI registry over the
# distribution API, telling an absent tag apart from a failed lookup so
# callers can fail closed, and points tags at stored manifests.
#
#   registry.sh digest <registry>/<repository>:<tag>
#       Prints the tag's manifest digest (sha256:<hex>).
#       Exits 3 when the registry reports the tag as unknown.
#   registry.sh tags <registry>/<repository>
#       Prints every tag in the repository, one per line.
#   registry.sh tag <registry>/<repository>@<digest> <tag>
#       Points <tag> at the manifest stored under <digest> by uploading
#       its exact bytes, then checks <tag> resolves to <digest>. It does
#       not look at what <tag> named before; callers decide that.
#
# Any other failure exits 1. Token requests are anonymous unless
# REGISTRY_USERNAME and REGISTRY_PASSWORD are set, in which case they are
# sent as basic credentials to the registry's token endpoint.
set -euo pipefail

readonly EXIT_ABSENT=3
readonly MANIFEST_TYPES=(application/vnd.oci.image.index.v1+json application/vnd.docker.distribution.manifest.list.v2+json application/vnd.oci.image.manifest.v1+json application/vnd.docker.distribution.manifest.v2+json)
MANIFEST_ACCEPT=$(IFS=,; printf '%s' "${MANIFEST_TYPES[*]}")
readonly MANIFEST_ACCEPT
readonly TAG_PATTERN='^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'
readonly DIGEST_PATTERN='^sha256:[0-9a-f]{64}$'
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

# auth_header <registry> <repository> [<actions>] prints an Authorization
# header for the repository and actions (default pull), or nothing when
# the registry does not challenge anonymous requests.
auth_header() {
  local registry=$1 repository=$2 actions=${3:-pull} headers challenge realm service token
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
  # Artifact Registry's challenge carries no service parameter.
  token=$(credentials_config | curl "${CURL_OPTS[@]}" --config - --fail -G "$realm" \
    ${service:+--data-urlencode "service=${service}"} \
    --data-urlencode "scope=repository:${repository}:${actions}" | jq -r '.token // .access_token // empty') ||
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
  [[ "$tag" =~ $TAG_PATTERN ]] || die "invalid tag '${tag}'"
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

cmd_tag() {
  local src=$1 tag=$2 name digest auth workdir status content_type got
  [[ "$src" == *@* ]] || die "reference '${src}' has no digest"
  name=${src%@*}
  digest=${src##*@}
  [[ "$digest" =~ $DIGEST_PATTERN ]] || die "invalid digest '${digest}'"
  [[ "$tag" =~ $TAG_PATTERN ]] || die "invalid tag '${tag}'"
  split_name "$name"
  auth=$(auth_header "$REGISTRY" "$REPOSITORY" pull,push)

  workdir=$(mktemp -d)
  trap 'rm -rf "$workdir"' RETURN
  status=$(curl "${CURL_OPTS[@]}" -o "$workdir/manifest" -D "$workdir/headers" -w '%{http_code}' \
    -H "Accept: ${MANIFEST_ACCEPT}" ${auth:+-H "$auth"} \
    "https://${REGISTRY}/v2/${REPOSITORY}/manifests/${digest}") ||
    die "manifest request for ${src} failed"
  [ "$status" = 200 ] || die "manifest request for ${src} returned HTTP ${status}: $(head -c 300 "$workdir/manifest")"
  got="sha256:$(sha256_hex "$workdir/manifest")"
  [ "$got" = "$digest" ] || die "${src}: the registry served a manifest hashing to ${got}"
  content_type=$(header_value "$workdir/headers" content-type)
  content_type=${content_type%%;*}
  [[ " ${MANIFEST_TYPES[*]} " == *" ${content_type} "* ]] ||
    die "${src}: unsupported manifest media type '${content_type}'"

  status=$(curl "${CURL_OPTS[@]}" -o "$workdir/body" -D "$workdir/headers" -w '%{http_code}' \
    -X PUT -H "Content-Type: ${content_type}" ${auth:+-H "$auth"} \
    --data-binary "@$workdir/manifest" \
    "https://${REGISTRY}/v2/${REPOSITORY}/manifests/${tag}") ||
    die "tagging ${name}:${tag} failed"
  case "$status" in
    200 | 201) ;;
    *) die "tagging ${name}:${tag} returned HTTP ${status}: $(head -c 300 "$workdir/body")" ;;
  esac

  got=$(cmd_digest "${name}:${tag}") || die "cannot read back ${name}:${tag}"
  [ "$got" = "$digest" ] || die "${name}:${tag} resolves to ${got} after tagging, not ${digest}"
  echo "${name}:${tag} -> ${digest}" >&2
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
  tag)
    [ $# -eq 3 ] || die "usage: registry.sh tag <registry>/<repository>@<digest> <tag>"
    cmd_tag "$2" "$3"
    ;;
  *)
    die "usage: registry.sh {digest <ref> | tags <name> | tag <name>@<digest> <tag>}"
    ;;
esac
