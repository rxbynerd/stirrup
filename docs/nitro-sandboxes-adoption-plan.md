# AWS Nitro Sandboxes: assessment and adoption plan

Status: implemented and open for review as three stacked pull requests, 2026-11-13; not released. On
2026-11-13 the implementation ran on two EKS Auto Mode clusters and passed its live acceptance; see
[Live acceptance (2026-11-13)](#live-acceptance-2026-11-13). See
[Implementation status](#implementation-status-2026-11-13) for what was built, where it departs from this
plan, and the live checks still open.
Measured results are summarised in [Live spike findings](#live-spike-findings-incorporated-on-2026-11-11)
and [Tested recipe](#tested-recipe-2026-11-11). The raw spike evidence was removed
from the tree once distilled; it remains at commit `bccf4862` under `docs/research/nitro-spike-2026-11-11/`
(PR #881), with key learnings in the Alexandria `equestrianism` space.
Every Stirrup file and line reference was checked against `main` at
`eeaad430`. AWS behaviour is drawn from current AWS documentation and sample
material, with review corrections checked on 2026-11-11 and cited in
[Sources](#sources); a bounded subset was exercised by the 2026-11-11 spike on EKS 1.36 Auto Mode, IPv4/amd64,
`m5d.xlarge` in us-east-1. The spike evidence is authoritative for those
measurements; documentation claims are not promoted to live results. Remaining
questions and the original validation procedure are collected in
[Live validation](#live-validation-paid-needs-approval).

A first draft was reviewed adversarially against the code. The review
confirmed the verdict and changed the design in several places; where
that happened the text says what the earlier draft proposed and why it
was dropped, so the reasoning is not lost.

The reader is the session that amends or executes this plan. Re-verify
the line references before acting on them; the code will have moved.

## Contents

- [Implementation status](#implementation-status-2026-11-13)
  - [Live acceptance (2026-11-13)](#live-acceptance-2026-11-13)
    - [Clusters and builds](#clusters-and-builds)
    - [Results against the open items](#results-against-the-open-items)
    - [Defects found](#defects-found)
    - [Accepting `diskMb`](#accepting-diskmb)
    - [Guest disk](#guest-disk)
    - [Guest CPU and memory](#guest-cpu-and-memory)
    - [Instance types](#instance-types)
    - [Node pools](#node-pools)
    - [Annotations](#annotations)
    - [Admission mutation](#admission-mutation)
    - [Whole runs](#whole-runs)
  - [Where the implementation departs from this plan](#where-the-implementation-departs-from-this-plan)
  - [Live items still open](#live-items-still-open)
- [Implementation handoff gates](#implementation-handoff-gates)
- [Live spike findings](#live-spike-findings-incorporated-on-2026-11-11)
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

## Implementation status (2026-11-13)

The plan was implemented as three stacked pull requests, verified with unit
tests, deliberate breakage of the checks and a local kind cluster, and then on
2026-11-13 on two EKS Auto Mode clusters
([Live acceptance](#live-acceptance-2026-11-13)). That run closed the nine live
items open before it; what remains is under
[Live items still open](#live-items-still-open). The Nitro runtime is **not
released**.

| Plan item | Pull request | State |
|---|---|---|
| PR 1: readiness, admission check, teardown, token re-exchange | #887 | Implemented; open for review |
| PR 4: read-only `--dry-run` for `k8s` and `k8s-sandbox` | #888 | Implemented; open for review. Ordered before the Nitro runtime, as [Step 0](#step-0-live-validation-no-code-needs-approval-to-spend) requires |
| PR 2 + PR 3: `nitro-isolated` runtime and runtime diagnostics | #894 | Implemented as one change; open as a draft; live acceptance passed on 2026-11-13 |
| PR 5: networked Nitro sandboxes | none | Not started; still needs its own design (#896) |

Follow-up issues: live EKS acceptance (#895), the networked design (#896),
runtime diagnostics for gVisor (#897), control-plane visibility of the
diagnostics and typed build failures (#898), missing CLI flags (#899), and
the work this plan defers until a need is shown (#900).

### Live acceptance (2026-11-13)

Two temporary EKS Auto Mode clusters ran the `k8s` executor with
`executor.runtime: nitro-isolated`. The raw logs and evidence stayed outside
the tree; this subsection is the record.

#### Clusters and builds

| | Cluster 1 | Cluster 2 |
|---|---|---|
| Region | us-east-1 | eu-west-2 |
| Kubernetes | 1.36 (`v1.36.5-eks-91f89a9`), platform `eks.17` | 1.37 (`v1.37.1-eks-91f89a9`), platform `eks.9` |
| Node software | Bottlerocket (EKS Auto, Standard) 2026.11.11, kubelet `v1.36.4-eks-cfb47f5`, kernel 6.18.56, containerd 2.2.9+bottlerocket, AMI `ami-029e8be0f5b2dba0b`; guest kernel 6.18.56 | not recorded |
| IP family | IPv4 | IPv4 |
| NodeClass | `networkPolicy: DefaultAllow`, public IPs, 80Gi ephemeral storage | same |

Both clusters had the `isolated-sandbox` and `confidential-sandbox`
RuntimeClasses two minutes after creation. `isolated-sandbox` carried overhead
`memory: 256Mi` plus one `eks.amazonaws.com/nitro-sandbox` and node selector
`eks.amazonaws.com/nitro-sandbox: "true"` on both; `confidential-sandbox`
carries 512Mi. The sandbox image was
`public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4`
(amd64 only; the arm64 runs used the tag `busybox:1.37.0-glibc`).

Three builds of the branch were used: `251ba0f4` (the pull request as first
opened), `782f51ba` (adds the wait for exec to reach a Ready Pod) and
`bc177e8d` (also accepts `resources.diskMb`).

#### Results against the open items

| # | Item | Result |
|---|---|---|
| 1 | Guest workspace size with no `ephemeral-storage` declared | 10,367,980 KiB (9.89 GiB) on all 13 instance types tried; see [Guest disk](#guest-disk). |
| 2 | Offline acceptance cases against the implemented executor | `TestK8sNitroIsolated_EndToEnd` passed 10 times, every subtest: runtime diagnostics, Pod shape, exec, non-zero exit, timeout, process-tree kill, output cap, file round trip, UID write, no guest network, capacity, teardown. It failed twice before the cold-start fix, and on arm64 by design. |
| 3 | Whole-Pod admission check against a real `Create` response | Passes. The `Create` response already carries both platform marks (`spec.overhead["eks.amazonaws.com/nitro-sandbox"]` and the node selector). |
| 4 | Field classification | No false failure. At creation EKS added only API-server defaults, the node-selector entry, the two default tolerations, `priority: 0`, `preemptionPolicy` and `overhead`. `imagePullSecrets`, `priorityClassName`, `schedulingGates`, `readinessGates` and `affinity` were absent at creation and at readiness. The cluster's three mutating webhooks (`pod-identity-webhook`, `vpc-resource-mutating-webhook`, `eks-load-balancing-webhook`) changed nothing on these Pods. |
| 5 | Runtime diagnostics in their implemented form | Pass; the security event read `{"dmiProductName":"m5d.enclave","interfaces":"lo,dummy0","routeEntries":"0"}`. |
| 6 | Graceful-delete timing | Executor `Close` to "Pod gone" took 3.2 to 5.3 s over 9 runs; `kubectl delete --grace-period=1` 3.5 to 4.9 s over about 40 Pods. |
| 7 | DMI product name on other instance families | `<family>.enclave` on 11 amd64 families. arm64 has no DMI and is rejected; see [Instance types](#instance-types). |
| 8 | Throttle annotation list | All eleven documented keys accepted together by the platform. A real mutating admission policy that retuned `log-bandwidth` to `200M` was allowed by the admission check and the run passed. |
| 9 | Timeout, process-tree kill and output cap at `log-bandwidth: 20M` | Pass every time. Process-tree kill 13.2 to 15.7 s (the test's own waits), output cap 3.5 to 4.3 s. No `could not kill timed-out command` warning in any run. |

The general suite on ordinary Pods (`go test -tags integration_k8s -run
TestK8s`, excluding the Nitro group) ran on cluster 1 on `251ba0f4` and
`bc177e8d`: 45 passed, 0 failed, 3 skipped (two egress-enforcement tests need
`STIRRUP_TEST_ENFORCE_EGRESS`; one helper). It includes
`TestK8sRuntimeDiagnostics_NitroIsolatedFailsOnNonNitroPod`.

#### Defects found

1. **The first exec failed on a node that had just joined** (fixed in
   `782f51ba`). A node is Ready within 3 s of registering, but its kubelet
   serving certificate request was approved 15 s after it was created (26 s
   in one of seven samples). Until then the API server cannot dial the
   kubelet and exec fails with `unable to upgrade connection: error dialing
   backend: remote error: tls: internal error`. A sandbox Pod is Ready about
   7 s after scheduling, inside that window. Measured with `kubectl exec`
   from the moment `kubectl wait` returned, exec was refused for up to 7.9 s
   on 7 of 12 new nodes.
   - Before the fix: 2 of 2 Nitro cold starts failed in the runtime
     diagnostics, and 4 of 4 ordinary-Pod cold starts failed
     `TestK8sExec_Success`.
   - After the fix: 8 of 8 Nitro cold starts on amd64 passed (`m5d.xlarge`
     four times, `m5.xlarge`, `m7a.xlarge`, `m7i.xlarge`, `c8a.large`) and 4
     of 4 ordinary-Pod cold starts passed. Each logged `k8s executor waiting
     for exec to reach the sandbox pod` where the window was hit.
2. **A dead sandbox ends the run with a provider error** (not fixed on this
   branch; separate branch `fix/run-command-empty-error-result`). When a
   command exhausts memory the container is OOM-killed and the Pod goes
   `Failed`. The next `run_command` returns an error tool result with empty
   content, and the Anthropic API rejects the request (`tool_result: content
   cannot be empty if is_error is true`). Reproduced 3 of 3. An ordinary Pod
   is killed the same way, so this is not specific to Nitro.

#### Accepting `diskMb`

`executor.resources.diskMb` was rejected under `nitro-isolated` because the
2026-11-11 spike saw a 128 MiB write succeed against a 64 MiB limit (V8). The
declaration is in fact the size of the guest disk, with a floor (see
[Guest disk](#guest-disk)), and the size is a hard limit; the earlier write
fitted under the floor. The handoff gate for a disk quota claim is "Measured
enforcement; otherwise reject explicit Nitro `diskMb`"
([Implementation handoff gates](#implementation-handoff-gates)), and
enforcement is now measured, so `bc177e8d` accepts `diskMb` and submits it as
an equal `ephemeral-storage` request and limit.
`TestK8sNitroIsolated_DiskMbBoundsTheGuestDisk` passed three times.

#### Guest disk

One XFS filesystem (`/dev/vdb2`, `noquota`) backs both the `emptyDir`
workspace and the container's writable layer.

| Declared `ephemeral-storage` | `df -k` total (KiB) |
|---|---|
| none | 10,367,980 |
| 64Mi (2026-11-11), 300Mi | 394,220 |
| 600Mi | 496,620 |
| 1Gi | 930,796 |
| 2Gi (request only, and limit only) | 1,979,372 |
| 4Gi | 4,076,524 |
| 8Gi | 8,270,828 |
| 20Gi | 20,853,740 |
| 100Gi | 104,739,820 |
| 200Gi on `m5d.xlarge` | Pod did not become Ready in 90 s |

- About `max(declared, 500 MiB) - 115 MiB`; 10 GiB when undeclared.
- `emptyDir.sizeLimit: 2Gi` with nothing declared: still 10,367,980.
- 1Gi declared: `dd` stopped at 856 MiB; the next write failed with `No space
  left on device`; the Pod stayed `Running` and was usable after the file was
  removed.
- Undeclared: 8,388,608,000 bytes written to `/workspace` in 155.9 s (51.3
  MB/s), then the same to `/tmp` in 155.7 s.
- The root filesystem is not writable by UID 65532; `/tmp` and `/workspace`
  are. `HOME` is `/`.

#### Guest CPU and memory

| Requests / limits | `nproc` | `MemTotal` (kB) |
|---|---|---|
| none | 1 | 132,460 (2026-11-11) |
| 250m / 256Mi | 1 | 389,908 |
| 500m / 512Mi | 1 | 647,192 |
| 1 / 1Gi | 1 | 1,161,748 |
| request 1 / 1Gi, limit 2 / 2Gi | 1 | 1,161,744 |
| 2 / 4Gi | 2 | 4,247,324 |

The guest is sized from requests, not limits. Memory exhaustion in a 1 GiB
guest: the command exits 137, the container is `OOMKilled`, the Pod is
`Failed`, and the next exec fails with `container not found ("agent")`.
`pids.max` is `max`; `threads-max` was 9,013 in a 1 GiB guest.
`activeDeadlineSeconds: 40` ended the Pod as `Failed` / `DeadlineExceeded`.

#### Instance types

Hand-made sandbox Pod per type (cluster 1), 1 CPU / 1 GiB:

| Type | Cold start to Ready (s) | DMI product name |
|---|---|---|
| `m5.xlarge` | 52.5 | `m5.enclave` |
| `c5.xlarge` | 40.3 | `c5.enclave` |
| `r5.xlarge` | 28.7 | `r5.enclave` |
| `m6i.xlarge` | 88.6 | `m6i.enclave` |
| `m7i.xlarge` | 41.3 | `m7i.enclave` |
| `c7i.xlarge` | 24.5 | `c7i.enclave` |
| `m6a.xlarge` | 25.8 | `m6a.enclave` |
| `m7a.xlarge` | 28.0 | `m7a.enclave` |
| `m5d.2xlarge` | 78.1 | `m5d.enclave` |
| `m6id.xlarge` | 28.7 | `m6id.enclave` |
| `m7g.xlarge` (arm64) | 29.4 | no `/sys/class/dmi` |
| `m6gd.xlarge` (arm64) | 27.5 | no `/sys/class/dmi` |

`TestK8sNitroIsolated_EndToEnd` passed on `m5d.xlarge` (both regions),
`m5.xlarge`, `m7a.xlarge`, `m7i.xlarge` and `c8a.large` (chosen by the
built-in pool). On `m7g.xlarge` it fails by design: `runtime diagnostics for
executor.runtime "nitro-isolated" failed: dmi_product_name:
/sys/class/dmi/id/product_name is absent, unreadable or not a regular file`,
and the Pod is removed.

Both architectures expose `/dev/nsm` in the guest and a dm-verity root on the
kernel command line. amd64 guests report `Amazon EC2` for `sys_vendor`,
`bios_vendor`, `board_vendor` and `chassis_asset_tag`.

Warm start (existing sandbox node): 6.6 to 7.9 s over about 45 Pods. An
executor Pod on a warm node was created at 10:27:42 and Ready at 10:27:48.

#### Node pools

- A node launched for an ordinary Pod is labelled
  `eks.amazonaws.com/nitro-sandbox: "false"` and cannot host a sandbox; Auto
  Mode launches a separate node for the first sandbox Pod. A placeholder Pod
  keeps a sandbox node warm only if it is itself a sandbox Pod.
- A sandbox node advertises 5 `eks.amazonaws.com/nitro-sandbox` per vCPU (20
  on an `xlarge`, 10 on `c8a.large`). Ten sandbox Pods (250m / 256Mi) ran on
  one `m5d.xlarge`, all Ready within 28 s of being created together.
- **Auto Mode's built-in `general-purpose` pool hosts sandboxes with no custom
  NodePool.** For a sandbox Pod selecting only that pool and `amd64` it
  launched `c8a.large` (Ready in 25.3 s). The full executor group and a real
  `stirrup harness` run passed there. With every custom NodePool deleted, the
  unmodified example RunConfig (node selector `amd64` and `m5d.xlarge`) was
  served an `m5d.xlarge` by the built-in pool and the run succeeded 46 s after
  starting from no node. The built-in pool's requirements were: on-demand,
  instance category `c`, `m` or `r`, generation above 4, `amd64`, Linux.
- An untainted arm64 pool beside amd64 pools catches ordinary Pods that have
  no architecture selector.

#### Annotations

- `network-isolated`: only the exact value `"true"` isolates. `"True"`,
  `"TRUE"`, `"1"` and `"yes"` leave the Pod `Pending` with
  `FailedCreatePodSandBox ... invalid sandbox.amazonaws.com/network-isolated
  annotation`. `"false"` or no annotation gives a networked guest (`eth0`,
  default route via 169.254.1.1, internet reachable, IMDS answers 401).
- The key is case-sensitive: `Sandbox.amazonaws.com/network-isolated: "true"`
  alone gives a networked guest.
- `log-bandwidth` and 11 MiB of exec output: default 44.2 to 44.8 s; `20M`
  4.45 to 4.69 s; `200M` 2.28 s; `disabled` 2.49 s. `"0"` leaves the Pod
  `Pending` (`resolves to 0 bytes/sec`); `"banana"` behaves as the default.
- An unknown `sandbox.amazonaws.com/*` key is ignored by the platform.

#### Admission mutation

| Mutation | Outcome |
|---|---|
| LimitRange defaulting `ephemeral-storage` | run fails: `admitted pod ... does not match the submitted pod: spec.containers[agent].resources: ...`; nothing left behind |
| MutatingAdmissionPolicy sets `log-bandwidth: 200M` | allowed; run passes |
| MutatingAdmissionPolicy adds `sandbox.amazonaws.com/policy` | run fails: `metadata.annotations[sandbox.amazonaws.com/policy]: expected absent, observed "..."` |
| MutatingAdmissionPolicy removes `network-isolated` | run fails: `metadata.annotations[sandbox.amazonaws.com/network-isolated]: expected "true", observed absent` |

Each failure emitted `sandbox_runtime_diagnostic_failed` with check
`admitted_pod`, exited 1 before any provider call, and left no Pod or
NetworkPolicy.

#### Whole runs

`stirrup harness` with model `claude-sonnet-5-5`:

| Run | Result |
|---|---|
| Dry run, admin credentials | OK; `RuntimeClass "isolated-sandbox" exists`; nothing created |
| Dry run, reference `stirrup-orchestrator` service account | OK; RuntimeClass and namespace "not checked" |
| BusyBox task (write and run a script), warm node | success, 25 s |
| Go task (module with tests, `go test`, `go vet`), `golang` image, 2 CPU / 4 GiB | success, 69 s; the agent set `GOCACHE`, `GOPATH`, `HOME` under `/workspace` |
| Trivial task as the reference service account | success, 17 s; the reference Role is sufficient |
| Trivial task from a cold start (no node) | success, 47 s |
| Trivial task on the built-in pool (`c8a.large`, warm) | success, 13 s |
| Unmodified example RunConfig, built-in pool only, cold | success, 46 s |

### Where the implementation departs from this plan

Each of these was decided during implementation without the owner present and
is called out in the pull request that contains it.

- **Runtime diagnostics read different files.** The plan says to check that
  `/sys/class/net` contains only `lo`. The spike's raw output shows `lo` and
  `dummy0` in a network-isolated guest, and `/sys/class/net` reflects the
  network namespace of whoever mounted that sysfs. The implementation reads
  `/proc/net/dev` (only `lo` and `dummy0` allowed) and `/proc/net/route` (no
  entries), with shell builtins.
- **The admitted Pod is compared as a whole.** The plan lists the fields to
  check. The implementation classifies every `PodSpec` and `Container` field
  as compared, platform-owned or may-gain-entries, and a test fails when
  either struct gains a field the table does not name. It also requires the
  two marks the EKS RuntimeClass puts on the Pod (the
  `eks.amazonaws.com/nitro-sandbox` overhead resource and node selector),
  which a sandbox image cannot forge.
- **A fixed `sandbox.amazonaws.com/log-bandwidth: "20M"` annotation is set.**
  The plan defers throttle annotations until measured. The spike measured
  that the default 2M throttle also throttles exec output (11 MiB took 44
  seconds against 4.48 seconds at 20M), and exec is the only channel the
  executor reads results over. The value is not operator-configurable;
  `executor.k8sPodAnnotations` stays deferred. A cluster mutating policy may
  still retune the documented throttle annotations; any other key under
  `sandbox.amazonaws.com/` fails the run.
- **Sandbox identity and the git proxy are rejected for `nitro-isolated`.**
  V12 confirmed the memory-backed token directory is tmpfs, and the token
  re-exchange after a slow start is implemented for every runtime. Identity
  is still rejected for this runtime in the first release: an offline guest
  has no network path on which to use the token, and the token lifecycle was
  not exercised on a Nitro guest.
- **Teardown uses a one-second graceful delete, not a zero-grace delete**, for
  every runtime, and the sandbox Pod sets `terminationGracePeriodSeconds: 1`.
  An ambiguous create result keeps the egress policy.
- **`k8s-sandbox` teardown follows the same contract** and reads the backing
  Pod until it is gone, which the plan did not ask for.
- **The `k8s` executor's Pod sets `enableServiceLinks: false`**, for every
  runtime. Otherwise Kubernetes injects environment variables for each
  Service in the namespace, which the whole-Pod comparison cannot predict.
  The `k8s-sandbox` Pod is unchanged.
- **The constructor waits for exec to reach a Ready Pod** (`782f51ba`), for
  every `k8s` runtime. The plan's readiness design ends at the Pod's Ready
  condition. The live run showed that on a new node exec is refused until
  the kubelet's serving certificate is issued, after the Pod is Ready (see
  [Defects found](#defects-found)). The wait runs `/bin/sh -c :` and retries
  for up to a minute only while the API server cannot dial the kubelet or an
  attempt times out; any other failure is left to the first real exec. The
  `k8s-sandbox` executor does not run it.
- **`resources.diskMb` is accepted under `nitro-isolated`** (`bc177e8d`). The
  plan rejects it until V8 measures enforcement; the live run measured it
  (see [Accepting `diskMb`](#accepting-diskmb)). For every `k8s` runtime
  `diskMb` is now submitted as an `ephemeral-storage` request as well as a
  limit, because the API server defaults an absent request to the limit and
  the whole-Pod admission check compares the admitted Pod with the submitted
  one.

### Live items still open

Closing any of these needs a paid EKS run with its own authorization. #895
tracks them.

1. Networked Nitro sandboxes (`allowlist`), which the runtime rejects; PR 5
   needs its own design (#896).
2. NetworkPolicy enforcement with this runtime (`DefaultDeny`, the network
   policy controller). Both 2026-11-13 clusters used `DefaultAllow`.
3. Pod Identity or IRSA on the sandbox service account. That its webhook
   injects a token volume, which the whole-Pod admission check would reject,
   rests on AWS documentation.
4. IPv6 clusters, standard node groups and FIPS AMIs.
5. arm64. The guest has no `/sys/class/dmi`, so the DMI diagnostic fails by
   design; support needs another diagnostic.
6. Long runs: no sandbox lived longer than about 20 minutes.
7. Whether a Nitro Sandbox honours an image patch or an ephemeral container
   added after readiness.
8. The delay before Auto Mode removes an idle sandbox node.
9. The `k8s-sandbox` executor, which stays gVisor-only, was not run.

The nine items open before 2026-11-13 are closed; each result is in
[Results against the open items](#results-against-the-open-items):

1. Guest workspace size with no `ephemeral-storage` declared: 10,367,980
   KiB (9.89 GiB); `diskMb` is now accepted.
2. Candidate acceptance against the implemented executor: the gated group
   passed 10 times.
3. Whole-Pod admission check against a real `Create` response: passes; the
   response carries both platform marks.
4. Runtime diagnostics in their implemented form: pass.
5. Graceful-delete timing: 3.2 to 5.3 s from `Close` to "Pod gone".
6. DMI product name on other families: `<family>.enclave` on 11 amd64
   families; none on arm64.
7. Field classification: no false failure.
8. Throttle annotation list: all eleven keys accepted together; a policy
   retuning `log-bandwidth` passed.
9. Timeout, process-tree kill and output cap at `20M`: pass every time, with
   no `could not kill timed-out command` warning.

Separately, the `terminationGracePeriodSeconds: 1` field now sent by
`k8s-sandbox` has not been accepted by a live GKE Agent Sandbox admission
policy; one `TestAgentSandbox_Lifecycle` run on GKE settles it before #887
merges.

## Implementation handoff gates

The executor direction is approved as a design recommendation. General
hardening (PR 1 and PR 4) can proceed. Nitro enablement is **not ready
for release** until the paid spike establishes the gates below. The original
review authorized no AWS provisioning or spend. The owner subsequently authorized
the dated spike under a USD 25 soft/USD 40 hard budget; that is not standing
authorization for future paid runs.

Decisions adopted by this review so agents need not invent them:

- Use `executor.runtime: nitro-isolated`, mapped to AWS's
  `isolated-sandbox` RuntimeClass. No arbitrary RuntimeClass override.
- The first release accepts only `network.mode: none` for this profile.
  Reject `allowlist` in both RunConfig validation and direct executor
  construction before any cluster mutation. PR 5 is a later design and
  validation gate for networked support, not a probe to ship as written
  in the original draft. Existing runtimes keep their current API.
- Require explicit positive CPU and memory for Nitro. Do not infer
  defaults through a server-side dry-run create. Include 1 CPU and
  1024 MiB in the example as a starting point, not a measured minimum.
- PR 2 and PR 3 are one enablement change (or a stack that merges
  together). No accepted Nitro runtime may ship without its checks.
- The initial supported recipe pins IPv4, amd64, a tested instance-family
  allowlist and a sandbox image digest. Architecture alone does not pin
  a tested family. Broader support needs its own recorded evidence.

| Work | Entry condition | Required exit evidence |
|---|---|---|
| PR 1 readiness/admission and PR 4 preflight | Repository tests and contracts | Tests below, no AWS requirement |
| Paid spike | Owner supplies account, region, spending ceiling, teardown owner | Dated results with exact versions, manifests, image digest, family, commands and failures |
| PR 2 + PR 3 enablement | PR 1 and PR 4 landed; spike results reviewed | V1–V5, V7, V10–V11 pass; V12 passes if identity is enabled; admission mutation and rollback tests; token expiry handled if identity is enabled |
| Disk quota claim | V8 | Measured enforcement; otherwise reject explicit Nitro `diskMb` rather than imply a hard bound |
| PR 5 networked support | Separate reviewed network contract | Startup, execution and teardown confinement; DNS and credential paths accounted for; no inconclusive probe treated as success |

V6 is required for PR 5. V9 is an optimization input, not a release
blocker for offline execution. V13 verifies the chosen region; it need
not survey every region. Unsupported configurations fail explicitly.
An agent must not mark a skipped live test as a pass or replace a gate
with an AWS documentation quote.

## Live spike findings incorporated on 2026-11-11

The dated spike supports the offline
executor direction on the tested recipe. It does not approve release: no
implementation candidate, admission mutation/rollback suite, identity-expiry
handling or full harness acceptance was exercised.

- V5's central claim held: the network-isolated annotation removed guest IP
  networking while exec and binary tar I/O worked. UID 65532/fsGroup workspace
  access and the `.enclave` DMI diagnostic worked on `m5d.xlarge`.
- V6 found actual startup leakage with controller enabled and DefaultAllow:
  three requests escaped a preinstalled policy. A separate DefaultDeny node
  blocked ten sampled startup requests with healthy positive controls. IMDS
  still returned 401 under deny-all in both profiles; credential issuance was
  not tested. Keep Nitro allowlist rejected.
- V8 did not establish a hard disk quota: a 128 MiB write succeeded against a 64 MiB
  limit without eviction over the observation window. **Reject explicit Nitro
  `diskMb` in the initial implementation.** *Corrected 2026-11-13:* the
  declaration sets the guest disk size with a floor of about 500 MiB, so the
  64 MiB declaration gave a 385 MiB disk and the 128 MiB write fitted; the size
  is a hard limit, and `diskMb` is now accepted (see
  [Accepting `diskMb`](#accepting-diskmb)).
- V12 confirmed memory emptyDir was tmpfs and the dummy-token writer worked.
  Identity configuration still needs the remaining token lifecycle/expiry gates.
- V10 observed heartbeat arrivals 1.94 s after a successful zero-grace delete
  response. Preserve confinement through confirmed termination; response success
  is not a guest-stop acknowledgement.
- Add the default `sandbox.amazonaws.com/log-bandwidth: 2M` to G7. It also
  throttled exec stdout: 11 MiB took ~44 s; 20M reduced that to 4.48 s. The original 30 s
  output-cap probe failed; targeted 90 s probes returned the expected cap error.
  Resolve output budgets and any permitted annotation in implementation rather
  than silently relabelling that failure.
- A clean cold-node sample reached Ready in 43.84 s; warm BusyBox samples were ~6–8 s.
  These are samples, not timeout ceilings. Full harness dependencies downloaded
  through the real proxy in 10.58 s after a 1.224 s clone, with default throttles.
- Pin the initial recipe to the recorded amd64 `m5d.xlarge`, IPv4 and image
  digest. The live RuntimeClass had no toleration. Current AWS region wording
  now says all commercial regions; only us-east-1 was verified here.

PR 1/PR 4 and the combined PR 2/PR 3 candidate checks remain required. PR 5 remains a
separate design and validation gate.

### Tested recipe (2026-11-11)

The initial supported recipe pins these values. Broader support needs its own
recorded evidence.

| Item | Tested value |
|---|---|
| Cluster | EKS Auto Mode, Kubernetes 1.36, platform `eks.17`, IPv4, us-east-1 |
| Instance | `m5d.xlarge`, amd64, on-demand |
| AMI | `ami-09ef60d209279212e` (`eks-auto-standard-1.36-x86_64-20261031`) |
| Node software | Bottlerocket 2026.10.31, kubelet `v1.36.4-eks-cfb47f5`, kernel 6.18.56, containerd `2.2.9+bottlerocket`; guest kernel 6.18.54 |
| Sandbox image | `public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4` (BusyBox 1.37.0-glibc) |
| Workload image (V9 only) | `public.ecr.aws/docker/library/golang@sha256:025eefc1e8dbe63201734a72aaa0a55d7a077f2d8410432f8ae2f3e939f32871` (Go 1.27.3) |
| Pod shape | 1 CPU / 1 GiB requests equal to limits, UID 65532, fsGroup 65532 (primary GID 0), all capabilities dropped, `RuntimeDefault` seccomp, no service-account token |
| Stirrup code | `cc8d529d`; probes called production `podExecCore` on hand-made Pods, bypassing the constructor and validation |

Facts measured on that recipe that the implementation needs:

- The `isolated-sandbox` RuntimeClass sets node selector
  `eks.amazonaws.com/nitro-sandbox: "true"` and overhead of 256 MiB plus one
  `eks.amazonaws.com/nitro-sandbox` extended resource. Admission left
  container requests and limits unchanged.
- A network-isolated Pod still has a PodIP in its API status, so the check
  must look inside the guest, not at the Pod status. `/proc/net/dev` in a
  network-isolated guest listed `lo` and `dummy0` (all counters zero); a
  networked Nitro guest listed `lo`, `dummy0` and `eth0`. `/sys/class/net`
  was not read in the spike, so the earlier instruction here to check that it
  holds only `lo` was not based on a measurement; the implementation reads
  `/proc/net/dev` and `/proc/net/route`.
- `activeDeadlineSeconds` counts from Pod start and so includes VM boot (a
  120-second deadline stopped the container about 121 seconds after start).
- The workspace `emptyDir` was XFS mounted `noquota`. Its size followed the
  declared `ephemeral-storage`: 394,220 KiB (about 385 MiB) at 64Mi and
  930,796 KiB (about 909 MiB) at 1Gi. Two points, no formula; the size with
  no declaration was not measured. The 64 MiB limit did not bound a 128 MiB
  write (V8). *Corrected 2026-11-13:* see [Guest disk](#guest-disk) for the
  size with no declaration and the floor that let the write fit.
- Guest `MemTotal` was 1,161,772 kB with 1 GiB of container memory and
  132,460 kB (about 130 MiB) with no requests or limits. Requests equalled
  limits in every sized Pod, so the spike does not show which of the two
  sizes the guest. *Settled 2026-11-13:* the requests do (see
  [Guest CPU and memory](#guest-cpu-and-memory)).
- Output-cap timing for 11 MiB over exec: 44.07 s at the default 2M log
  throttle, 4.48 s at `log-bandwidth: 20M`, 3.39 s on an ordinary Pod.
- Create-to-Ready on a warm node was 5.8 s, 6.6 s and 7.8 s across three
  Nitro Pods (2.8 s for an ordinary Pod); 43.84 s on a clean cold node.
- `podExecCore.Exec` checks context expiry before the output-cap flag. Under
  the 2M log throttle, a stream that reaches the cap can still return a
  timeout. Output-cap tests need a measured timeout for this runtime.
- `exec` negotiated WebSocket `v5.channel.k8s.io`; forced SPDY/3.1 also
  worked. The automatic fallback branch was not exercised (V2).
- With a `DefaultDeny` NodeClass, deleting a Pod's policy does not restore
  egress, so the egress proxy Pod needs its own explicit egress-allow policy.
  The proxy also needed one on the `DefaultAllow` node; that cause was not
  isolated.
- Auto Mode serves CoreDNS node-locally at the service CIDR's `.10` address.
- Setup pitfalls for future spikes: optional custom NodeClass tags fail the
  managed `AmazonEKSComputePolicy` tag-key condition
  (`CreateLaunchTemplateAuthCheckFailed`); default AWS CLI EC2 listings hide
  Auto Mode managed instances and volumes, so cleanup must query recorded IDs.
  The modelled cost of the run was about USD 0.26.

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
   RuntimeClass carries the node selector. The tested Auto Mode class had
   no toleration; custom operator taints may still need one. Standard node groups need a custom AMI.
   Kubernetes 1.36 is the documented minimum; confirm the current
   supported version window before provisioning. [EKS-OVERVIEW] [EKS-ISOLATED]
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
- **Regions.** Current announcement and EKS overview wording say all commercial
  regions. The earlier documentation conflict has resolved. The spike verified
  us-east-1 only. [WHATSNEW] [EKS-OVERVIEW]

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
anything else (`types/runconfig.go:6259`). This is the immediate configuration blocker; the handoff gates also
block a supported release.

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
restricts non-DNS traffic to the proxy
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
limits needs Pod annotations. The spike measured a separate default 2M log
throttle affecting exec stdout: 11 MiB took about 44 seconds; setting
`sandbox.amazonaws.com/log-bandwidth: 20M` reduced that to 4.48 seconds.
The original 30-second output-cap probe timed out. Resolve transfer budgets
and permitted annotations explicitly. A single clone/dependency experiment
succeeded under default throttles; broader workload effects remain unmeasured.

**G8. `confidential-sandbox` does not fit an external harness.** The
mode protects the workload from the node operator. Stirrup's threat is
the reverse: protect everything else from the workload, which isolated
mode covers. Mechanically, the default confidential policy denies
`Exec` and stdin [EKS-CONFIDENTIAL], and `podExecCore` has no other
channel. AWS also supports `--allow-exec-all-for-container`, so arbitrary exec
is possible with an explicit policy. The exclusion here is a product
scope decision, not a protocol impossibility. [EKS-CONFIDENTIAL]
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

**G10. The tested `diskMb` mapping is not a hard workspace quota.** It maps to an
`ephemeral-storage` limit (`k8s.go:435`), which the kubelet enforces by
measuring usage. Usage inside a Nitro Sandbox reports as zero.
[EKS-LIMITS] AWS's stated workaround is to set the limit anyway, which
suggests the runtime sizes the overlay from it, but the documentation
does not say so. The live spike wrote 128 MiB against a 64 MiB limit and
observed no eviction over the follow-up window. Reject explicit Nitro `diskMb`
until its requested-byte enforcement can be established; do not infer unlimited
storage or a general size formula from that sample. *Corrected 2026-11-13:*
enforcement is measured and `diskMb` is accepted (see
[Accepting `diskMb`](#accepting-diskmb)).

**G11. `--dry-run` creates a real sandbox for `k8s`.** The preflight
special-cases only the `container` executor
(`harness/internal/core/preflight.go:337-370`); for `k8s` it builds the
real executor under a 30-second budget (`preflight.go:94`). This is
already true today. On a Nitro Sandbox it boots a VM, may provision a
node, and will usually time out.

**G12. Memory-volume behavior is measured; identity lifecycle remains gated.** The
token directory is an `emptyDir` with `medium: Memory`
(`k8s.go:253-262`), and `docs/executors/k8s.md:680-682` promises the
token never touches node disk. AWS says `emptyDir` volumes live on the
guest's overlay disk, which is backed by the node's instance store,
and does not say whether memory-medium volumes are an exception.
[EKS-STORAGE] The spike confirmed the memory-medium token directory was tmpfs
with guest swap disabled and the production dummy-token writer worked. This
resolves the mount question for the tested recipe only; real identity exchange,
refresh and expiry remain untested.

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
VM that is slower to stop widens the window in `allowlist` mode. The spike
observed heartbeat arrivals 1.94 seconds after a successful delete response,
with a healthy receiver afterward. Receipt timing can include buffering and
is not exact CPU-halt evidence; it does not justify removing policy early.

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
    // verify checks guest-reported runtime diagnostics, not attestation.
    // Nil means the runtime has no diagnostic check.
    verify func(ctx context.Context, core *podExecCore) error
}
```

Entries on day one:

| `executor.runtime` | RuntimeClass | Sized from requests | No-network annotation | Verifier |
|---|---|---|---|---|
| `""` | unset | no | none | none |
| `runc`, `gvisor`, `kata-*` | same as the value | no | none | none (follow-up) |
| `nitro-isolated` | `isolated-sandbox` | yes | `sandbox.amazonaws.com/network-isolated: "true"` | DMI product name ends `.enclave` |

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
  `runtimeClassName` costs nothing and separates the technology name from AWS's class name.
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

Require positive explicit CPU and memory for Nitro in validation and
in the direct constructor. Preserve existing namespace-default behavior
for other runtimes. This avoids a second admission pass and a race
between dry-run defaults and real admission. Reject unsupported disk
quota claims until V8 establishes enforcement (established 2026-11-13; see
[Accepting `diskMb`](#accepting-diskmb)).

Inspect the actual Pod returned by `Create`, then recheck the same UID
at readiness. Fail on unexpected RuntimeClass, missing or changed
no-network annotation, or changed policy-selector labels. For Nitro,
check the expected container set, commands, security settings, service
account, credential-bearing env/volumes and positive resource requests
and limits against the submitted contract. Allow ordinary API defaults
and RuntimeClass scheduling/overhead; do not require whole-object
byte equality. Reject injected sidecars or init containers in the first
supported recipe. Trusted admission configuration must preserve these requirements
before scheduling. The operator owns that configuration and RBAC. PR 2
must document excluded mutating webhooks and the deployment checks;
a generic cluster admission-policy engine is outside this change.

A response check occurs **after creation** and cannot stop the Pod
starting first. It detects configuration drift and prevents tool use;
it does not undo exposure during startup. The trusted cluster must
preserve Stirrup's Pod contract at admission. Include a stripped
annotation and rewritten labels in mutation tests, not just a changed
RuntimeClass. A dry run would not remove this race.

Narrow `classifyPodCreateError` so it blames the RuntimeClass only when
the API server cause names that field. Preserve other admission errors.
Use the teardown contract below on every failure after creation.

### Readiness

One change for all runtimes, replacing the constant:

- A single ceiling of five minutes, still capped by the caller's
  context.
- Fail immediately on terminal states: Pod phase `Failed`, or a
  Pod phase `Succeeded` before readiness, `InvalidImageName` or a
  permanent configuration error. `ErrImagePull` and `ImagePullBackOff`
  can reflect transient registry failures: retry within the same
  bounded deadline and include their last reason in the timeout.
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
PR 1 must test a token expiring during startup. Before initial token-file
delivery and hooks, re-exchange an expired or nearly expired token or
fail construction and clean up. Refresh starts only after executor
construction today. Keep credentials out of failure diagnostics; an
expiry warning alone is not an acceptance criterion.

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
a port the guest binds) must be stated in the documentation. It is available to authorized parent-side processes with socket access,
not only after a compromise. The trusted node/operator is outside this
threat model; describe `none` as no guest IP network, not no possible I/O.

### Verifying the runtime after the Pod is Ready

After readiness and before the executor is returned to the factory,
run the profile's verifier over the same `exec` path the run will use.
It runs before lifecycle hooks. For Nitro isolated:

1. `/sys/class/dmi/id/product_name` ends with `.enclave`.
2. When `network.mode == "none"`: `/sys/class/net` contains only `lo`.
   This is a diagnostic consistent with the annotation taking effect.

A failed check fails executor construction and deletes the Pod and
policy through the existing cleanup path. The error states the value
observed. This is fail-closed with no opt-out field: the operator named
a runtime, and running under a different one is the event the runtime
choice exists to prevent.

These commands use binaries supplied by the sandbox image. An untrusted
image can fabricate their output. They are configuration diagnostics,
not attestation or proof against a malicious image, node or admission
controller. Keep the event name and documentation honest about that
limit. Do not weaken the hardware boundary's untrusted-code threat
model by treating a guest assertion as its trust anchor. Bound and
scrub observed output before logging it.

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
(`harness/cmd/stirrup/cmd/job.go:310-316`). Typed build failures remain an optional follow-up.

### Detecting unenforced egress policy

PR 5 needs a separate network design before implementation. The original
proposal to pass on the first refused or timed-out connection is
withdrawn: a dead listener, routing failure or transient loss can all
produce that result while policy enforcement is absent. A reachable
proxy does not establish the health of the forbidden destination.

A future probe must use an operator-controlled forbidden listener with
independent health/reachability evidence, a positive allowed control,
bounded repeated observations and explicit pass/fail/inconclusive
results. Inconclusive is an error, including a missing TCP client. The
image contract and destination provisioning must be settled before PR 5
is assigned. These remain sampled diagnostics, never lifetime proof.

The enforcement boundary must exist before any image code starts.
A probe after Ready cannot contain an image's startup code. Validate
EKS Auto Mode's controller enablement plus the selected NodeClass's
`networkPolicy: DefaultDeny`; do not assume the standard VPC CNI's
configuration applies to Auto Mode. [EKS-AUTO-NETPOL] [EKS-NODECLASS]

The network contract must also resolve these paths:

- `allowlistEgressPolicy` currently permits TCP/UDP 53 to any destination
  (`k8s_netpol.go`). Restrict it to approved resolvers and document the
  remaining DNS channel, or explicitly accept that exception. It is
  incorrect to claim all egress is forced through the proxy today.
- Kubernetes policies are additive. Another matching policy can reopen
  egress. Namespace policy ownership and workload-label integrity are
  operator prerequisites, not something one probe can establish.
- Auto Mode IPv6 Pods can have an additional IPv4 egress interface that
  NetworkPolicy does not cover. Initial support is IPv4 only; IPv6
  needs `enableV4Egress: false` and validation of both families.
  [EKS-NODECLASS]
- Metadata and Pod Identity are served inside the guest. Do not assume
  node NetworkPolicy filters these surrogate endpoints. Use a dedicated
  sandbox service account with no Pod Identity association or IRSA role,
  reject unexpected credential injection, and test the endpoints.
  With the current executor this means the default service account in
  a dedicated sandbox namespace; a configurable service-account name
  would need a separately specified API change.
  `automountServiceAccountToken: false` alone does not settle AWS
  credential exposure. [EKS-METADATA]

Validate startup, steady state, policy changes and shutdown, including
node loss, on each supported platform. Until then Nitro `allowlist` is
rejected, not enabled with a warning.

### Teardown and failed construction

G14 is an unconditional lifecycle correction. Apply it to `Close`,
readiness/verifier/admission failure, and ambiguous create responses.
Do not remove policy while deletion failed or termination is uncertain.
Use a bounded cleanup context independent of the canceled run and UID
preconditions so retries cannot delete a replacement Pod. Reconcile a
create timeout by the preselected name and ownership before cleanup.

Prefer normal graceful deletion and retain confinement until workload
termination is established. Force deletion followed by API `NotFound`
is not evidence the guest stopped, especially with an unreachable node.
[K8S-LIFECYCLE] On uncertainty retain the policy, report the orphan by
run ID, and document an operator cleanup/reconciliation procedure with
an owner. Do not attach a policy owner reference that removes it as soon
as a force-deleted Pod object disappears. Test delete failure, node loss,
cancellation, retry and no-policy-removal on ambiguous termination.



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

PR 1 and PR 4 are independently mergeable. PR 2 and PR 3 merge together
after their evidence gates; PR 5 requires a follow-up design. Line references will have drifted; re-verify.

### Step 0: live validation (no code; needs approval to spend)

See [Live validation](#live-validation-paid-needs-approval). The
findings are written back into this document. PR 1 and PR 4 do not
depend on it. PR 2 + PR 3 can be drafted before the spike, but must not merge until
the handoff gates pass. The runtime name and explicit sizing are settled
above. PR 4 must land before enabling Nitro.

### PR 1: readiness and admission checks for the `k8s` executor

Closes G2, G13, G14 and the admission-response half of G6. No Nitro content; it
benefits every runtime and is reviewable without AWS knowledge.

- `harness/internal/executor/k8s.go`: the readiness changes in
  [Readiness](#readiness); the admitted-RuntimeClass comparison;
  narrowed `classifyPodCreateError`.
- Apply the teardown contract to Close and constructor rollback.
- Unit tests for each terminal state, transient-error retry, the
  timeout message, and an admitted RuntimeClass that differs.
- `docs/executors/k8s.md`: the troubleshooting row for the 60-second
  timeout (line 745); the run-deadline and token-lifetime notes.
- The integration suite's per-test contexts are 90 seconds
  (`k8s_test.go:86` and others) and must rise with the ceiling, or the
  suite cannot run on an autoscaled cluster.

### PR 2: Nitro isolated runtime for the `k8s` executor

Closes G1, G3, G4, G8. Depends on PR 1, PR 4 and the handoff gates.
Merge together with PR 3. Reject Nitro allowlist before cluster calls.

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
  for Nitro isolated with mode `none`; missing or partial resources rejected; positive fractional CPU;
  admitted resource mutation rejected);
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
  (including first-release rejection of `allowlist`, the tested family
  and architecture pins, IPv4 scope, and admission/identity prerequisites); the egress enforcement caveat extended from
  kindnet to EKS Auto Mode; `examples/runconfig/k8s-eks-nitro.json` and
  its entry in `examples/runconfig/README.md`.
- Integration: make the suite exercise the annotation path, and add a
  `/proc/mounts` assertion to `TestK8sSandboxIdentityToken_RoundTrip`
  for G12.

### PR 3: verify the runtime in force

Adds the bounded runtime diagnostics for G6. Merge with PR 2; these
checks do not attest the runtime against a malicious image.

- Profile `verify` hook, called in `NewK8sExecutor` after readiness;
  the new security event and its plumbing.
- Unit tests with the `execFn` hook (`k8s_execcore.go:47`); integration
  test asserting the verifier passes on a real Nitro Sandbox and fails
  when the same check runs against a `runc` Pod.
- Docs: replace the manual verification step for this runtime; state
  what the check does and does not prove; `docs/security.md`.
- Follow-up issue: a gVisor verifier.

### PR 4: read-only `--dry-run` for `k8s`

Closes G11. Route both `k8s` and `k8s-sandbox` through metadata checks
that never construct an executor or create Pods, Sandboxes or policies.
Use namespaced reads and SelfSubjectAccessReview for the actual verbs
needed by each executor, including `pods/exec`; `k8s-sandbox` needs
Sandbox CR permissions, not merely Pod creation permission. Do not
require `namespaces/get` or RuntimeClass reads: the reference Role does
not grant these cluster-scoped permissions. Report unavailable optional
checks as not checked, not success. Honor `--no-probe-executor` before
client calls. Test that neither constructor nor persistent create/delete
is invoked and that skip works for both executor types. Authorization
review requests are allowed; they do not create a workload.

### PR 5: fail when egress policy is not enforced

Requires a reviewed follow-up design under
[Detecting unenforced egress policy](#detecting-unenforced-egress-policy).
Do not assign implementation until startup enforcement, DNS scope,
credential paths, destination health and the image contract are settled.
G14 is handled by PR 1, not deferred until a measured exploit window.
Only this follow-up can enable Nitro `allowlist`.

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

The first authorized spike has been run; its outcomes are in
[Live spike findings](#live-spike-findings-incorporated-on-2026-11-11) and
[Tested recipe](#tested-recipe-2026-11-11).
The candidate acceptance ran on 2026-11-13; its outcomes are in
[Live acceptance (2026-11-13)](#live-acceptance-2026-11-13).
The procedure below is retained for reproduction and candidate acceptance.
Future runs create billable resources and require their own authorization,
account, spending ceiling and teardown owner. Skipped candidate tests remain open.

Minimum environment, for roughly half a day:

- One EKS Auto Mode cluster on a currently supported sandbox Kubernetes
  version (minimum 1.36; recheck current support), in a verified US commercial
  region. Record NodePool/NodeClass manifests, AMI/runtime version, IPv4
  mode, tested family, image digest and controller settings. Check
  current family exclusions and SMT/memory constraints. [EKS-LIMITS]
- The standing objects from `examples/k8s/` (namespace, RBAC), and the
  egress proxy from `examples/k8s/egress-proxy/` deployed in the test
  namespace.
- A sandbox image that satisfies the image contract and also has a TCP
  client for the egress tests.
- A long-lived placeholder Pod with `runtimeClassName:
  isolated-sandbox`, so a capable node exists before the suite starts.
  *Corrected 2026-11-13:* no placeholder is needed for the candidate. With
  the 5-minute readiness ceiling and the `k8s` executor's wait for exec to
  reach a Pod on a new node, 8 of 8 Nitro cold starts passed (see
  [Defects found](#defects-found)). A placeholder keeps a sandbox node warm
  only if it is itself a sandbox Pod, because Auto Mode runs sandbox Pods on
  separate nodes from ordinary Pods (see [Node pools](#node-pools)).

The charges are the EKS cluster hourly fee, the Auto Mode management
fee and one or two mid-sized EC2 instances. Do not use the earlier unpriced "few dollars" estimate as a budget.
Before provisioning, price the selected region/families, storage, NAT
and load balancers as applicable; record a spending ceiling and named
teardown owner. Delete all dedicated resources and verify cleanup.

The integration suite cannot simply be pointed at the cluster. It
builds `K8sExecutorConfig` directly and so bypasses the validation gap
(G1), but on unmodified `main`:

- readiness is 60 seconds and each test's context 90 seconds, so a cold
  node or large image can fail the first test (hence the placeholder Pod);
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

Baseline research invocation, unmodified code (not candidate release acceptance):

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

For the implementation candidate, run the supported offline cases and
explicit rejection tests for Nitro allowlist. Networked baseline cases
are research for V6/PR 5 and must not be required to pass on a candidate
that intentionally rejects them. Record every skipped case and why.

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
| V1 | Do the supported offline suite cases pass (exec, bounded output, `tar` file I/O, process-group kill, timeouts), and does the candidate reject Nitro allowlist before cluster calls? | The verdict |
| V2 | Does `exec` use WebSocket or the SPDY fallback (`k8s_execcore.go:432`) through the sandbox runtime? | Robustness |
| V3 | Time to Ready with a node present, and with none. | Readiness ceiling; whether warm pools matter |
| V4 | Guest size with no requests, with requests set, and with fractional CPU. | G3 explicit sizing |
| V5 | With enforcement off, does a mode-`none` Pod reach the internet? With the annotation, does it not, and does `exec` still work? | G4, the design's central claim |
| V6 | With enforcement off and on, can an `allowlist` Pod reach `169.254.169.254`, the Pod Identity endpoint, and an arbitrary internet host directly? How long after Pod start does enforcement begin? | G5, PR 5 |
| V7 | Does `/sys/class/dmi/id/product_name` end in `.enclave` on every amd64 instance family permitted by the initial recipe? Graviton is a later extension. | PR 3 verifier |
| V8 | Is `ephemeral-storage` enforced inside the guest, and what does exceeding it look like? | G10 |
| V9 | Wall-clock time for a representative `git clone` and dependency install through the egress proxy under default throttles. | G7 |
| V10 | Do `activeDeadlineSeconds`, the owner reference and zero-grace deletion behave as on other runtimes? After deletion, how long does the guest keep running? | Lifecycle parity, G14 |
| V11 | Does UID 65532 with `fsGroup` get a writable `/workspace` inside the guest? | PR 2 |
| V12 | What is the mount type of the sandbox identity token directory inside the guest? If unverified, reject Nitro sandboxIdentity configuration. | G12, conditional identity support |
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

**Generating Cedar policies for the guest.** AWS supports both command-hash rules and per-container unrestricted
exec; policy generation is outside this first isolated-mode integration. Stirrup's own Cedar ring already
gates tool calls before they reach the executor.

**`ApplicationNetworkPolicy` instead of the egress proxy.** EKS Auto
Mode's hostname-based policy could replace the proxy for `allowlist`
mode. It allows the addresses a name resolves to, so other names on the
same addresses are reachable; it is one platform's API; and the proxy
also produces the security events Stirrup records. The proxy stays the
mechanism.

## Open decisions

The review adopted the runtime name, explicit sizing, offline-only first
release and combined enablement/checks above. These no longer require
agents to choose among alternatives.

1. **Paid spike:** authorization and execution for the 2026-11-11 run are recorded
   in the spike evidence at commit `bccf4862` under `docs/research/nitro-spike-2026-11-11/`. Future paid work needs a fresh budget and teardown owner;
   this report is not standing authorization.
2. **Supported deployment:** the spike owner must record tested family,
   image digest, AWS/Kubernetes versions and region, then limit the
   recipe to them. V8 and V12 determine disk and identity claims.
3. **Networked follow-up:** settle the PR 5 contract before assigning
   implementation, including the approved DNS exception, probe tooling
   and fixture ownership. Do not add warning-only bypasses.
4. **Typed build failures:** optional follow-up if a control plane needs
   them. PR 3 emits scrubbed diagnostic events and build-error text.

No remaining owner decision blocks PR 1 or PR 4. The dated spike and design
corrections above are recorded. Nitro release still requires the implementation
candidate, admission/rollback/configuration tests, lifecycle acceptance and
identity-expiry evidence if identity is enabled. Further measurements may change
the design; record those changes here before changing the agreed scope.

## Sources

Additional review sources, read 2026-11-11:

- [EKS-AUTO-NETPOL] <https://docs.aws.amazon.com/eks/latest/userguide/auto-net-pol.html>
- [EKS-NODECLASS] <https://docs.aws.amazon.com/eks/latest/userguide/create-node-class.html>
- [EKS-METADATA] <https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-metadata-credentials.html>
- [K8S-LIFECYCLE] <https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination>

AWS documentation, originally read 2026-11-10:

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
