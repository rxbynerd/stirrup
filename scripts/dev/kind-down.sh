#!/usr/bin/env bash
# kind-down.sh — tear the Stirrup K8sExecutor sandbox cluster down.
#
# Idempotent: if no cluster exists this exits 0 with an informational
# message rather than erroring. Safe to run from CI/cleanup hooks.

set -euo pipefail

CLUSTER_NAME="${STIRRUP_CLUSTER_NAME:-stirrup-sandbox}"

log()  { printf '[kind-down] %s\n' "$*"; }
fail() { printf '[kind-down] ERROR: %s\n' "$*" >&2; exit 1; }

if ! command -v kind >/dev/null 2>&1; then
    fail "required command not found in PATH: kind"
fi

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    log "no kind cluster named '${CLUSTER_NAME}' is registered; nothing to do."
    exit 0
fi

log "deleting kind cluster '${CLUSTER_NAME}'..."
kind delete cluster --name "${CLUSTER_NAME}"
log "done."
