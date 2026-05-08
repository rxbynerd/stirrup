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
# gVisor binaries are pulled from a pinned upstream release and verified
# against SHA-512 hashes hardcoded in this script. The hardcoded values
# are the trust anchor; the upstream `.sha512` file is intentionally
# NOT consulted, because fetching the binary and its checksum from the
# same GCS origin in the same execution would only prove transfer
# integrity, not that a known-good version was installed.
#
# To bump: pick a release from https://github.com/google/gvisor/releases,
# update GVISOR_VERSION, then refresh the four RUNSC_*/SHIM_* hashes by
# running (from a trusted machine):
#
#   for arch in x86_64 aarch64; do
#       for bin in runsc containerd-shim-runsc-v1; do
#           curl -fsS \
#             "https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}/${arch}/${bin}.sha512"
#       done
#   done
#
# This is dev-only; production clusters should additionally pin via a
# private mirror and out-of-band signature verification.

set -euo pipefail

CLUSTER_NAME="${STIRRUP_CLUSTER_NAME:-stirrup-sandbox}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIND_CONFIG="${SCRIPT_DIR}/kind-config.yaml"
RUNTIMECLASSES="${SCRIPT_DIR}/runtimeclasses.yaml"

# bump: https://github.com/google/gvisor/releases
GVISOR_VERSION="20260504.0"
GVISOR_BASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}"

# Expected SHA-512 hashes for the pinned release. These are the trust
# anchor — they live in this script (and therefore in git history),
# decoupled from the GCS origin that serves the binary itself.
RUNSC_SHA512_X86_64="27ade75278e3ee7d8ff1b1cdd979383f8beca2327a2a572d054280b43a5a6db615db6387fa82b230839fed30c9a0c65ac249195a81b51298e639ef1262182640"
RUNSC_SHA512_AARCH64="e2a6c39afa478e4040e8b2a803324011fb81ee268fdac5cd005f64e165218b3a45e45ba9cef15fcf7f514e80da13b752ce2733d035d6dcded48c89a468d4fd17"
SHIM_SHA512_X86_64="5b502b951bfd80666ad628d6f68ebe5a888d986163db9efc6647b07b26eac95e108367648d642b5d4dc8c44b631de55b7ea35e737d620d3b88e636fe11cd20e3"
SHIM_SHA512_AARCH64="1ffd9edb4d200661d1b6b85dd07dab02ec71e211909a20fe61a96ce554fcb03494378f10ca226c5c345afaf039902675959fb06eeafbde693acc63967d0d7cc9"

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
    arch="$("${engine}" exec "${node}" uname -m)"
    case "${arch}" in
        x86_64|amd64)        echo "x86_64" ;;
        aarch64|arm64)       echo "aarch64" ;;
        *) fail "unsupported node architecture: ${arch}" ;;
    esac
}

# Look up the expected SHA-512 for an (arch, binary-name) pair from the
# constants pinned at the top of this script. Failing closed here is the
# whole point of B1 — if a future maintainer adds a binary without
# pinning, we want the script to refuse to install rather than silently
# skip verification.
expected_sha512() {
    local arch="$1" name="$2"
    case "${arch}/${name}" in
        x86_64/runsc)                       printf '%s' "${RUNSC_SHA512_X86_64}" ;;
        x86_64/containerd-shim-runsc-v1)    printf '%s' "${SHIM_SHA512_X86_64}" ;;
        aarch64/runsc)                      printf '%s' "${RUNSC_SHA512_AARCH64}" ;;
        aarch64/containerd-shim-runsc-v1)   printf '%s' "${SHIM_SHA512_AARCH64}" ;;
        *) fail "no pinned SHA-512 for ${arch}/${name}; refusing to install unverified binary" ;;
    esac
}

# Download a single gVisor artifact into the node and verify its
# SHA-512 against the in-repo constant. We deliberately do NOT consult
# the upstream .sha512 file: trusting a checksum file served from the
# same GCS origin as the binary would only prove transfer integrity.
# The hardcoded constant (committed to git) is the trust anchor.
install_gvisor_binary() {
    local engine="$1" node="$2" arch="$3" name="$4" dest="$5"
    local url="${GVISOR_BASE_URL}/${arch}/${name}"
    local expected
    expected="$(expected_sha512 "${arch}" "${name}")"
    log "downloading ${name} (${arch}) into node..."
    # -fL fails fast on HTTP errors; retry on transient network blips.
    # Inner trap: clean up the partial download if curl fails so the
    # next retry starts from a known-empty state.
    "${engine}" exec "${node}" sh -c \
        "trap 'rm -f /tmp/${name}' EXIT INT TERM; \
         curl -fLsS --retry 3 --retry-delay 2 -o '/tmp/${name}' '${url}' \
         && trap - EXIT INT TERM"
    log "verifying SHA-512 for ${name} against pinned hash..."
    "${engine}" exec "${node}" sh -c \
        "printf '%s  /tmp/%s\n' '${expected}' '${name}' | sha512sum -c -"
    log "installing ${name} -> ${dest}"
    "${engine}" exec "${node}" sh -c \
        "install -m 0755 '/tmp/${name}' '${dest}' && rm -f '/tmp/${name}'"
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

    log "creating kind cluster '${CLUSTER_NAME}' (may take several minutes on first run — node image pull is the slowest step)..."
    # `kind create cluster --name X` overrides any `name:` field in the
    # config file (kind documents this precedence explicitly), so the
    # static `name: stirrup-sandbox` in kind-config.yaml does not block
    # the STIRRUP_CLUSTER_NAME override.
    kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 5m

    local node; node="$(node_container_name)"
    if ! "${engine}" inspect "${node}" >/dev/null 2>&1; then
        fail "expected node container '${node}' is not visible to ${engine}"
    fi

    local arch; arch="$(detect_node_arch "${engine}" "${node}")"
    log "node arch: ${arch} (gVisor pinned at ${GVISOR_VERSION})"

    install_gvisor_binary "${engine}" "${node}" "${arch}" \
        "runsc" "/usr/local/bin/runsc"
    install_gvisor_binary "${engine}" "${node}" "${arch}" \
        "containerd-shim-runsc-v1" "/usr/local/bin/containerd-shim-runsc-v1"

    log "writing /etc/containerd/conf.d/runsc.toml..."
    # The kind containerdConfigPatches in kind-config.yaml already adds
    # a `runsc` runtime entry, but kind only applies patches at cluster
    # creation time. Drop a conf.d snippet too so the configuration is
    # explicit and survives operator-side containerd config edits.
    "${engine}" exec "${node}" mkdir -p /etc/containerd/conf.d
    "${engine}" exec "${node}" sh -c 'cat >/etc/containerd/conf.d/runsc.toml <<EOF
# Stirrup sandbox: register the gVisor (runsc) runtime so a Pod with
# RuntimeClass handler "runsc" can be scheduled. Installed by
# scripts/dev/kind-up.sh.
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF'

    log "restarting containerd inside the node..."
    "${engine}" exec "${node}" systemctl restart containerd

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
