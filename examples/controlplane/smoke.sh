#!/usr/bin/env bash
# Runs one task end to end: the example control plane, a scripted
# OpenAI-compatible provider and the locally built `stirrup job` with the
# local executor. No container engine, network access or API key is needed.
#
# Usage: smoke.sh [success|provider-error]
#
# Exits 0 when the control plane and the job both report a successful run.
# The provider-error scenario scripts a provider failure, so it exits non-zero
# by design; `just controlplane-smoke-failure` asserts that.
#
# STIRRUP overrides the path of the stirrup binary (default: ./stirrup at the
# repository root, produced by `just build`).
set -euo pipefail

scenario="${1:-success}"
case "$scenario" in
    success) stub_mode=success ;;
    provider-error) stub_mode=error ;;
    *) echo "usage: $0 [success|provider-error]" >&2; exit 2 ;;
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

"$workdir/controlplane" -config "$workdir/runconfig.json" -listen 127.0.0.1:0 \
    -addr-file "$workdir/cp.addr" -permission deny &
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
            echo "smoke: timed out after 120s" >&2
            exit 1
        fi
        sleep 0.2
    done
}

status=0
await_exit "$cp_pid"
wait "$cp_pid" || status=$?
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
echo "smoke: ok ($scenario)"
