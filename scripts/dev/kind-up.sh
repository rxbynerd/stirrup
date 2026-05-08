#!/usr/bin/env bash
# kind-up.sh — bring up the Stirrup K8sExecutor sandbox cluster.
#
# Creates a single-node kind cluster named `stirrup-sandbox`, installs
# gVisor (`runsc` + `containerd-shim-runsc-v1`) into the node so Pods
# can schedule onto a `runsc` containerd runtime, and applies the
# `runc` / `gvisor` RuntimeClass resources used by the executor.
#
# Idempotency: if the cluster already exists this script bails with a
# clear error pointing at kind-down.sh. We deliberately do not auto-
# recreate — this is a developer machine and silently destroying state
# would be surprising.
#
# gVisor binaries are pulled from the upstream "latest" channel and
# verified against the project's published SHA-512 sums. This is dev-
# only; production clusters should pin to a specific release.

set -euo pipefail

CLUSTER_NAME="stirrup-sandbox"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIND_CONFIG="${SCRIPT_DIR}/kind-config.yaml"
RUNTIMECLASSES="${SCRIPT_DIR}/runtimeclasses.yaml"
GVISOR_BASE_URL="https://storage.googleapis.com/gvisor/releases/release/latest"

log()  { printf '[kind-up] %s\n' "$*"; }
fail() { printf '[kind-up] ERROR: %s\n' "$*" >&2; exit 1; }

require_cmd() {
    local cmd="$1"
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        fail "required command not found in PATH: ${cmd}"
    fi
}

# Pick a container engine. kind itself uses one of these; we need it
# directly for the `exec` calls that drop gVisor into the node.
detect_engine() {
    if command -v docker >/dev/null 2>&1; then
        echo "docker"
    elif command -v podman >/dev/null 2>&1; then
        echo "podman"
    else
        fail "neither docker nor podman is available"
    fi
}

# kind names the control-plane container "<cluster>-control-plane".
node_container_name() {
    printf '%s-control-plane' "${CLUSTER_NAME}"
}

# Detect the node's architecture by inspecting /bin/sh inside the
# container. gVisor publishes assets under x86_64 / aarch64 paths.
detect_node_arch() {
    local engine="$1" node="$2" arch
    arch="$(${engine} exec "${node}" uname -m)"
    case "${arch}" in
        x86_64|amd64)        echo "x86_64" ;;
        aarch64|arm64)       echo "aarch64" ;;
        *) fail "unsupported node architecture: ${arch}" ;;
    esac
}

# Download a single gVisor artifact + its .sha512 file into the node
# and verify the checksum from inside the node (so the published sum
# applies to the bytes that actually landed on disk).
install_gvisor_binary() {
    local engine="$1" node="$2" arch="$3" name="$4" dest="$5"
    local url="${GVISOR_BASE_URL}/${arch}/${name}"
    log "downloading ${name} (${arch}) into node..."
    # -fL fails fast on HTTP errors; retry on transient network blips.
    ${engine} exec "${node}" sh -c \
        "curl -fLsS --retry 3 --retry-delay 2 -o '/tmp/${name}' '${url}' \
         && curl -fLsS --retry 3 --retry-delay 2 -o '/tmp/${name}.sha512' '${url}.sha512'"
    log "verifying SHA-512 for ${name}..."
    # The published .sha512 file has the upstream filename in column 2;
    # rewrite to /tmp/<name> so sha512sum -c finds the bytes we wrote.
    ${engine} exec "${node}" sh -c \
        "cd /tmp && awk -v f='${name}' '{print \$1, f}' '${name}.sha512' > '${name}.sha512.local' \
         && sha512sum -c '${name}.sha512.local'"
    log "installing ${name} -> ${dest}"
    ${engine} exec "${node}" sh -c \
        "install -m 0755 '/tmp/${name}' '${dest}' \
         && rm -f '/tmp/${name}' '/tmp/${name}.sha512' '/tmp/${name}.sha512.local'"
}

main() {
    require_cmd kind
    require_cmd kubectl
    local engine; engine="$(detect_engine)"
    log "container engine: ${engine}"

    if [[ ! -f "${KIND_CONFIG}" ]]; then
        fail "kind config not found at ${KIND_CONFIG}"
    fi
    if [[ ! -f "${RUNTIMECLASSES}" ]]; then
        fail "runtimeclasses manifest not found at ${RUNTIMECLASSES}"
    fi

    if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
        fail "kind cluster '${CLUSTER_NAME}' already exists. \
Run scripts/dev/kind-down.sh first if you want to recreate it."
    fi

    log "creating kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --config "${KIND_CONFIG}" --wait 5m

    local node; node="$(node_container_name)"
    if ! ${engine} inspect "${node}" >/dev/null 2>&1; then
        fail "expected node container '${node}' is not visible to ${engine}"
    fi

    local arch; arch="$(detect_node_arch "${engine}" "${node}")"
    log "node arch: ${arch}"

    install_gvisor_binary "${engine}" "${node}" "${arch}" \
        "runsc" "/usr/local/bin/runsc"
    install_gvisor_binary "${engine}" "${node}" "${arch}" \
        "containerd-shim-runsc-v1" "/usr/local/bin/containerd-shim-runsc-v1"

    log "writing /etc/containerd/conf.d/runsc.toml..."
    # The kind containerdConfigPatches in kind-config.yaml already adds
    # a `runsc` runtime entry, but kind only applies patches at cluster
    # creation time. Drop a conf.d snippet too so the configuration is
    # explicit and survives operator-side containerd config edits.
    ${engine} exec "${node}" mkdir -p /etc/containerd/conf.d
    ${engine} exec "${node}" sh -c 'cat >/etc/containerd/conf.d/runsc.toml <<EOF
# Stirrup sandbox: register the gVisor (runsc) runtime so a Pod with
# RuntimeClass handler "runsc" can be scheduled. Installed by
# scripts/dev/kind-up.sh.
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF'

    log "restarting containerd inside the node..."
    ${engine} exec "${node}" systemctl restart containerd

    log "waiting for kubelet to recover..."
    # systemctl restart can briefly knock the API server offline; loop
    # until kubectl responds before applying manifests.
    local attempts=0
    until kubectl --context "kind-${CLUSTER_NAME}" get nodes >/dev/null 2>&1; do
        attempts=$((attempts + 1))
        if (( attempts > 30 )); then
            fail "API server did not come back within 60s of containerd restart"
        fi
        sleep 2
    done

    log "applying RuntimeClass resources..."
    kubectl --context "kind-${CLUSTER_NAME}" apply -f "${RUNTIMECLASSES}"

    log "done."
    cat <<EOF

Cluster '${CLUSTER_NAME}' is up.

Next steps:
    kubectl config use-context kind-${CLUSTER_NAME}
    kubectl get runtimeclass
    ./scripts/dev/smoke-test.sh

Tear down with:
    ./scripts/dev/kind-down.sh
EOF
}

main "$@"
