# Kubernetes executor

The `k8s` executor runs the agent inside a hardened, single-use
**sandbox Pod** rather than a local Docker/Podman container. It is the
executor for running stirrup *on* a Kubernetes cluster: an orchestrator
process (the harness binary holding a kubeconfig or in-cluster
ServiceAccount) creates one Pod per run, drives command execution and
file I/O over the `pods/exec` subresource, confines the Pod's egress
with a per-Pod `NetworkPolicy`, and deletes the Pod when the run ends.

This document is the operator reference. The reference manifests it
points at live under [`examples/k8s/`](../../examples/k8s/) and the
local development cluster under [`scripts/dev/`](../../scripts/dev/).
The executor implementation is
[`harness/internal/executor/k8s.go`](../../harness/internal/executor/k8s.go)
and [`k8s_netpol.go`](../../harness/internal/executor/k8s_netpol.go);
every flag, field, and label documented here is cross-checked against
those files.

## Contents

- [When to use it](#when-to-use-it)
- [Architecture](#architecture)
- [Configuration reference](#configuration-reference)
- [Deployment recipes](#deployment-recipes)
- [RuntimeClass selection per run](#runtimeclass-selection-per-run)
- [Egress](#egress)
- [Safety rings on Kubernetes](#safety-rings-on-kubernetes)
- [Troubleshooting](#troubleshooting)

## When to use it

| Executor | Boundary | Use when |
|---|---|---|
| `local` | none (host process) | Trusted local iteration. |
| `container` | a Docker/Podman container on the harness host | A single host with a container engine is the deployment target. |
| `k8s` | a Pod on a Kubernetes cluster | The deployment target is a cluster; multi-tenant isolation, per-tenant RuntimeClass, and cluster-native NetworkPolicy egress are required. |

The `k8s` executor is the cluster-native analogue of the `container`
executor. The two share the `image`, `network`, `resources`, and
`runtime` configuration surface; the differences are that the sandbox
is a Pod (not a host container), the runtime maps to a Pod
`RuntimeClassName` (not a host OCI runtime), and egress is enforced by
a `NetworkPolicy` plus an in-cluster proxy Deployment (not an in-process
host proxy).

## Architecture

```mermaid
flowchart LR
  subgraph Orchestrator["Orchestrator (harness binary)"]
    SA[authenticates as<br/>stirrup-orchestrator SA]
  end
  subgraph NS["Sandbox namespace (PodSecurity: restricted)"]
    NP{{Per-Pod egress<br/>NetworkPolicy}}
    Pod[/"Sandbox Pod (agent)<br/>hardened securityContext<br/>RuntimeClass = runtime"/]
    Proxy[Egress proxy<br/>Deployment]
  end
  SA -->|pods: create/get/delete| Pod
  SA -->|pods/exec: create| Pod
  SA -->|networkpolicies: create/delete| NP
  NP -.->|binds| Pod
  Pod -->|HTTP_PROXY allowlist mode| Proxy
  Proxy -->|FQDN allowlist| Net([Internet])
```

The pieces and their lifecycle:

- **The orchestrator** is whatever runs the harness (a CI runner, a
  controller, an operator shell). It authenticates with the cluster in
  this order (see
  [`buildRESTConfig`](../../harness/internal/executor/k8s.go)): an
  explicit kubeconfig at `executor.k8sKubeconfig` if set (it wins even
  when running in-cluster), then the in-cluster ServiceAccount, then
  `$KUBECONFIG`. It holds the RBAC the executor's lifecycle needs:
  `pods` (create/get/delete), `pods/exec` (create), and
  `networkpolicies` (create/delete). The reference Role
  ([`examples/k8s/rbac.yaml`](../../examples/k8s/rbac.yaml)) also grants
  `pods/log` (get); the executor does not use it, so it is granted only
  for operator `kubectl logs` debugging and may be dropped to tighten
  the Role to the executor's minimum.

- **The sandbox Pod** is created per run with a fixed, config-independent
  hardened `securityContext`: `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`, `runAsNonRoot: true`, `runAsUser: 65532`
  (the distroless "nonroot" UID), and `seccompProfile.type:
  RuntimeDefault`. `automountServiceAccountToken` is **always** `false`,
  so the sandbox itself has no Kubernetes API access regardless of the
  ServiceAccount named. `restartPolicy` is `Never` (one-shot), the
  container is named `agent`, and the entrypoint is `/bin/sh -c "sleep
  infinity"` — all real work runs through subsequent `exec` calls, not
  the entrypoint. The working directory `/workspace` is an `emptyDir` the
  executor mounts, with pod `securityContext.fsGroup: 65532`, so the
  workspace is writable by the non-root UID for *any* image that ships a
  shell — the image need not pre-create a writable `/workspace`, and the
  volume is wiped per Pod. (This mirrors the container executor's writable
  host bind mount; both hide any content an image bakes at that path.) The
  exact spec is annotated field-by-field in
  [`examples/k8s/sample-sandbox-pod.yaml`](../../examples/k8s/sample-sandbox-pod.yaml).

- **Command execution and file I/O** both ride the `pods/exec`
  subresource. `Exec` runs `/bin/sh -c`; `ReadFile`/`WriteFile` stream a
  `tar` archive over exec; `ListDirectory` runs `ls -A1`. The image must
  therefore ship a POSIX shell at `/bin/sh` plus `tar` and `ls` on
  `PATH` — a shell-less distroless static image will not work. Output
  and file payloads are capped at 10 MB (matching the container
  executor).

- **Egress** is enforced by a per-Pod `NetworkPolicy` the executor
  installs *before* the Pod is created (closing the window in which a
  Running Pod would otherwise have cluster-default egress). Mode `none`
  installs a deny-all egress policy; mode `allowlist` installs a policy
  permitting egress only to DNS and the in-cluster egress proxy, and
  injects `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` into the container. See
  [Egress](#egress).

The label contract knits these together: every sandbox Pod carries
`stirrup-sandbox: "true"` and `stirrup.dev/pod: <pod-name>`, and the
allowlist policy selects the proxy by `app=stirrup-egress-proxy` on TCP
8080. These labels are fixed in `k8s_netpol.go`; the manifests must keep
them in sync.

## Configuration reference

The `k8s` executor is selected by `--executor k8s` /
`executor.type: "k8s"`. It draws on the shared executor fields (`image`,
`network`, `resources`, `runtime`) plus a set of `K8s*` fields. Every
flag and field below is verified against
[`runconfigflags.go`](../../harness/cmd/stirrup/cmd/runconfigflags.go),
[`harness.go`](../../harness/cmd/stirrup/cmd/harness.go), and
[`types/runconfig.go`](../../types/runconfig.go).

### Flags and fields

| CLI flag | RunConfig field | Required | Notes |
|---|---|---|---|
| `--executor k8s` | `executor.type: "k8s"` | yes | Selects the executor. |
| `--image` | `executor.image` | yes for `k8s` | Pod container image. Must ship `/bin/sh`, `tar`, and `ls`. |
| `--k8s-namespace` | `executor.k8sNamespace` | yes for `k8s` | Namespace the sandbox Pod (and its `NetworkPolicy`) is created in. |
| `--k8s-kubeconfig` | `executor.k8sKubeconfig` | no | Path to a kubeconfig file. Empty prefers in-cluster config, then `$KUBECONFIG`. |
| `--k8s-node-selector key=value` | `executor.k8sNodeSelector` | no | Repeatable `nodeSelector` constraint, e.g. `--k8s-node-selector disktype=ssd`. Merged with any `nodeSelector` the RuntimeClass contributes. |
| `--k8s-service-account` | `executor.k8sServiceAccount` | no | ServiceAccount name for the Pod. Empty uses the namespace `default`. The token is never automounted regardless. |
| `--k8s-egress-proxy-url` | `executor.k8sEgressProxyUrl` | conditional | Egress proxy URL the Pod's `HTTP_PROXY`/`HTTPS_PROXY` point at. Required when `network.mode` is `allowlist`; rejected otherwise. |
| `--container-runtime` | `executor.runtime` | no | Pod `RuntimeClassName`. Closed set: `runc`, `gvisor`, `kata-qemu`, `kata-fc`, `kata-clh`. Empty selects the cluster-default RuntimeClass (and logs an isolation warning). |
| *(RunConfig only)* | `executor.network` | yes for `k8s` | Mode `none` or `allowlist`. A nil network is rejected at config load (fail closed). |
| *(RunConfig only)* | `executor.resources` | no | CPU/memory/disk mapped onto the Pod container. See [Resource mapping](#resource-mapping). |

`--container-runtime` is shared with the `container` executor, where it
names a host OCI runtime. For `k8s` the same field names a Pod
`RuntimeClassName`, which is **not** the same namespace of values: the
closed set for `k8s` is `runc / gvisor / kata-qemu / kata-fc / kata-clh`
(`gvisor`, not the OCI runtime name `runsc`). Shell completions list the
container set only, so `gvisor` is valid but not offered by completion.

### Validation rules

`ValidateRunConfig` enforces the following for `executor.type: "k8s"`
(see `validateK8sExecutor` / `validateK8sEgressProxy` /
`validateExecutorRuntime` in
[`types/runconfig.go`](../../types/runconfig.go)):

- `executor.image` is **required**.
- `executor.k8sNamespace` is **required**.
- `executor.workspace` is **rejected** — the Pod workspace is fixed at
  `/workspace`, not a mapped host directory.
- `executor.network` is **required** (set `mode` to `none` or
  `allowlist`). A nil network leaves egress posture undefined and is
  surfaced at config-load time rather than at runtime.
- `executor.k8sEgressProxyUrl` is **required** when `network.mode` is
  `allowlist` and **rejected** when it is not.
- `executor.runtime`, when non-empty, must be one of the closed set
  `runc / gvisor / kata-qemu / kata-fc / kata-clh`; any other value is
  rejected.
- `executor.resources`, when set, must not carry negative values — a
  negative bound would silently map to "no limit".

### Resource mapping

`executor.resources` maps onto the Pod container as follows
(`resourcesToPodResources`):

| Field | Pod mapping |
|---|---|
| `cpus` | `requests.cpu` **and** `limits.cpu`. Whole cores render as an integer (`"2"`), fractional cores as milli-CPU (`"500m"`). |
| `memoryMb` | `requests.memory` **and** `limits.memory` (Mi). |
| `diskMb` | `limits.ephemeral-storage` **only** (Mi) — the eviction ceiling. No request, so the scheduler is not forced to find a node with that much free scratch. |
| `pids` | **Logged and ignored.** Per-Pod PID limits are a kubelet setting (`--pod-max-pids`), not a container resource field. |

A nil or all-zero `resources` block leaves the Pod's requirements empty
so it inherits namespace defaults.

## Deployment recipes

Each recipe below is a `kind`/cluster bring-up plus the run invocation.
The standing objects (namespace, RBAC, RuntimeClasses) come from
[`examples/k8s/`](../../examples/k8s/); apply them once per cluster:

```sh
kubectl apply -f examples/k8s/namespace.yaml
kubectl apply -f examples/k8s/rbac.yaml
kubectl apply -f examples/k8s/runtimeclass.yaml   # registered handlers only
```

Register only the RuntimeClasses whose handlers are actually installed
on the nodes — a RuntimeClass whose handler is missing makes Pod
scheduling fail. The recipes set `--mode execution` and a network mode;
fill in the provider/model and prompt as for any run.

### kind + runc (manifest-shape smoke test)

A stock `kind` cluster runs every Pod under `runc` and does **not**
enforce `NetworkPolicy` (its default CNI, kindnet, accepts policy objects
but ignores them). This recipe proves the executor can create, exec
into, and tear down a Pod — it does **not** prove egress confinement.

```sh
./scripts/dev/kind-up.sh        # or: just kind-up
kubectl config use-context kind-stirrup-sandbox

stirrup harness \
  --executor k8s \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox \
  --mode execution \
  --prompt "..."
# network.mode comes from a RunConfig; use "none" for the smoke test.
```

### kind + gVisor (sandboxed runtime, unenforced egress)

`scripts/dev/kind-up.sh` installs gVisor (`runsc` + the containerd shim)
into the kind node and registers a `gvisor` RuntimeClass, so a Pod can
schedule onto a kernel-isolated runtime. Egress is still unenforced on
kindnet — combine gVisor with a real CNI for the full posture.

```sh
./scripts/dev/kind-up.sh        # installs gVisor + RuntimeClasses
./scripts/dev/smoke-test.sh     # or: just kind-smoke — verifies gVisor runs

stirrup harness \
  --executor k8s \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox \
  --container-runtime gvisor \
  --mode execution \
  --prompt "..."
```

Kata backends are deliberately absent from the kind dev cluster: kind
nodes are themselves containers and Kata needs nested KVM, which is not
available inside a containerised host. Exercise Kata on a real cluster.

### GKE Sandbox (gVisor on GKE)

GKE Sandbox runs Pods under gVisor and exposes a `gvisor` RuntimeClass
on Sandbox-enabled node pools. No node-level gVisor install is needed —
GKE manages it. A NetworkPolicy-enforcing CNI is available on GKE
(Dataplane V2 / Calico), so the egress policy is genuinely enforced.

> **GKE manages the `gvisor` RuntimeClass — do not apply this repo's.**
> GKE Sandbox creates and reconciles a `gvisor` RuntimeClass whose
> `handler` is **`gvisor`** (not `runsc`) and whose `scheduling` already
> carries the Sandbox node pool's `nodeSelector`
> (`sandbox.gke.io/runtime: gvisor`) **and** the matching toleration for
> the pool's `sandbox.gke.io/runtime=gvisor:NoSchedule` taint. Setting
> `--container-runtime gvisor` targets that managed class directly, so a
> Pod schedules onto the Sandbox pool with the toleration injected for
> it. Do **not** `kubectl apply` the `gvisor` entry from
> [`examples/k8s/runtimeclass.yaml`](../../examples/k8s/runtimeclass.yaml)
> on GKE — its `handler: runsc` and `stirrup.dev/runtime-gvisor`
> nodeSelector describe a self-managed gVisor install and conflict with
> the GKE-managed object. The sandbox **image must be amd64-compatible**
> (or multi-arch): GKE Sandbox node pools are x86.

```sh
# Create a node pool with GKE Sandbox enabled, then:
stirrup harness \
  --executor k8s \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox \
  --container-runtime gvisor \
  --mode execution \
  --prompt "..."
```

The managed `gvisor` RuntimeClass already pins scheduling to the Sandbox
pool and tolerates its taint, so `--k8s-node-selector
sandbox.gke.io/runtime=gvisor` is not required (it is an additional,
redundant constraint). Add `--k8s-node-selector` only to further
constrain placement. To verify gVisor is actually in force inside a Pod,
`uname -r` reports a synthetic version (observed `4.4.0`) and `dmesg`
shows a `Starting gVisor...` banner — both distinct from the host kernel.

The control plane of a **private-endpoint** GKE cluster is unreachable
from outside the VPC; the orchestrator can run in-cluster (in-cluster
ServiceAccount) or, for an out-of-cluster orchestrator, reach the API
server through [GKE Connect
Gateway](https://docs.cloud.google.com/kubernetes-engine/enterprise/multicluster-management/gateway).
The executor negotiates WebSocket-first for the `pods/exec` stream (with
a SPDY fallback), so exec and file I/O work through such a proxied API
endpoint, not only against a directly-reachable API server.

### Kata Containers (kata-qemu / kata-fc / kata-clh)

The three Kata backends give hardware-virtualization isolation: each Pod
runs in a lightweight VM. They are **not** interchangeable on one node —
`kata-fc` needs a devmapper snapshotter, `kata-clh` a different VMM — so
[`examples/k8s/runtimeclass.yaml`](../../examples/k8s/runtimeclass.yaml)
gives each a **distinct** `nodeSelector` label. Label each node only for
the handler(s) it actually has:

```sh
kubectl label node <node> stirrup.dev/runtime-kata-qemu=true
kubectl label node <node> stirrup.dev/runtime-kata-fc=true
kubectl label node <node> stirrup.dev/runtime-kata-clh=true
```

Then select the matching runtime per run:

```sh
# kata-qemu — broadest compatibility, needs KVM (bare metal or nested virt)
stirrup harness --executor k8s --container-runtime kata-qemu \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox --mode execution --prompt "..."

# kata-fc — Firecracker, fast boot, needs a devmapper snapshotter + KVM
stirrup harness --executor k8s --container-runtime kata-fc \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox --mode execution --prompt "..."

# kata-clh — Cloud Hypervisor, needs KVM
stirrup harness --executor k8s --container-runtime kata-clh \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox --mode execution --prompt "..."
```

Kata prerequisites are documented inline in
[`examples/k8s/runtimeclass.yaml`](../../examples/k8s/runtimeclass.yaml).
The RuntimeClass's `scheduling.nodeSelector` and any
`--k8s-node-selector` the run supplies are merged by Kubernetes into the
effective node selection.

## RuntimeClass selection per run

`executor.runtime` maps directly to the Pod `RuntimeClassName`, so a
multi-tenant orchestrator selects the isolation level per run by setting
the field. A cluster can register the full set and route each tenant to
the appropriate class:

- Low-trust / untrusted prompts → `gvisor` or a `kata-*` backend.
- Trusted internal workloads → `runc` (or leave empty for the cluster
  default, accepting the isolation warning the executor logs).

An empty `executor.runtime` leaves `RuntimeClassName` unset, which
selects the cluster-default RuntimeClass — often plain `runc` with no
sandbox isolation. The executor logs a warning in that case so the
opt-out is visible; a run wanting guaranteed isolation must set the
field explicitly.

Node isolation for sandbox runtimes is a `taint`/`toleration` pattern,
documented (but **not yet implemented** by the executor — it does not
inject tolerations) in
[`examples/k8s/taint-and-toleration.yaml`](../../examples/k8s/taint-and-toleration.yaml).

## Egress

The `k8s` executor's network posture comes from `executor.network.mode`,
and the matching `NetworkPolicy` is installed before the Pod exists.

### Mode `none` — deny all egress

A deny-all egress `NetworkPolicy` (an `Egress` policy type with no egress
rules) selects the Pod and permits no outbound traffic. No proxy and no
proxy env vars are involved.

### Mode `allowlist` — proxy + DNS

The policy permits egress only to (a) DNS (UDP/TCP 53) and (b) the
in-cluster egress proxy (`app=stirrup-egress-proxy` on TCP 8080).
Everything else is forced through the proxy, where the FQDN allowlist
applies. The executor injects:

```
HTTP_PROXY  = <k8sEgressProxyUrl>
HTTPS_PROXY = <k8sEgressProxyUrl>
NO_PROXY    = localhost,127.0.0.1,::1
```

The `NO_PROXY` set (`localhost,127.0.0.1,::1`) is fixed in the executor
and not flag-configurable.

The `NetworkPolicy` intentionally does **not** encode the FQDN allowlist
— NetworkPolicy operates on IPs/ports/selectors, not hostnames. The
hostname allowlist lives in the proxy. The policy's job is the
complementary half: guarantee the Pod cannot reach the network except
via the proxy.

### The egress proxy

The proxy is the same allowlist proxy the `container` executor runs
in-process, deployed as a long-lived in-cluster Deployment many sandbox
Pods share (a Pod cannot start its own host-side proxy). Deploy it from
[`examples/k8s/egress-proxy/`](../../examples/k8s/egress-proxy/), or run
it standalone with the `stirrup egress-proxy` subcommand:

```sh
stirrup egress-proxy --listen :8080 \
  --allowlist api.anthropic.com --allowlist '*.github.com:443'
# or read the allowlist from a file:
stirrup egress-proxy --listen :8080 --allowlist-file ./allowlist.txt
```

The proxy reads its allowlist once at startup (no hot reload); roll the
Deployment to change it. During a rolling update, in-flight runs keep
routing through the old Pod (and its old allowlist) until the new Pod is
Ready; pause runs across the roll if that overlap window is
unacceptable.

**The proxy MUST run in the same namespace as the sandbox Pod.** The
allowlist policy selects the proxy by a `PodSelector` with **no**
`NamespaceSelector`, so a cross-namespace proxy is not matched: under an
enforcing CNI the sandbox then gets *no* egress at all (a silent deny,
not a bypass). Set `--k8s-namespace` and the proxy's namespace to the
same value, and point `--k8s-egress-proxy-url` at the
`<service>.<namespace>.svc` name — for example
`http://stirrup-egress-proxy.stirrup-sandbox.svc:8080`. The
top-level `examples/k8s/` manifests use `stirrup-sandbox` while
`egress-proxy/` ships `default`; align them before applying (see the
[examples README](../../examples/k8s/README.md#namespace-alignment--required-before-applying-the-egress-proxy)).

### Enforcement caveat — kindnet does not enforce NetworkPolicy

`NetworkPolicy` is enforced only by a CNI that implements it. **kindnet,
the default CNI for `kind`, accepts NetworkPolicy objects but does not
enforce them.** On a stock kind cluster the deny-all and allowlist
policies are inert, and a sandbox Pod retains cluster-default egress.
The confinement holds only on a NetworkPolicy-enforcing CNI such as
[Cilium](https://cilium.io/) or
[Calico](https://www.tigera.io/project-calico/).

Treat a kind-based test as proof of *manifest shape* — the objects are
created with the right selectors, ports, and env — not as proof that
egress is actually confined. This mirrors the `container` executor's
honest fail-open note around `host.docker.internal`.

## Safety rings on Kubernetes

The five [safety rings](../safety-rings.md) map onto the `k8s` executor
as follows. The full per-ring detail is in
[`docs/safety-rings.md`](../safety-rings.md#safety-rings-on-kubernetes);
the K8s-specific mapping is:

| Ring | On Kubernetes |
|---|---|
| **1 — Runtime class** | `executor.runtime` is the Pod `RuntimeClassName`, not a host OCI runtime. `gvisor` / `kata-*` give kernel/VM isolation; empty selects the cluster default (warned). The hardened `securityContext` (drop ALL, runAsNonRoot, seccomp RuntimeDefault) is applied unconditionally beneath the runtime choice. |
| **2 — Egress proxy** | A per-Pod `NetworkPolicy` plus the in-cluster proxy Deployment, instead of an in-process host proxy. Enforcement depends on a NetworkPolicy-enforcing CNI. |
| **3 — Cedar policy** | Unchanged — per tool-call authorization runs in the orchestrator before the executor acts, identically across executor types. |
| **4 — Rule of Two** | Unchanged — a pre-flight invariant on the `RunConfig`. A non-`none` `network.mode` counts toward `canCommunicateExternally` exactly as for other executors. |
| **5 — Code scanner** | Unchanged — post-edit content check, executor-agnostic. |

Defence-in-depth on K8s additionally relies on cluster-level controls the
reference manifests configure: the sandbox namespace enforces the
`restricted` Pod Security Standard (pinned to a version, not `latest`),
and the orchestrator's RBAC is the minimal `pods` / `pods/exec` /
`networkpolicies` verb set the executor needs (the reference Role adds
`pods/log` get for operator debugging only; drop it to reach the
executor's exact minimum). The sandbox Pod has no API access of its own
(`automountServiceAccountToken: false`).

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `RuntimeClass "<name>" was rejected by the cluster` | The named RuntimeClass is not registered or not permitted. | `kubectl get runtimeclass`; apply [`runtimeclass.yaml`](../../examples/k8s/runtimeclass.yaml) for the handlers actually installed, or pick a registered runtime. |
| `executor.k8sNamespace is required for executor.type="k8s"` | `--k8s-namespace` / `executor.k8sNamespace` unset. | Set the namespace. |
| `executor.k8sEgressProxyUrl is required when ... "allowlist"` | `network.mode: allowlist` without a proxy URL. | Set `--k8s-egress-proxy-url`, or use `network.mode: none`. |
| `executor.k8sEgressProxyUrl is only valid when ... "allowlist"` | A proxy URL set while `network.mode` is `none`. | Remove the URL, or switch to `allowlist`. |
| `executor.network is required for executor.type="k8s"` | No `network` block. | Set `executor.network.mode` to `none` or `allowlist`. |
| `not in cluster and KUBECONFIG is unset` | No in-cluster config, no kubeconfig. | Set `--k8s-kubeconfig` or `$KUBECONFIG`, or run in-cluster. |
| Pod scheduling fails / pending forever | RuntimeClass `nodeSelector` matches no node, or the handler is missing. | Label the node for the runtime; confirm the handler is installed. |
| Egress not actually confined on `kind` | kindnet does not enforce NetworkPolicy. | Use a NetworkPolicy-enforcing CNI (Cilium, Calico) for real confinement. |
| `pod ... not ready` after the readiness timeout (default 60 s, or the caller context deadline if shorter) | Image lacks `/bin/sh`, or the runtime cannot start the Pod. | Use an image shipping `/bin/sh`, `tar`, `ls`; check `kubectl describe pod`. |
| Exec/file I/O fails with API errors | The orchestrator lacks `pods/exec` create. | Apply [`rbac.yaml`](../../examples/k8s/rbac.yaml) (or grant the verb). |

## See also

- [`examples/k8s/`](../../examples/k8s/) — reference manifests
  (namespace, RBAC, RuntimeClasses, sample Pod, egress proxy).
- [`scripts/dev/`](../../scripts/dev/) — local `kind` cluster with
  gVisor for development (`just kind-up` / `kind-smoke` / `kind-down`).
- [`docs/safety-rings.md`](../safety-rings.md) — the five-ring model.
- [`docs/configuration.md`](../configuration.md) — full CLI/RunConfig
  reference.
