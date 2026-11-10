# AWS Nitro Sandboxes: assessment and adoption plan

Status: proposal, 2026-11-10. Nothing in this document is implemented.
Every Stirrup file and line reference was checked against `main` at
`eeaad430`. Every statement about AWS behaviour comes from AWS
documentation published on 2026-11-09 and is cited in
[Sources](#sources); none of it has been exercised against a live
cluster. Statements that the plan depends on and that only a live run
can settle are collected in
[Live validation](#live-validation-paid-needs-approval).

A first draft was reviewed adversarially against the code. The review
confirmed the verdict and changed the design in several places; where
that happened the text says what the earlier draft proposed and why it
was dropped, so the reasoning is not lost.

The reader is the session that amends or executes this plan. Re-verify
the line references before acting on them; the code will have moved.

## Contents

- [Verdict](#verdict)
- [What Nitro Sandboxes are](#what-nitro-sandboxes-are)
- [How this meets Stirrup's sandbox seams](#how-this-meets-stirrups-sandbox-seams)
- [Gaps](#gaps)
- [Design](#design)
- [Pull request sequence](#pull-request-sequence)
- [Live validation (paid, needs approval)](#live-validation-paid-needs-approval)
- [What is deliberately not built](#what-is-deliberately-not-built)
- [Open decisions](#open-decisions)
- [Sources](#sources)

## Verdict

Stirrup should support Nitro Sandboxes, on Amazon EKS, as a runtime of
the existing `k8s` executor. It should not add a new executor type.

The reasoning in one paragraph: on EKS a Nitro Sandbox is selected by
setting the Pod's `runtimeClassName` to `isolated-sandbox`, and the
sandboxed Pod still answers `pods/exec`, which is the only channel the
`k8s` executor uses for commands and file I/O. The executor seam
(`executor.Executor`, `podExecCore`) therefore needs no change. What
needs to change is narrower: the executor treats every RuntimeClass as
interchangeable, and Nitro Sandboxes differ from gVisor and Kata in
three ways that the executor must know about: how the guest is sized,
how "no network" is expressed, and how to confirm the runtime actually
in force. The work also exposes weaknesses that are not specific to
Nitro and are worth fixing for every runtime: how the executor waits
for a Pod, what `--dry-run` does on a cluster, and the fact that a
cluster which accepts a NetworkPolicy without enforcing it goes
undetected.

The most important caveat: on EKS Auto Mode's default settings,
NetworkPolicy is not enforced. `network.mode: none` can be made safe
regardless (G4). `network.mode: allowlist` cannot, until the cluster's
enforcement is switched on, and Stirrup cannot yet detect that it is
off (G5).

Summary of the recommendation by surface:

| AWS surface | Recommendation | Why |
|---|---|---|
| EKS, `isolated-sandbox` RuntimeClass, `k8s` executor | **Build.** Five pull requests, two of them not Nitro-specific. | Maps onto the existing seam; gives Ring 1 a hardware VM boundary on AWS, which Stirrup has no tested answer for today. |
| EKS, `isolated-sandbox`, `k8s-sandbox` executor (Agent Sandbox CRD, warm pools) | **Design later**, after cold-start time is measured. | The executor is hard-wired to GKE today; AWS uses a newer API version of the same CRD. Worth doing only if cold start is a measured problem. |
| EKS, `confidential-sandbox` RuntimeClass for the sandbox Pod | **Reject at validation** with an explanatory error. | It defends the workload from the node operator, which is not Stirrup's threat, and its default policy denies `exec`, the executor's only I/O channel. |
| ECS isolated tasks as an executor | **Do not build.** | Would need a new executor on ECS Exec (an interactive Systems Manager session protocol) for the same isolation the EKS path gives. |
| Self-managed Nitro Sandboxes on EC2 (Firecracker) | **Do not build.** | Stirrup would become a VM orchestrator: guest images, tap devices, an in-guest agent. |
| Harness process itself inside a Nitro Sandbox (EKS Job or ECS task) | **Document only**, low priority. | No code needed; it is a `runtimeClassName` on the harness Job. |

## What Nitro Sandboxes are

A Nitro Sandbox is a lightweight virtual machine that the Nitro
hypervisor runs as a first-level guest beside an EC2 instance's own
workload, using CPU and memory donated by that instance (the "parent
instance"). It is not nested virtualisation. It builds on Nitro
Enclaves and adds ordinary virtio network and block devices. Firecracker
is the supported virtual machine monitor. There is no additional charge
beyond the EC2 instance. [EC2]

There are three ways to consume it:

1. **EKS.** A Pod opts in with `runtimeClassName: isolated-sandbox` or
   `confidential-sandbox`. On EKS Auto Mode both RuntimeClasses exist on
   every cluster and a capable node is provisioned on demand; the
   RuntimeClass carries the node selector and toleration, so the Pod
   spec needs neither. Standard node groups need a custom AMI.
   Kubernetes 1.36 or later is required. [EKS-OVERVIEW] [EKS-ISOLATED]
2. **ECS.** A task definition sets
   `trustedExecutionConfiguration.isolationMode` to `ISOLATED_SANDBOX`
   or `CONFIDENTIAL_SANDBOX`. ECS Managed Instances only. [ECS]
3. **EC2 directly.** The operator runs Firecracker on an
   enclave-enabled instance and owns everything else. [EC2]

The two EKS modes differ in who is trusted:

| | `isolated-sandbox` | `confidential-sandbox` |
|---|---|---|
| Protects | The node and other Pods from the workload | Also the workload from the node operator |
| `exec`, stdin | Allowed | Denied by the default policy |
| Cedar policy annotation | Optional; a permissive default applies | Required; pins image digest, args, env, mounts |
| Memory overhead per Pod | 256 MiB | 512 MiB |

[EKS-OVERVIEW] [EKS-CONFIDENTIAL] [EKS-AUTOSCALING]

Facts that shape the design:

- **`exec`, `cp` and `logs` use an internal control channel (vsock),
  not the Pod network.** They keep working when the Pod has no network
  device. [EKS-NETISO]
- **"No network" is a Pod annotation.**
  `sandbox.amazonaws.com/network-isolated: "true"` boots the guest with
  no network device at all: no DNS, no instance metadata, no Pod
  Identity credentials. AWS documents one residual channel: a root
  process on the parent instance can connect to vsock ports that a
  guest process binds. [EKS-NETISO]
- **NetworkPolicy is enforced on the node, outside the guest**, by eBPF
  programs on the Pod's host-side interface. Nothing inside the guest
  can alter it. But **EKS Auto Mode accepts NetworkPolicy objects
  without enforcing them** until the `amazon-vpc-cni` ConfigMap in
  `kube-system` sets `enable-network-policy-controller: "true"`.
  [EKS-SECURITY] [SAMPLES-SKILL]
- **The guest is sized from the Pod's resource requests.** A Pod with no
  requests gets a small default guest; the AWS sample measured one vCPU
  and about 130 MiB. CPU and memory are fixed at boot. [SAMPLES-SKILL]
  [EKS-LIMITS]
- **`emptyDir` and the container writable layer live inside the guest**,
  on an overlay disk backed by the node's instance store. The kubelet
  reports their usage as zero. [EKS-STORAGE] [EKS-LIMITS]
- **Shared node resources are throttled per Pod by default**: 100 Mbps
  and 1,500 packets per second each way, 100 DNS queries per second,
  4,096 tracked connections, 50 MB/s overlay disk throughput. Each is
  adjustable by a `sandbox.amazonaws.com/*` Pod annotation, read once at
  Pod creation. [EKS-THROTTLE]
- **The Pod spec is a trusted input.** AWS states the feature is
  designed to contain untrusted images and code, not untrusted Pod
  specs. [EKS-SECURITY] This matches Stirrup: the harness authors the
  Pod spec and the model never does.
- **What was requested is not proof of what ran.** The Pod's
  `runtimeClassName` shows the request. Evidence from inside the guest
  includes `/sys/class/dmi/id/product_name` ending in `.enclave` and
  virtio block devices (`vda`, `vdb`) in `/sys/block`. [BLOG]
  [SAMPLES-SKILL]
- **Unsupported**: `hostUsers: false`, `hostNetwork`, GPU passthrough,
  in-place resize, snapshot and restore. [EKS-LIMITS]
- **Regions.** The announcement says all commercial regions; the EKS
  guide says US commercial regions. These conflict and the EKS guide is
  the more specific source. [WHATSNEW] [EKS-OVERVIEW]

AWS's own sample repository contains an example with Stirrup's shape, an
agent loop outside the sandbox that sends commands into it
("brain and hands"). It reaches the sandbox over the Pod network through
an in-Pod daemon rather than through `pods/exec`. [SAMPLES-BRAIN]
Stirrup's `pods/exec` path is the stronger arrangement for
`network.mode: none`, because it works with no network device at all.

## How this meets Stirrup's sandbox seams

Stirrup has six executor types (`types/runconfig.go:1051`). The two
that create sandboxes on a cluster are `k8s` (creates a Pod directly)
and `k8s-sandbox` (creates an Agent Sandbox custom resource whose
controller creates the Pod). Both embed `podExecCore`
(`harness/internal/executor/k8s_execcore.go:32`), which implements
every `Executor` method over the `pods/exec` subresource: commands as
`/bin/sh -c`, file I/O as `tar` streams.

What already fits a Nitro Sandbox Pod without change:

| Stirrup behaviour | Where | Fit |
|---|---|---|
| All I/O over `pods/exec` | `k8s_execcore.go` | Supported in isolated mode, including with no network device. |
| `runtimeClassName` from `executor.runtime` | `k8s.go:273` | The opt-in mechanism. |
| Workspace is an `emptyDir` | `k8s.go:246-252` | Lives inside the guest; the node cannot be reached through it. |
| No `nodeSelector` or toleration added by the harness; an operator's `k8sNodeSelector` is passed through | `k8s.go:272` | The RuntimeClass carries scheduling. An operator selector that conflicts with it leaves the Pod unschedulable. |
| `automountServiceAccountToken: false` | `k8s.go:271` | Matches the AWS sample; the guest holds no cluster credential. |
| Drop all capabilities, non-root UID, seccomp `RuntimeDefault`, `fsGroup` | `k8s.go:277-304` | All supported; none of the unsupported fields are set. |
| CPU and memory requests equal their limits when `executor.resources` sets them | `k8s.go:416-453` | The guest is sized from requests. |
| Per-Pod egress NetworkPolicy installed before the Pod | `k8s.go:166-176` | Enforced outside the guest, when the cluster enforces at all. |

The integration suite is already cluster-portable
(`harness/internal/executor/k8s_test.go`, build tag `integration_k8s`,
gated on `STIRRUP_TEST_KUBECONFIG`). It builds `K8sExecutorConfig`
directly, so it bypasses the validation gap below and can exercise
`isolated-sandbox` on unmodified `main`. It needs an adjusted
invocation and a prepared cluster to do so; see
[Live validation](#live-validation-paid-needs-approval).

## Gaps

Numbered for reference from the pull request sequence.

**G1. The runtime name is rejected.** `validK8sRuntimes`
(`types/runconfig.go:2173`) is a closed set of `runc`, `gvisor`,
`kata-qemu`, `kata-fc`, `kata-clh`; `ValidateRunConfig` rejects
anything else (`types/runconfig.go:6259`). This is the only hard
blocker.

**G2. Readiness waits 60 seconds and handles failure badly.**
`k8sReadyTimeout` (`k8s.go:36`) is a constant. On EKS Auto Mode a first
sandbox Pod may wait for a node to be provisioned; AWS's own
walkthrough waits 300 seconds. [BLOG] The cause is node autoscaling,
not the runtime: gVisor on an autoscaled pool has the same cold start.
`waitForPodReady` (`k8s.go:473-489`) also has two weaknesses that a
longer wait makes worse. It returns on the first API error, so one
transient error ends the run (the `k8s-sandbox` poller already retries
these, `agentsandbox.go:238-246`). And it treats a `Failed` phase or an
image-pull error as "not yet", so a misconfigured run consumes the
whole timeout.

**G3. An unset `executor.resources` can produce an undersized guest.**
`resourcesToPodResources` returns an empty block when resources are nil
(`k8s.go:416-420`), which leaves sizing to the namespace. Where the
namespace has a LimitRange default, that default sizes the guest. Where
it has none, a Nitro Sandbox boots the default guest of roughly one
vCPU and 130 MiB, too small for a build or test run, and the failure
surfaces as out-of-memory kills inside the agent's commands rather than
as a configuration error.

**G4. `network.mode: none` can silently fail open.** The executor
installs a deny-all egress NetworkPolicy (`k8s_netpol.go:67`). On EKS
Auto Mode with default settings the object is accepted and not
enforced, so the sandbox would have full egress while the RunConfig
says `none`. This is the same class of failure `docs/executors/k8s.md`
already documents for kindnet, but on a production platform. Nitro
Sandboxes offer a structural alternative (the `network-isolated`
annotation) that the executor cannot use, because it sets no Pod
annotations (`k8s.go:216-224`).

**G5. `network.mode: allowlist` is unconfined on EKS Auto Mode
defaults, and no annotation fixes it.** In this mode the NetworkPolicy
is the only thing that forces traffic through the proxy
(`k8s_netpol.go:92-137`). With enforcement off, the sandbox can ignore
the proxy and can also reach the in-guest instance metadata and Pod
Identity endpoints. Two further points weaken the "policy is installed
before the Pod" guarantee stated at `k8s.go:166-169` on this platform.
The reviewer's research found that the AWS network policy agent's
default ("standard") mode starts a new Pod with allow-all until its
policies are reconciled, and that upstream issues report longer
unenforced windows (`aws/aws-network-policy-agent` issues 573, 676,
691). Neither was verified here; both are questions for live
validation.

**G6. Nothing confirms the runtime in force.** The executor trusts that
a Pod created with a RuntimeClass ran under it. `docs/executors/k8s.md`
has a manual "Verify gVisor is in force" step. A misconfigured handler,
or an admission webhook that rewrites `runtimeClassName`, would go
unnoticed. The executor also discards the admitted Pod that `Create`
returns (`k8s.go:178`), which already shows what admission did to the
spec.

**G7. Default throttles may slow real agent work.** 1,500 packets per
second is roughly 18 Mbps at a 1,500-byte MTU. Dependency installs and
large clones in `allowlist` mode may be visibly slow. Raising the
limits needs Pod annotations. Whether this matters is unmeasured.

**G8. `confidential-sandbox` does not fit an external harness.** The
mode protects the workload from the node operator. Stirrup's threat is
the reverse: protect everything else from the workload, which isolated
mode covers. Mechanically, the default confidential policy denies
`Exec` and stdin [EKS-CONFIDENTIAL], and `podExecCore` has no other
channel. A custom policy can allow `exec`, but only by pinning command
hashes, which cannot describe an agent that runs arbitrary commands.
The policy also pins every environment variable, and the sandbox
identity token is a per-run environment value (`factory.go:155-162`,
`k8s.go:237-244`), so a pre-generated policy cannot match a run that
uses one.

**G9. The `k8s-sandbox` executor is GKE-specific.** It forces
`runtimeClassName: gvisor`, a `sandbox.gke.io/runtime` node selector and
toleration (`agentsandbox.go:63-110`), and pins the CRD at
`agents.x-k8s.io/v1alpha1` (`agentsandbox_api.go:12`). AWS's samples use
the upstream controller at `v1beta1`. [SAMPLES-IDE] The warm-pool
feature that AWS recommends for fast starts ("about two seconds"
[BLOG]) is also the feature Stirrup's executor refuses today
(`agentsandbox.go:263-270`), because its per-Pod egress policy would
not bind to an adopted Pod.

**G10. `executor.resources.diskMb` may not be enforced.** It maps to an
`ephemeral-storage` limit (`k8s.go:435`), which the kubelet enforces by
measuring usage. Usage inside a Nitro Sandbox reports as zero.
[EKS-LIMITS] AWS's stated workaround is to set the limit anyway, which
suggests the runtime sizes the overlay from it, but the documentation
does not say so.

**G11. `--dry-run` creates a real sandbox for `k8s`.** The preflight
special-cases only the `container` executor
(`harness/internal/core/preflight.go:337-370`); for `k8s` it builds the
real executor under a 30-second budget (`preflight.go:94`). This is
already true today. On a Nitro Sandbox it boots a VM, may provision a
node, and will usually time out.

**G12. The sandbox identity token volume may reach node disk.** The
token directory is an `emptyDir` with `medium: Memory`
(`k8s.go:253-262`), and `docs/executors/k8s.md:680-682` promises the
token never touches node disk. AWS says `emptyDir` volumes live on the
guest's overlay disk, which is backed by the node's instance store,
and does not say whether memory-medium volumes are an exception.
[EKS-STORAGE]

**G13. Pod-create errors blame the RuntimeClass too readily.**
`classifyPodCreateError` (`k8s.go:382-390`) attributes any `Invalid` or
`Forbidden` response to the RuntimeClass whenever one was requested. A
LimitRange maximum or a node selector that conflicts with the
RuntimeClass's scheduling would produce a misleading message. More
runtimes and default sizing make this more likely to be hit.

**G14. Teardown briefly removes the policy from a guest that may still
be running.** `Close` deletes the Pod with zero grace and then deletes
the NetworkPolicy immediately (`k8s.go:318-331`). Zero grace removes
the API object before the workload has stopped. This exists today; a
VM that is slower to stop widens the window in `allowlist` mode.

## Design

### Runtime profiles

The core change is to stop treating RuntimeClass names as opaque
strings inside the executor. Introduce a small registry in
`harness/internal/executor`, one entry per accepted `k8s` runtime
value, that declares what the executor must do differently for that
runtime. This follows the provider quirks registry
(`harness/internal/provider/quirks/`) and the observability platform
profiles (`harness/internal/observability/platform/`): behaviour that
varies by vendor lives in one declared table rather than in
conditionals at the call sites.

Proposed shape (names are suggestions):

```go
// k8sRuntimeProfile declares how the k8s executor adapts a sandbox Pod
// to one runtime.
type k8sRuntimeProfile struct {
    // runtimeClassName is the Pod RuntimeClassName this runtime maps
    // to. Empty leaves the field unset.
    runtimeClassName string
    // guestSizedFromRequests marks runtimes that boot a VM sized from
    // the Pod's resource requests, so empty requests are a defect.
    guestSizedFromRequests bool
    // noNetworkAnnotations are added to the Pod when network.mode is
    // "none", for runtimes that can remove the network device.
    noNetworkAnnotations map[string]string
    // verify confirms from inside the sandbox that this runtime is in
    // force. Nil means the runtime has no verifier.
    verify func(ctx context.Context, core *podExecCore) error
}
```

Entries on day one:

| `executor.runtime` | RuntimeClass | Sized from requests | No-network annotation | Verifier |
|---|---|---|---|---|
| `""` | unset | no | none | none |
| `runc`, `gvisor`, `kata-*` | same as the value | no | none | none (follow-up) |
| Nitro isolated (value is [open decision 1](#open-decisions)) | `isolated-sandbox` | yes | `sandbox.amazonaws.com/network-isolated: "true"` | DMI product name ends `.enclave` |

Decisions inside this design and why:

- **The accepted-value set stays closed and stays in `types/`.**
  Validation cannot import `harness/internal`. A unit test in the
  executor package asserts that the set and the registry agree in both
  directions, following `TestWorkspaceArchiver_ImplementorsMatchTypesTable`
  (`harness/internal/executor/workspacearchive_test.go:25`), which pins
  the same kind of pair. `types` needs an exported accessor for the
  `k8s` set; the existing helper drops the empty key
  (`types/runconfig_completions.go:104-114`), so the test checks `""`
  separately.
- **An unknown value fails closed in the executor.** Only callers that
  bypass `ValidateRunConfig` can reach this (the integration suite
  does). Constructing with a runtime that has no profile is an error,
  not a fallback to shared-kernel assumptions.
- **The closed set is kept rather than opened to any RuntimeClass
  name.** The point of a profile is that the executor knows the
  runtime's semantics.
- **The profile carries the RuntimeClass name.** Today the value and
  the RuntimeClass name are the same string. Giving the profile its own
  `runtimeClassName` costs nothing and is what makes open decision 1 a
  free choice.
- **`confidential-sandbox` is named in validation only to reject it**
  with an error that states G8 and points at the documentation. An
  operator will try it; the generic "unsupported executor.runtime"
  would not say the limit is deliberate.
- **Readiness time is not a profile property.** See
  [Readiness](#readiness). An earlier draft keyed it on the runtime;
  review showed the need comes from node autoscaling.
- **No new RunConfig field in PR 1 to PR 3.** Sizing checks, the
  no-network annotation and verification are all derived from the
  runtime value. This avoids the proto-mirror work and avoids fields
  that let a RunConfig weaken a posture the runtime value implies.

### Checking what admission did

`Create` returns the Pod as admitted. Before waiting for readiness the
executor compares it with what it asked for, for every runtime:

1. **RuntimeClass.** If the admitted `runtimeClassName` differs from
   the requested one, delete the Pod and policy and fail. This catches
   a rewriting webhook before anything boots. It also catches the
   reverse case, a namespace policy that injects a sandbox RuntimeClass
   into a Pod whose RunConfig left the runtime empty and which would
   otherwise run with the wrong profile.
2. **Sizing, for runtimes sized from requests.** If the admitted agent
   container has no CPU or memory request, the guest would boot at the
   platform default. See open decision 2 for what happens then. The
   recommended behaviour is to fill only the missing request (and its
   matching limit) with a harness default of 1 CPU and 1024 MiB. Because
   Kubernetes fills only fields that are absent, a default set before
   `Create` would silently override a namespace LimitRange, which
   contradicts `docs/executors/k8s.md:211-212`. So the executor first
   issues the same `Create` as a server-side dry run, reads back what
   admission would produce, and adds defaults only where requests are
   still empty. A dry-run create uses the `create` verb the Role already
   grants.

`classifyPodCreateError` is narrowed in the same change so that it
blames the RuntimeClass only when the API server's cause mentions it
(G13).

### Readiness

One change for all runtimes, replacing the constant:

- A single ceiling of five minutes, still capped by the caller's
  context.
- Fail immediately on terminal states: Pod phase `Failed`, or a
  container waiting with `ErrImagePull`, `ImagePullBackOff`,
  `InvalidImageName` or `CreateContainerConfigError`.
- Retry transient API errors until the deadline; fail at once on
  `Forbidden` or `Unauthorized`, as the `k8s-sandbox` poller does.
- On timeout, report the Pod's phase and its `PodScheduled` condition
  message. That comes from Pod status and needs no new permission. The
  autoscaler's own reason for not provisioning a node is published as
  Events, which the reference Role cannot read
  (`examples/k8s/rbac.yaml:54-74`); the documentation says where to
  look rather than widening the Role.

Two costs of a longer wait must be documented. The wait counts against
the run's deadline in `stirrup job` (`factory.go:263-266`). And a
sandbox identity token is exchanged before the executor is built
(`factory.go:129-162`), so a slow start consumes token lifetime;
`docs/executors/k8s.md:713` recommends tokens of 15 minutes or less.

### Network mode `none` on a Nitro Sandbox

When the profile declares `noNetworkAnnotations` and
`network.mode == "none"`, the executor adds the annotation to the Pod
**and still installs the deny-all NetworkPolicy**. The annotation is
the control that holds regardless of cluster configuration; the policy
remains as a second layer and keeps the object model identical across
runtimes (`Close` deletes it as it does today).

Annotations are set in `buildSandboxPod`, not in the shared
`buildSandboxPodSpec`. The `k8s-sandbox` executor builds its own Pod
template metadata (`agentsandbox.go:119-159`) and does not inherit
them, which is intended while that executor remains gVisor-only.

This closes G4 for Nitro Sandboxes without depending on the Auto Mode
enforcement switch. It also removes instance metadata and Pod Identity
from the guest, which is the correct posture for a sandbox whose
RunConfig says it has no network. Rule of Two accounting is unchanged:
`run_command` already counts as external communication regardless of
network mode (`types/runconfig.go:3507-3521`).

The residual vsock channel (a root process on the node can connect to
a port the guest binds) must be stated in the documentation. It needs a
compromised node, which is outside what isolated mode claims to defend.

### Verifying the runtime after the Pod is Ready

After readiness and before the executor is returned to the factory,
run the profile's verifier over the same `exec` path the run will use.
It runs before lifecycle hooks. For Nitro isolated:

1. `/sys/class/dmi/id/product_name` ends with `.enclave`.
2. When `network.mode == "none"`: `/sys/class/net` contains only `lo`.
   This confirms the annotation took effect.

A failed check fails executor construction and deletes the Pod and
policy through the existing cleanup path. The error states the value
observed. This is fail-closed with no opt-out field: the operator named
a runtime, and running under a different one is the event the runtime
choice exists to prevent.

Three things this needs:

- **A new security event.** `SecurityEventEmitter`
  (`harness/internal/executor/local.go:39-43`) has three methods and
  none fits. Add one, implement it in
  `harness/internal/security/securityevent.go` and in the test recorder
  (`k8s_unit_test.go`), and list it in `docs/security.md`.
- **Only tools the image contract already requires** (`/bin/sh`,
  `tar`, `ls`, `mkdir`, `readlink`; `k8s.go:63-66`). `cat` is not in the
  contract, so read the file with the shell's `read` builtin. Bound the
  check with `k8sFileIOTimeout`.
- **Evidence that the DMI string is stable.** EKS Auto Mode chooses the
  instance type per Pod. If the string differs on some family
  (Graviton, for example), runs would fail depending on where they
  land. Live validation must cover every family the node pool can
  provision. Until it has, the EKS recipe pins architecture with the
  existing `--k8s-node-selector kubernetes.io/arch=amd64`.

A control plane cannot currently tell a verification failure from a
configuration error, because build errors pass through as text
(`harness/cmd/stirrup/cmd/job.go:310-316`). Whether it should is open
decision 5.

### Detecting unenforced egress policy

This is the remedy for G5 and for the kindnet caveat, and it applies to
every `k8s` runtime. It is the least settled part of the design.

The idea: after the Pod is Ready, attempt from inside the sandbox a
connection the policy must refuse. If it succeeds, the policy is not
being enforced, and the run fails.

Review found three problems with the first draft, which was a single
direct TCP connect to the API server Service:

- **A single attempt misfires.** If the platform starts Pods
  allow-all and reconciles shortly after (G5), one early success would
  fail a run that is about to be confined.
- **A refused connection proves little on its own.** A Service address
  not yet programmed also refuses.
- **The image contract has no TCP client.** busybox `sh` has no
  `/dev/tcp`. Requiring `nc` or bash would break existing sandbox
  images.

Revised specification:

- **Wait until enforced.** Retry over a bounded settle window. Pass on
  the first refused or timed-out attempt; fail only if every attempt
  connects. This also guarantees the policy is in force before the
  model's first command, which the current ordering does not guarantee
  on a platform that reconciles late.
- **Add a positive control in `allowlist` mode.** The proxy port must
  connect. Without it, "everything refused" could mean the network is
  simply down.
- **Choose the forbidden destination deliberately.** Options: the API
  server Service (`KUBERNETES_SERVICE_HOST`, injected into every
  container), or a second listener on `stirrup egress-proxy` that no
  policy ever allows. The second is routable in every cluster the
  proxy runs in and does not depend on how a network plugin treats the
  API server, but exists only in `allowlist` deployments.
- **Decide how the probe makes a TCP connection** (open decision 4).

Because this changes behaviour for existing gVisor and Kata users, and
would fail every run on the kind dev cluster, it is its own pull
request. Validate it on the GKE Dataplane V2 cluster Stirrup already
tests against and on an enforcing CNI in kind before merging.

Until it lands, the documentation must say plainly that `allowlist`
mode on EKS is confined only when NetworkPolicy enforcement is switched
on, and the EKS recipe must include the step that switches it on.

### Pod annotations (deferred until measured)

If live validation shows the default throttles impair real work (G7),
the first answer is that an operator can already add
`sandbox.amazonaws.com/*` throttle annotations with a cluster-side
mutating admission policy, with no Stirrup change. Document that.

Only if that proves inadequate, add `executor.k8sPodAnnotations`. An
earlier draft proposed passing arbitrary annotations through with a
short list of rejected keys. Review pointed out that this contradicts
the rule above about not adding fields that weaken posture: Kata and
gVisor read Pod annotations that reconfigure the sandbox, and the full
set of `sandbox.amazonaws.com/*` keys is not known. So if the field is
built, each profile declares the annotation keys it permits (for Nitro
isolated, the throttle keys in [EKS-THROTTLE]), and validation rejects
everything else. It ships with its proto field and its copy in
`harness/internal/transport/grpc_translate.go`, per the RunConfig
mirror invariant.

### `k8s-sandbox` and warm pools (separate design)

Making `k8s-sandbox` work on EKS needs three things, in increasing
size:

1. Move the GKE admission deltas (`applyAgentSandboxAdmissionDeltas`)
   behind the runtime profile so they apply only when the runtime is
   `gvisor`, and stop forcing `gvisor` when another runtime is named.
2. Discover the served Agent Sandbox API version instead of pinning
   `v1alpha1`, and build the object in the matching shape.
3. Warm pools: claim a pre-booted Pod through a `SandboxClaim` against
   a `SandboxTemplate`. This breaks two current assumptions. The egress
   policy must bind to the adopted Pod (by the controller's
   `agents.x-k8s.io/sandbox-name-hash` label, as
   `docs/executors/k8s-agent-sandbox.md` already notes). And per-run
   environment (proxy variables, sandbox identity, git proxy
   configuration) cannot be set on a Pod that already exists, so it
   must be delivered after claim, over `exec`, the way
   `WriteSandboxIdentityToken` already delivers tokens. A
   network-isolated pool is a distinct template from a networked one,
   because the annotation is read once at Pod creation.

Items 1 and 2 are mechanical. Item 3 is a design of its own and should
be written only once the cold-start measurement from live validation
shows it is needed. If a Nitro Sandbox on an already-provisioned node
is Ready in a few seconds, warm pools buy little for a run that lasts
minutes.

## Pull request sequence

Each pull request is independently mergeable and reviewer-gated in the
usual way. Line references will have drifted; re-verify.

### Step 0: live validation (no code; needs approval to spend)

See [Live validation](#live-validation-paid-needs-approval). The
findings are written back into this document. PR 1 and PR 4 do not
depend on it. PR 2 can be drafted before the spike, but must not merge
documentation claims the spike has not confirmed, and open decision 1
must be settled before PR 2 is written.

### PR 1: readiness and admission checks for the `k8s` executor

Closes G2, G13 and the RuntimeClass half of G6. No Nitro content; it
benefits every runtime and is reviewable without AWS knowledge.

- `harness/internal/executor/k8s.go`: the readiness changes in
  [Readiness](#readiness); the admitted-RuntimeClass comparison;
  narrowed `classifyPodCreateError`.
- Unit tests for each terminal state, transient-error retry, the
  timeout message, and an admitted RuntimeClass that differs.
- `docs/executors/k8s.md`: the troubleshooting row for the 60-second
  timeout (line 745); the run-deadline and token-lifetime notes.
- The integration suite's per-test contexts are 90 seconds
  (`k8s_test.go:86` and others) and must rise with the ceiling, or the
  suite cannot run on an autoscaled cluster.

### PR 2: Nitro isolated runtime for the `k8s` executor

Closes G1, G3, G4, G8. Depends on PR 1 and on open decisions 1 and 2.

- `types/runconfig.go`: add the value to `validK8sRuntimes`; reject
  `confidential-sandbox` (and, if the value is not the RuntimeClass
  name, `isolated-sandbox`) for `k8s` with a message naming the right
  value; keep `k8s-sandbox` gVisor-only. Update the comments at
  1073-1076 and 2167-2172.
- `types/runconfig_completions.go`: export the `k8s` set; decide
  whether `--container-runtime` shell completion offers it
  (`completion_test.go:104` pins the current list).
- `harness/internal/executor/k8s_runtimeprofile.go` (new): the profile
  type and registry, and the coverage test.
- `harness/internal/executor/k8s.go`: resolve the profile; map the
  value to the RuntimeClass name; the sizing check in
  [Checking what admission did](#checking-what-admission-did);
  no-network annotations in `buildSandboxPod`.
- `harness/cmd/stirrup/cmd/runconfigflags.go:86`: the flag help repeats
  the set.
- `proto/harness/v1/harness.proto:2244-2252`: the `runtime` field
  comment lists the set; update it and run `just proto`. No field
  changes, so `grpc_translate.go` and the eval spec need nothing (both
  pass the string through).
- Tests: validation accept and reject cases, extending
  `TestValidateRunConfig_RuntimeSetIsPerType`
  (`types/runconfig_test.go`); `types/runconfig_completions_test.go:56`
  pins each accessor to its map; Pod shape per profile (annotation only
  for Nitro isolated with mode `none`; defaults only where requests are
  empty; partial resources, CPU only or memory only; fractional CPU);
  registry coverage; an example-RunConfig test following
  `TestExampleRunConfig_LoadsAndValidates_*` (`harness_test.go:1291`).
- Docs and examples. Every place that lists the set:
  `docs/executors/k8s.md` lines 166, 173-175, 193-195, the resource
  defaults at 211-212, the test variable table at 768-769;
  `docs/safety-rings.md` lines 810-811 and the Ring 1 table at 1268;
  `docs/configuration.md:522`; `README.md:99`;
  `examples/k8s/runtimeclass.yaml:3-7`;
  `examples/k8s/sample-sandbox-pod.yaml:50`; `examples/k8s/README.md:18`.
  New: an "EKS with Nitro Sandboxes" recipe in `docs/executors/k8s.md`
  (including the enforcement switch and the statement that `allowlist`
  mode is unconfined without it, the architecture pin, and the
  requirement that the egress proxy runs in the sandbox namespace on an
  ordinary runtime); the egress enforcement caveat extended from
  kindnet to EKS Auto Mode; `examples/runconfig/k8s-eks-nitro.json` and
  its entry in `examples/runconfig/README.md`.
- Integration: make the suite exercise the annotation path, and add a
  `/proc/mounts` assertion to `TestK8sSandboxIdentityToken_RoundTrip`
  for G12.

### PR 3: verify the runtime in force

Closes the rest of G6 for Nitro isolated. Ship in the same release as
PR 2 so there is no released version that accepts the runtime without
verifying it.

- Profile `verify` hook, called in `NewK8sExecutor` after readiness;
  the new security event and its plumbing.
- Unit tests with the `execFn` hook (`k8s_execcore.go:47`); integration
  test asserting the verifier passes on a real Nitro Sandbox and fails
  when the same check runs against a `runc` Pod.
- Docs: replace the manual verification step for this runtime; state
  what the check does and does not prove; `docs/security.md`.
- Follow-up issue: a gVisor verifier.

### PR 4: read-only `--dry-run` for `k8s`

Closes G11. Route the `k8s` and `k8s-sandbox` preflight through checks
that create nothing: build the client, confirm the namespace is
reachable and that the caller may create Pods and NetworkPolicies
there. A RuntimeClass lookup is cluster-scoped and the reference Role
cannot do it; either skip it or report it as "not checked". Independent
of the Nitro work and can land at any point.

### PR 5: fail when egress policy is not enforced

Closes G5 and the kindnet caveat, for every `k8s` runtime. Specified in
[Detecting unenforced egress policy](#detecting-unenforced-egress-policy).
Depends on open decisions 3 and 4. Measure G14 here and, if the window
is real, make `Close` wait for the Pod to be gone before deleting the
policy, as the `k8s-sandbox` executor already does
(`agentsandbox.go:294-334`).

### Conditional and later, each behind its own issue

- `executor.k8sPodAnnotations` with per-profile permitted keys, only if
  G7 is real and a cluster-side policy is not enough.
- `k8s-sandbox` portability (profile-keyed admission deltas, API
  version discovery).
- Warm pools, with a design document first.
- A recipe for running the harness Job itself under the Nitro isolated
  RuntimeClass, with Bedrock credentials from EKS Pod Identity.
  `harness/internal/provider/bedrock.go:101` uses the AWS SDK default
  credential chain, which should pick up Pod Identity without code.
  Untested, and `examples/k8s/harness-job.yaml` sets no resources, so
  the harness would itself boot in the small default guest.

## Live validation (paid, needs approval)

Nothing here has been run. It creates billable AWS resources and needs
the maintainer's approval, an AWS account, and credentials.

Minimum environment, for roughly half a day:

- One EKS Auto Mode cluster, Kubernetes 1.36 or later, in a US
  commercial region.
- The standing objects from `examples/k8s/` (namespace, RBAC), and the
  egress proxy from `examples/k8s/egress-proxy/` deployed in the test
  namespace.
- A sandbox image that satisfies the image contract and also has a TCP
  client for the egress tests.
- A long-lived placeholder Pod with `runtimeClassName:
  isolated-sandbox`, so a capable node exists before the suite starts.

The charges are the EKS cluster hourly fee, the Auto Mode management
fee and one or two mid-sized EC2 instances. This should come to a few
US dollars; confirm against current pricing before starting, and delete
the cluster afterwards.

The integration suite cannot simply be pointed at the cluster. It
builds `K8sExecutorConfig` directly and so bypasses the validation gap
(G1), but on unmodified `main`:

- readiness is 60 seconds and each test's context 90 seconds, so a cold
  node fails the first test (hence the placeholder Pod);
- `TestK8sRuntimeClass_Admitted` iterates `""`, `runc`, `gvisor` unless
  `STIRRUP_TEST_RUNTIME_CLASSES` is set (`k8s_test.go:454-460`);
- the egress tests skip unless `STIRRUP_TEST_ENFORCE_EGRESS` is set
  (`k8s_test.go:679`, `:729`), so with it unset nothing observes the
  fail-open case;
- the orphan test sets a 20-second Pod lifetime (`k8s_test.go:829`),
  which includes VM boot and image pull and may flake;
- the garbage-collection test's stand-in owner Pod has no
  `securityContext` (`k8s_test.go:957-968`) and is rejected by a
  namespace enforcing the restricted Pod Security Standard;
- about 27 tests each boot a VM, which can exceed `go test`'s default
  ten-minute limit; a timeout panics without running cleanup and
  leaves Pods and a node behind.

Invocation, unmodified code:

```sh
cd harness
STIRRUP_TEST_KUBECONFIG=... \
STIRRUP_TEST_NAMESPACE=... \
STIRRUP_TEST_RUNTIME_CLASS=isolated-sandbox \
STIRRUP_TEST_RUNTIME_CLASSES=isolated-sandbox \
STIRRUP_TEST_IMAGE=... \
STIRRUP_TEST_EGRESS_PROXY_URL=... \
STIRRUP_TEST_ENFORCE_EGRESS=1 \
go test -tags integration_k8s -count=1 -v -timeout 60m \
  ./internal/executor/ -run 'TestK8s'
```

Check the variable names against the comment block at the top of
`k8s_test.go` before running. Run it twice: first with NetworkPolicy
enforcement off (the Auto Mode default), where the egress tests are
expected to fail and that failure is the finding; then with enforcement
on.

Three questions need hand-applied manifests, because `main` cannot
express them: cold-start time (a Pod plus `kubectl wait
--timeout=10m`, with and without a node present); the
`network-isolated` annotation (`examples/k8s/sample-sandbox-pod.yaml`
plus the annotation, then a `tar` round trip over `kubectl exec`); and
guest sizing.

Questions the spike must answer, each of which a section above depends
on:

| # | Question | Depends on it |
|---|---|---|
| V1 | Does the suite pass with the adjusted invocation (exec, 10 MB output, `tar` file I/O, process-group kill, timeouts)? | The verdict |
| V2 | Does `exec` use WebSocket or the SPDY fallback (`k8s_execcore.go:432`) through the sandbox runtime? | Robustness |
| V3 | Time to Ready with a node present, and with none. | Readiness ceiling; whether warm pools matter |
| V4 | Guest size with no requests, with requests set, and with fractional CPU. | G3 defaults |
| V5 | With enforcement off, does a mode-`none` Pod reach the internet? With the annotation, does it not, and does `exec` still work? | G4, the design's central claim |
| V6 | With enforcement off and on, can an `allowlist` Pod reach `169.254.169.254`, the Pod Identity endpoint, and an arbitrary internet host directly? How long after Pod start does enforcement begin? | G5, PR 5 |
| V7 | Does `/sys/class/dmi/id/product_name` end in `.enclave` on every instance family the node pool can provision, including Graviton? | PR 3 verifier |
| V8 | Is `ephemeral-storage` enforced inside the guest, and what does exceeding it look like? | G10 |
| V9 | Wall-clock time for a representative `git clone` and dependency install through the egress proxy under default throttles. | G7 |
| V10 | Do `activeDeadlineSeconds`, the owner reference and zero-grace deletion behave as on other runtimes? After deletion, how long does the guest keep running? | Lifecycle parity, G14 |
| V11 | Does UID 65532 with `fsGroup` get a writable `/workspace` inside the guest? | PR 2 |
| V12 | What is the mount type of the sandbox identity token directory inside the guest? | G12 |
| V13 | Which regions actually offer the RuntimeClasses? | Documentation |

Per the project's rule on negative claims, any documentation sentence
of the form "the sandbox cannot reach X" must be backed by an executed
check, not by AWS's description of the mechanism.

## What is deliberately not built

**An ECS executor.** ECS isolated tasks give the same boundary as the
EKS path. Reaching into a task means ECS Exec, which is an interactive
Systems Manager session: it requires a writable root filesystem and the
SSM agent in the task, and its session protocol is not designed for
exit codes, separate output streams or binary file transfer. It would
be a second remote-exec stack for no additional isolation. An operator
committed to ECS can run the whole harness as an `ISOLATED_SANDBOX`
task with the `local` or `none` executor; that needs a recipe, not an
executor.

**A self-managed EC2 or Firecracker executor.** The EC2 guide lists
what the orchestrator must own: guest images, device backends, memory
accounting and zeroing, attestation relay. [EC2] That is a product in
itself. Revisit only if a deployment cannot use EKS or ECS.

**AWS Lambda MicroVMs.** The EC2 guide names this as the alternative
for isolated workloads without managing servers or networking. [EC2] It
is a separate product with its own API and was not assessed here. It
would be a new executor type, so it deserves its own assessment if a
serverless AWS sandbox is wanted.

**`confidential-sandbox` for the sandbox Pod.** See G8.

**Attestation-gated secrets.** Confidential mode would let a harness
running inside a sandbox prove its identity to AWS KMS before
receiving a provider key, so that not even the node operator could
read it. That would be a new `security.SecretStore` backend. It is the
one capability here with no existing equivalent in Stirrup. It is
recorded as a possible direction, not planned: no current deployment
distrusts its own node operator.

**Generating Cedar policies for the guest.** The in-guest policy can
restrict `exec` only by command hash. Stirrup's own Cedar ring already
gates tool calls before they reach the executor.

**`ApplicationNetworkPolicy` instead of the egress proxy.** EKS Auto
Mode's hostname-based policy could replace the proxy for `allowlist`
mode. It allows the addresses a name resolves to, so other names on the
same addresses are reachable; it is one platform's API; and the proxy
also produces the security events Stirrup records. The proxy stays the
mechanism.

## Open decisions

For the maintainer. Each has a recommendation. Decision 1 blocks PR 2;
the others block only the pull request named.

1. **What is the `executor.runtime` value?** This is the one choice
   here that cannot be undone cheaply, because operators' RunConfigs
   will contain it.
   - `isolated-sandbox`: the RuntimeClass name AWS ships. Matches AWS's
     documentation and keeps the value equal to the RuntimeClass name,
     as it is for every existing value. But the name identifies no
     vendor, and the profile attaches AWS-specific behaviour to it (an
     `amazonaws.com` annotation, an `.enclave` check). Another platform
     shipping a RuntimeClass of the same name would get that behaviour
     and a verifier that always fails.
   - `nitro-isolated` (or similar), mapped by the profile to
     RuntimeClass `isolated-sandbox`. Names the technology, as `gvisor`
     and `kata-fc` do. Validation rejects `isolated-sandbox` with a
     message naming the right value, so an operator following AWS's
     documentation is redirected at once.

   Recommended: `nitro-isolated`. The existing values are technology
   names that happen to coincide with conventional RuntimeClass names;
   this keeps that meaning and leaves room for a later
   `executor.k8sRuntimeClassName` override for clusters that rename
   their classes.
2. **Sizing when requests are empty on a guest-sized runtime (PR 2).**
   Fill a default after a dry-run create, as designed above; or fail
   with an error asking for `executor.resources` or a namespace
   LimitRange. Recommended: fill the default and log it. Failing is
   simpler to implement and stricter, but makes the minimal EKS
   RunConfig fail, and the `k8s-sandbox` executor already fills
   defaults.
3. **PR 5 and the kind dev cluster.** Install an enforcing CNI in
   `scripts/dev/kind-up.sh`; add an explicit development-only opt-out;
   or apply the probe only to runtimes that claim isolation.
   Recommended: an enforcing CNI in the dev cluster and no opt-out, so
   the dev loop tests what production runs.
4. **How the PR 5 probe opens a TCP connection.** Add a TCP client to
   the sandbox image contract (a breaking change for existing images,
   to be release-noted); or have the harness run the probe from a
   short-lived second container; or skip the probe with a warning when
   the image has no client. Recommended: decide after the spike shows
   how large the unenforced window is. If it is zero on an enforcing
   cluster, a skipped probe with a loud warning is tolerable; if not,
   the contract change is justified.
5. **Should a control plane be able to distinguish an isolation
   verification failure from a configuration error?** Today both
   arrive as build-error text. Recommended: not in PR 3; the security
   event carries the distinction for operators, and a typed build error
   can follow if a control plane asks for it.
6. **Approve the paid spike?** Recommended: yes, before PR 2 merges.
   PR 1 and PR 4 need no AWS validation and can proceed now.

## Sources

AWS documentation, read 2026-11-10:

- [WHATSNEW] <https://aws.amazon.com/about-aws/whats-new/2026/11/nitro-sandboxes-eks-ecs-ec2/>
- [EC2] <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nitro-sandboxes.html>
- [EKS-OVERVIEW] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html>
- [EKS-ISOLATED] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-isolated-setup.html>
- [EKS-CONFIDENTIAL] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-confidential-setup.html>
- [EKS-NETISO] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-network-isolation.html>
- [EKS-SECURITY] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-security-model.html>
- [EKS-LIMITS] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-limitations.html>
- [EKS-THROTTLE] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-resource-isolation.html>
- [EKS-STORAGE] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-storage.html>
- [EKS-AUTOSCALING] <https://docs.aws.amazon.com/eks/latest/userguide/autoscaling.html>
- Cedar schema for the in-guest policy: <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-cedar-schema.html>
- [ECS] <https://docs.aws.amazon.com/AmazonECS/latest/developerguide/ict-overview.html> and <https://docs.aws.amazon.com/AmazonECS/latest/developerguide/ict-getting-started.html>
- [BLOG] <https://aws.amazon.com/blogs/containers/aws-nitro-sandboxes-hardware-enforced-workload-isolation-on-amazon-eks/>

AWS sample repository, `github.com/aws-samples/sample-eks-nitro-sandbox`
at its default branch on 2026-11-10. This is sample material, not
reference documentation; where it is the only source for a fact, the
fact is listed for live validation:

- [SAMPLES-SKILL] `.agents/skills/nitro-sandbox-pods/SKILL.md`
- [SAMPLES-BRAIN] `examples/agent-sandbox/brain-and-hands/deploy/warm-pool.yaml`
- [SAMPLES-IDE] `examples/agent-sandbox/ide/deploy/sandbox.yaml`
