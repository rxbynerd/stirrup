#!/usr/bin/env bash
# smoke-test.sh — verify the Stirrup sandbox cluster can land Pods on
# both registered RuntimeClasses.
#
# Two checks run sequentially:
#
#   1. A Pod with `runtimeClassName: gvisor` must reach Ready and the
#      kernel banner inside the Pod must contain a gVisor signature
#      (sandboxing actually engaged, not just scheduled).
#   2. A Pod with `runtimeClassName: runc` must reach Ready. The runc
#      path uses the host kernel directly so a banner check is not
#      meaningful; reaching Ready is sufficient evidence that the
#      runc RuntimeClass is registered and routable.
#
# Both Pods are named with a per-invocation suffix so concurrent runs
# do not collide, and a trap removes them on exit regardless of
# pass/fail.

set -euo pipefail

CLUSTER_NAME="${STIRRUP_CLUSTER_NAME:-stirrup-sandbox}"
CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="default"
GVISOR_POD="stirrup-sandbox-smoke-gvisor-$$-$RANDOM"
RUNC_POD="stirrup-sandbox-smoke-runc-$$-$RANDOM"

# busybox:1.36 multi-arch image index digest (works on both amd64 and
# arm64 kind nodes). Sourced from Docker Hub's tags API:
#   curl -fsS https://hub.docker.com/v2/repositories/library/busybox/tags/1.36
# media_type: application/vnd.oci.image.index.v1+json
BUSYBOX_IMAGE="busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"

log()  { printf '[smoke-test] %s\n' "$*"; }
fail() { printf '[smoke-test] FAIL: %s\n' "$*" >&2; exit 1; }

if ! command -v kubectl >/dev/null 2>&1; then
    fail "required command not found in PATH: kubectl"
fi

# shellcheck disable=SC2329  # invoked indirectly via trap below.
cleanup() {
    # Best-effort removal; do not fail the script in cleanup.
    for pod in "${GVISOR_POD}" "${RUNC_POD}"; do
        kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
            delete pod "${pod}" --ignore-not-found --wait=false \
            >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

# Sanity-check the cluster + RuntimeClasses before scheduling.
if ! kubectl --context "${CONTEXT}" get nodes >/dev/null 2>&1; then
    fail "cluster context '${CONTEXT}' is not reachable; run scripts/dev/kind-up.sh first"
fi

# Wait for at least one node to reach Ready before scheduling Pods.
# Without this, a NotReady node turns the per-Pod 90s wait below into
# an opaque timeout. 30s is generous: kind-up.sh already passed
# `kind create cluster --wait 5m`, so the API server should be back.
log "waiting for cluster nodes to reach Ready..."
if ! kubectl --context "${CONTEXT}" \
        wait --for=condition=Ready node --all --timeout=30s >/dev/null; then
    fail "cluster nodes did not reach Ready — is the cluster fully up?"
fi

# Assert each RuntimeClass exists AND its handler matches the expected
# value. A typo'd handler would otherwise pass this pre-flight and then
# fail much later with a cryptic shim-not-found error during scheduling.
assert_runtimeclass_handler() {
    local class="$1" expected="$2" actual
    if ! actual="$(kubectl --context "${CONTEXT}" \
            get runtimeclass "${class}" -o jsonpath='{.handler}' 2>/dev/null)"; then
        fail "RuntimeClass '${class}' is not registered; rerun scripts/dev/kind-up.sh"
    fi
    if [[ "${actual}" != "${expected}" ]]; then
        fail "RuntimeClass '${class}' handler is '${actual}', expected '${expected}'"
    fi
}
assert_runtimeclass_handler "gvisor" "runsc"
assert_runtimeclass_handler "runc"   "runc"

# --- gVisor RuntimeClass check -------------------------------------------

log "scheduling busybox pod '${GVISOR_POD}' on RuntimeClass=gvisor..."
# `sleep 30` keeps the pod alive long enough for kubectl exec; the
# trap removes it well before the sleep expires.
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${GVISOR_POD}
  labels:
    app: stirrup-sandbox-smoke
spec:
  runtimeClassName: gvisor
  restartPolicy: Never
  containers:
    - name: probe
      image: ${BUSYBOX_IMAGE}
      command: ["sleep", "30"]
EOF

log "waiting for gvisor pod Ready (up to 90s)..."
if ! kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        wait --for=condition=Ready "pod/${GVISOR_POD}" --timeout=90s; then
    log "pod did not become Ready; recent state:"
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        describe pod "${GVISOR_POD}" >&2 || true
    fail "pod ${GVISOR_POD} never reached Ready"
fi

log "reading kernel banner from inside the gvisor pod..."
# gVisor's user-space kernel prints a recognisable banner via dmesg.
# Captured banners across recent gVisor releases include strings such
# as "Starting gVisor" and "Linux version ... gVisor". Match on the
# literal "gVisor" token to stay robust across versions. 50 lines is
# wider than the spec's `head -1` to tolerate boot noise that some
# gVisor builds emit before the product banner.
banner="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
    exec "${GVISOR_POD}" -- sh -c 'dmesg 2>/dev/null | head -n 50')"

if [[ -z "${banner}" ]]; then
    fail "dmesg returned no output; gVisor sandbox may not be active"
fi

log "kernel banner (first 50 lines):"
printf '%s\n' "${banner}" | sed 's/^/    /'

if ! printf '%s' "${banner}" | grep -qi 'gvisor'; then
    fail "no gVisor signature in dmesg output. Pod scheduled but does not appear to be sandboxed."
fi
log "gvisor RuntimeClass: PASS (gVisor signature detected in kernel banner)."

# --- runc RuntimeClass check ---------------------------------------------

log "scheduling busybox pod '${RUNC_POD}' on RuntimeClass=runc..."
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${RUNC_POD}
  labels:
    app: stirrup-sandbox-smoke
spec:
  runtimeClassName: runc
  restartPolicy: Never
  containers:
    - name: probe
      image: ${BUSYBOX_IMAGE}
      command: ["sleep", "30"]
EOF

log "waiting for runc pod Ready (up to 90s)..."
# runc uses the host kernel directly; reaching Ready is sufficient
# evidence that the RuntimeClass is registered and the runc handler
# is routable. No banner check is meaningful here.
if ! kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        wait --for=condition=Ready "pod/${RUNC_POD}" --timeout=90s; then
    log "pod did not become Ready; recent state:"
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" \
        describe pod "${RUNC_POD}" >&2 || true
    fail "pod ${RUNC_POD} never reached Ready"
fi
log "runc RuntimeClass: PASS (pod reached Ready)."

log "PASS: both RuntimeClasses (gvisor, runc) verified."
