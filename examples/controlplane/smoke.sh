#!/usr/bin/env bash
# Runs one task end to end: the example control plane, a scripted
# OpenAI-compatible provider and the locally built `stirrup job` with the
# local executor. No container engine, network access or API key is needed.
#
# Usage: smoke.sh [success|followup|abandoned-turn|provider-error]
#
#   success         one run that ends successfully.
#   followup        a run followed by one follow-up run on the same stream,
#                   continuing the first run's history.
#   abandoned-turn  the provider returns an empty message, so the turn is
#                   reported as turn_abandoned with reason empty_response.
#   provider-error  a scripted provider failure; exits non-zero by design,
#                   which `just controlplane-smoke-failure` asserts.
#
# Exits 0 when the control plane and the job both report success and the
# scenario's expected events appear in the control plane's output.
#
# STIRRUP overrides the path of the stirrup binary (default: ./stirrup at the
# repository root, produced by `just build`).
set -euo pipefail

scenario="${1:-success}"
followups=()
case "$scenario" in
    success) stub_mode=success ;;
    followup) stub_mode=success; followups=(-followup "Say it again.") ;;
    abandoned-turn) stub_mode=empty ;;
    provider-error) stub_mode=error ;;
    *) echo "usage: $0 [success|followup|abandoned-turn|provider-error]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
stirrup="${STIRRUP:-$here/../../stirrup}"
if [[ ! -x "$stirrup" ]]; then
    echo "stirrup binary not found at $stirrup; run 'just build' first" >&2
    exit 2
fi

workdir="$(mktemp -d)"
pids=()
cleanup() {
    for pid in ${pids[@]+"${pids[@]}"}; do
        kill "$pid" 2>/dev/null || true
    done
    rm -rf "$workdir"
}
trap cleanup EXIT

(cd "$here" && go build -o "$workdir/controlplane" . && go build -o "$workdir/stubprovider" ./stubprovider)

wait_for_file() {
    for _ in $(seq 1 100); do
        [[ -s "$1" ]] && return 0
        sleep 0.1
    done
    echo "timed out waiting for $1" >&2
    return 1
}

"$workdir/stubprovider" -mode "$stub_mode" -addr-file "$workdir/stub.addr" &
pids+=($!)
wait_for_file "$workdir/stub.addr"

mkdir "$workdir/workspace"
sed -e "s#__STUB_ADDR__#$(cat "$workdir/stub.addr")#" \
    -e "s#__WORKSPACE__#$workdir/workspace#" \
    "$here/smoke-runconfig.json" > "$workdir/runconfig.json"
if [[ "$scenario" == followup ]]; then
    sed -i.bak -e 's#"maxTurns": 3,#"maxTurns": 3, "followUpGrace": 30, "followUpHistory": "continue",#' "$workdir/runconfig.json"
fi

cp_log="$workdir/controlplane.log"
"$workdir/controlplane" -config "$workdir/runconfig.json" -listen 127.0.0.1:0 \
    -addr-file "$workdir/cp.addr" -permission deny ${followups[@]+"${followups[@]}"} 2>"$cp_log" &
cp_pid=$!
pids+=("$cp_pid")
wait_for_file "$workdir/cp.addr"

(cd "$workdir" && CONTROL_PLANE_ADDR="$(cat "$workdir/cp.addr")" STUB_API_KEY=unused "$stirrup" job) &
job_pid=$!
pids+=("$job_pid")

# Poll rather than block in wait so a hung process cannot stall the caller.
deadline=$((SECONDS + 120))
await_exit() {
    while kill -0 "$1" 2>/dev/null; do
        if ((SECONDS >= deadline)); then
            cat "$cp_log" >&2
            echo "smoke: timed out after 120s" >&2
            exit 1
        fi
        sleep 0.2
    done
}

status=0
await_exit "$cp_pid"
wait "$cp_pid" || status=$?
cat "$cp_log" >&2
job_status=0
await_exit "$job_pid"
wait "$job_pid" || job_status=$?

if [[ "$status" -ne 0 ]]; then
    echo "smoke: control plane exited with status $status" >&2
    exit "$status"
fi
if [[ "$job_status" -ne 0 ]]; then
    echo "smoke: stirrup job exited with status $job_status" >&2
    exit "$job_status"
fi

expect() {
    if ! grep -qF -- "$1" "$cp_log"; then
        echo "smoke: control plane output lacks '$1'" >&2
        exit 1
    fi
}
case "$scenario" in
    followup)
        if [[ "$(grep -c '^\[run_started\]' "$cp_log")" -ne 2 ]]; then
            echo "smoke: want two run_started events, one per run" >&2
            exit 1
        fi
        expect "[user_message] followup_prompt: Say it again."
        ;;
    abandoned-turn)
        expect "[turn_abandoned] 0 empty_response"
        ;;
esac
echo "smoke: ok ($scenario)"
