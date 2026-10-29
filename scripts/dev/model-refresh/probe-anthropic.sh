#!/usr/bin/env bash
# probe-anthropic.sh — one-request probes of a Claude model's wire surface
# for a model refresh (docs/model-refresh.md).
#
# Usage: ANTHROPIC_API_KEY=... probe-anthropic.sh <model>
#
# Sends three short requests (non-default temperature, effort xhigh,
# forced tool_choice) and prints the HTTP status and the error message or
# stop reason for each. The key is passed to curl through a header file
# descriptor, so it never appears in the process list, and is never
# printed.

set -euo pipefail

model="${1:?usage: probe-anthropic.sh <model>}"
: "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY must be set}"
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

probe() {
    local name=$1 body=$2 code
    code=$(curl -sS -o "$out" -w '%{http_code}' https://api.anthropic.com/v1/messages \
        -H @<(printf 'x-api-key: %s\n' "$ANTHROPIC_API_KEY") \
        -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
        -d "$body")
    printf '%s\t%s\t%s\t%s\n' "$model" "$name" "$code" "$(jq -r '.error.message // .stop_reason' "$out")"
}

msg='[{"role":"user","content":"hi"}]'
probe temperature '{"model":"'"$model"'","max_tokens":64,"messages":'"$msg"',"temperature":0.1}'
probe effort-xhigh '{"model":"'"$model"'","max_tokens":64,"messages":'"$msg"',"output_config":{"effort":"xhigh"}}'
probe tool-choice-any '{"model":"'"$model"'","max_tokens":64,"messages":'"$msg"',"tools":[{"name":"t","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}'
