#!/usr/bin/env bash
# smoke-test.sh — verify the Stirrup sandbox cluster can land a Pod on
# the gVisor RuntimeClass and the kernel inside reports gVisor.
#
# The Pod is named with a per-invocation suffix so concurrent runs
# don't collide, and a trap removes it on exit regardless of pass/fail.

set -euo pipefail

CLUSTER_NAME="stirrup-sandbox"
CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="default"
POD_NAME="stirrup-sandbox-smoke-$$-$RANDOM"

log()  { printf '[smoke-test] %s\n' "$*"; }
fail() { printf '[smoke-test] FAIL: %s\n' "$*" >&2; exit 1; }

if ! command -v kubectl >/dev/null 2>&1; then
    fail "required command not found in PATH: kubectl"
fi

# shellcheck disable=SC2329  # invoked indirectly via trap below.
cleanup() {
    # Best-effort removal; do not fail the script in cleanup.
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        delete pod "${POD_NAME}" --ignore-not-found --wait=false \
        >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Sanity-check the cluster + RuntimeClass before scheduling.
if ! kubectl --context "${CONTEXT}" get nodes >/dev/null 2>&1; then
    fail "cluster context '${CONTEXT}' is not reachable; run scripts/dev/kind-up.sh first"
fi
if ! kubectl --context "${CONTEXT}" get runtimeclass gvisor >/dev/null 2>&1; then
    fail "RuntimeClass 'gvisor' is not registered; rerun scripts/dev/kind-up.sh"
fi

log "scheduling busybox pod '${POD_NAME}' on RuntimeClass=gvisor..."
# `sleep 30` keeps the pod alive long enough for kubectl exec; the
# trap removes it well before the sleep expires.
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  labels:
    app: stirrup-sandbox-smoke
spec:
  runtimeClassName: gvisor
  restartPolicy: Never
  containers:
    - name: probe
      image: busybox:1.36
      command: ["sleep", "30"]
EOF

log "waiting for pod Ready (up to 90s)..."
if ! kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        wait --for=condition=Ready "pod/${POD_NAME}" --timeout=90s; then
    log "pod did not become Ready; recent state:"
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        describe pod "${POD_NAME}" >&2 || true
    fail "pod ${POD_NAME} never reached Ready"
fi

log "reading kernel banner from inside the pod..."
# gVisor's user-space kernel prints a recognisable banner via dmesg.
# Captured banners across recent gVisor releases include strings such
# as "Starting gVisor" and "Linux version ... gVisor". Match on the
# literal "gVisor" token to stay robust across versions.
banner="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
    exec "${POD_NAME}" -- sh -c 'dmesg 2>/dev/null | head -n 20')"

if [[ -z "${banner}" ]]; then
    fail "dmesg returned no output; gVisor sandbox may not be active"
fi

log "kernel banner (first 20 lines):"
printf '%s\n' "${banner}" | sed 's/^/    /'

if printf '%s' "${banner}" | grep -qi 'gvisor'; then
    log "PASS: gVisor signature detected in kernel banner."
    exit 0
fi

fail "no gVisor signature in dmesg output. Pod scheduled but does not appear to be sandboxed."
