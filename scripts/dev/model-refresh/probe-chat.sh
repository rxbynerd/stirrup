#!/usr/bin/env bash
# probe-chat.sh — one-request probes of a model on an OpenAI-compatible
# Chat Completions endpoint (OpenAI, OpenRouter, Z.ai, DeepSeek) for a
# model refresh (docs/model-refresh.md).
#
# Usage: API_KEY=... BASE_URL=https://openrouter.ai/api/v1 probe-chat.sh <model> [effort ...]
#
# For each effort level (default: low high max) it sends a short
# streaming request with one tool and prints the HTTP status, the
# serving host where the endpoint names one, the delta keys the stream
# used (to show where reasoning arrives), and the reported reasoning
# tokens. A final request sends temperature without an effort. The key
# is passed to curl through a header file descriptor, so it never
# appears in the process list, and is never printed.

set -euo pipefail

model="${1:?usage: probe-chat.sh <model> [effort ...]}"
shift
efforts=("$@")
[ "${#efforts[@]}" -gt 0 ] || efforts=(low high max)
: "${API_KEY:?API_KEY must be set}"
: "${BASE_URL:?BASE_URL must be set, e.g. https://openrouter.ai/api/v1}"
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

tool='[{"type":"function","function":{"name":"get_weather","description":"Get the weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]'
msg='[{"role":"user","content":"What is the weather in Paris? Use the tool."}]'

probe() {
    local name=$1 body=$2 code
    code=$(curl -sS -N -o "$out" -w '%{http_code}' "${BASE_URL%/}/chat/completions" \
        -H @<(printf 'Authorization: Bearer %s\n' "$API_KEY") \
        -H 'content-type: application/json' -d "$body")
    local chunks
    chunks=$(grep '^data: {' "$out" | sed 's/^data: //' || true)
    if [ -z "$chunks" ]; then
        printf '%s\t%s\t%s\t%s\n' "$model" "$name" "$code" "$(jq -r '.error.message // "no stream"' "$out" 2>/dev/null || head -c 200 "$out")"
        return
    fi
    printf '%s\t%s\t%s\thost=%s\tdeltas=%s\treasoning_tokens=%s\n' "$model" "$name" "$code" \
        "$(jq -rs 'map(.provider // empty) | first // "-"' <<<"$chunks")" \
        "$(jq -rs '[.[].choices[0].delta // {} | keys[]] | unique | join(",")' <<<"$chunks")" \
        "$(jq -rs 'map(.usage.completion_tokens_details.reasoning_tokens // empty) | last // "-"' <<<"$chunks")"
}

for effort in "${efforts[@]}"; do
    probe "effort-$effort" '{"model":"'"$model"'","max_tokens":400,"stream":true,"stream_options":{"include_usage":true},"messages":'"$msg"',"tools":'"$tool"',"reasoning_effort":"'"$effort"'"}'
done
probe temperature '{"model":"'"$model"'","max_tokens":400,"stream":true,"stream_options":{"include_usage":true},"messages":'"$msg"',"temperature":0.1}'
