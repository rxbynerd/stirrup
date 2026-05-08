# Stirrup K8sExecutor sandbox (kind)

Local kind cluster for exercising the Pod-per-task `K8sExecutor` and
its `RuntimeClassName` selection. This is dev infrastructure; nothing
under `scripts/dev/` ships in a production image.

## What you get

- A single-node kind cluster named `stirrup-sandbox`.
- Two `RuntimeClass` resources: `runc` (default OCI runtime) and
  `gvisor` (handler `runsc`, gVisor user-space kernel).
- gVisor (`runsc` + `containerd-shim-runsc-v1`) installed into the
  kind node from a pinned upstream release. SHA-512 verified against
  pinned hashes committed to this repository; the upstream `.sha512`
  file is not used as the trust anchor.

The cluster is enough to validate that a Pod with
`runtimeClassName: gvisor` schedules and runs under gVisor. It is
**not** a substitute for production cluster prep.

## Prerequisites

- [`kind`](https://kind.sigs.k8s.io/) on the host.
- `kubectl` on the host.
- `docker` or `podman` on the host (kind drives one of them, and the
  bring-up script `exec`s into the node container directly to drop
  gVisor binaries in place).
- Internet egress to `storage.googleapis.com` for the gVisor download.

## Usage

```sh
# Bring the cluster up (creates kind cluster + installs gVisor +
# applies RuntimeClass manifests).
./scripts/dev/kind-up.sh

# Verify gVisor sandboxing actually works end-to-end.
./scripts/dev/smoke-test.sh

# Tear it down (idempotent — safe to run when nothing is up).
./scripts/dev/kind-down.sh
```

The bring-up script is **not** auto-recreating: if a cluster already
exists it bails with a clear message and asks you to run
`kind-down.sh` first. This is deliberate — silently destroying a
developer's running cluster would be surprising.

Justfile recipes mirror the scripts:

```sh
just kind-up
just kind-smoke
just kind-down
```

## Known limitation: Kata Containers

Kata Containers backends (`kata`, `kata-qemu`, `kata-fc`) are
deliberately absent from `runtimeclasses.yaml`. kind nodes are
themselves containers, and Kata requires nested KVM, which is not
available inside a containerised host. Trying to register a Kata
RuntimeClass here would only register a handler; Pods scheduled
against it would fail to start.

Exercise Kata on a real cluster (bare metal or a VM with nested
virtualisation enabled). Production cluster prep documentation will
cover this when the K8sExecutor moves past the kind-only stage.

## Staying current

The bring-up script pins a specific gVisor release (`GVISOR_VERSION`
in `kind-up.sh`) and verifies downloaded binaries against SHA-512
hashes hardcoded in the script. To bump:

1. Pick a release from <https://github.com/google/gvisor/releases>.
2. Update `GVISOR_VERSION` in `kind-up.sh`.
3. Refresh the four `RUNSC_SHA512_*` / `SHIM_SHA512_*` constants.
   The `# bump:` comment in `kind-up.sh` documents the exact curl
   incantation.

The kind node image is pinned by digest in `kind-config.yaml`; bump
it together with gVisor if compatibility shifts. Production deployers
should additionally pull binaries from a private mirror and verify
out-of-band signatures — neither is in scope for this dev sandbox.
