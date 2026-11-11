# Nitro spike probes

Prepared 2026-11-11 against Stirrup's current checkout. Nothing in this directory
was executed against a cluster during preparation. Parent agent owns AWS writes.

`probes.py` uses Python's standard library and kubectl. `generate` only writes a
manifest. Every other action requires an explicit `KUBECONFIG` to avoid mutating
the default cluster. The caller must select a dedicated namespace and an image
pinned by digest. BusyBox needs `sh`, `sleep`, `tar`, `ls`, `readlink`, `setsid`,
`seq`, `yes`, `head`, `timeout`, `wget`, `awk`, `dd`, and `stat`.

The generated Pod has a 20-minute active deadline, zero termination grace, UID
65532, fsGroup 65532, dropped capabilities, no service-account token, an emptyDir
workspace and 64 KiB memory emptyDir at Stirrup's actual token path. Default
requests/limits are 1 CPU, 1 GiB memory, 64 MiB ephemeral storage. It selects
amd64 and `isolated-sandbox`. All Pods bear `nitro-spike-owned=true`.

## First run

Create the dedicated namespace first, preferably with restricted Pod Security.
Set a capped NodePool before creating a Pod. No script here manages NodePools.

```sh
export KUBECONFIG=/tmp/nitro-spike.kubeconfig
python3 /tmp/nitro-probes/probes.py create --image IMAGE@sha256:DIGEST
python3 /tmp/nitro-probes/probes.py inspect
```

Create records request/ready timing and the admitted Pod. Inspect records the
guest's DMI product name, block devices, CPU/memory, routes/interfaces, mounts,
UID, writable workspace, dummy-token write, tar stdin transfer, binary readback,
logs, and bounded GET attempts to example.com, 1.1.1.1, IMDS and Pod Identity IP.
GET errors must be interpreted: an HTTP 401/403 proves an endpoint was reachable,
even if wget exits nonzero. Name-resolution or routing errors alone are not a
complete confinement proof. Run the same command on a networked positive control.
No response body or actual credential is recorded.

## Real Stirrup I/O on the existing offline Pod

The Go overlay adds a research test without modifying tracked source. It calls
the production `podExecCore` against the manually created Pod. It does not call
the constructor, so it does not prove admission checks or config validation.

```sh
STIRRUP_TEST_KUBECONFIG="$KUBECONFIG" \
STIRRUP_TEST_NAMESPACE=nitro-spike STIRRUP_NITRO_SPIKE_POD=nitro-offline \
GOCACHE=/tmp/nitro-spike-gocache \
go test -overlay=/tmp/nitro-probes/overlay.json -tags integration_k8s \
  -count=1 -v -timeout=8m ./harness/internal/executor \
  -run '^TestNitroSpike_ExistingPod$'
```

This covers stdout/stderr/exit status, ASCII/UTF-8/NUL/nested tar round trips,
the production 10 MiB output cap, timeout and process-tree kill through both
Exec and ExecStream, plus a dummy token write and explicit tmpfs mount assertion.
HTTP transport evidence includes only method, Upgrade, response status and
WebSocket protocol. It never dumps request headers. GET + websocket + 101 is
direct WebSocket evidence; POST + SPDY is SPDY transport evidence. The separate
`forced_spdy` subtest forces the SPDY executor and labels that result separately;
it does not prove a natural fallback happened. No fallback is evidence that the
WebSocket route worked, not that SPDY is broken.

## Lifecycle cases

```sh
python3 /tmp/nitro-probes/lifecycle.py --image IMAGE@sha256:DIGEST
```

Run with a warm node. The deadline case creates a restricted isolated Pod with
activeDeadlineSeconds=120 and records Ready and terminal status observations.
The GC case creates two separate restricted isolated Pods with 600-second
deadlines, places a Pod owner reference on the child, then deletes the owner
with zero grace/background propagation. It measures disappearance of the child
from the API. Every API call is bounded to 30 seconds and readiness polls to
20 seconds. At most two test Pods run concurrently. Cleanup only deletes Pods
successfully created by this script; existing conflicting names fail create.
Guest halt after API deletion remains explicitly UNVERIFIED without an external
heartbeat observer. Detailed JSON lives under `results/lifecycle`.

## Additional bounded cases

- Run `create --name nitro-networked --networked --image ...` as the same-node
  positive control. Apply deny-all NetworkPolicy before it to reproduce the
  unenforced-policy case. Repeat `inspect` before/after enabling enforcement.
- Create/delete sequentially for `--no-resources` and `--cpu 500m` sizing cases.
  Do not leave all cases running and accidentally trigger more nodes.
- Compare a first Pod with an empty NodePool to a second Pod while the first
  keeps its node alive. Retain `create`, `wait`, admitted JSON and cluster events.
- `disk` writes exactly 128 MiB against the declared 64 MiB storage limit,
  observes Pod status for 90 seconds, and removes the file. Passing write and
  absence of eviction over 90 seconds only establish this bounded observation;
  they do not establish an unlimited disk or permanent lack of eviction.
- `--deadline 60` tests an active deadline but includes scheduling/boot. Record
  startup and container termination timestamps separately. API deletion does not
  prove the guest stopped; external heartbeat observation is needed for that.
- `snapshot` records Pods, events, nodes, RuntimeClasses, NodePools, NodeClasses
  and NetworkPolicies. Record Auto Mode controller ConfigMap separately.

## Existing integration suite subset

The unmodified suite creates resource-unspecified Pods and cannot apply the
network-isolated annotation. It is baseline evidence only. The useful narrow
subset is:

```text
TestK8sExec_(Success|NonZeroExit|Timeout|Cancelled|TimeoutKillsProcessTree|OutputCap|CommandOutputSpill)
TestK8sFileIO_RoundTrip
TestK8sExecutor_(Lifecycle|CloseIdempotent|ListDirectory_ReturnsWrittenEntries)
TestK8sReadFile_(Missing|Directory)
TestK8sListDirectory_(Missing|WorkspaceRoot)
TestK8sWriteFile_RejectsWorkspaceRoot
TestK8sRuntimeClass_Admitted
TestK8sSandboxIdentityToken_RoundTrip
```

Set STIRRUP_TEST_KUBECONFIG, STIRRUP_TEST_NAMESPACE, STIRRUP_TEST_IMAGE,
STIRRUP_TEST_RUNTIME_CLASS=isolated-sandbox and
STIRRUP_TEST_RUNTIME_CLASSES=isolated-sandbox. Warm a node first. Constructors
allow only 60 seconds to readiness; tests generally allow 90 seconds. Give the
whole Go invocation an explicit timeout with time left for cleanup. Egress
enforcement tests additionally require STIRRUP_TEST_ENFORCE_EGRESS=1 and a working
proxy for allowlist. Do not report default skips as passes.

The existing orphan test's 20-second Pod lifetime may expire before boot. The
existing GC test's owner lacks restricted securityContext and may fail admission.
Run hand-applied lifecycle objects if needed instead of weakening namespace
security or silently editing the acceptance suite.

## Cleanup

```sh
python3 /tmp/nitro-probes/probes.py delete --name nitro-offline
kubectl -n nitro-spike delete pod -l nitro-spike-owned=true --grace-period=0 --wait=true
```

This deletes Pods only. Parent agent must tear down NodePools, instances, EKS,
VPC and all other dedicated AWS infrastructure and verify remaining resources.
`activeDeadlineSeconds` is not an infrastructure cost cutoff.

## Local validation

Python syntax checked. Go overlay compiled successfully with
`go test -overlay=... -tags integration_k8s -run '^TestNitroSpike_ExistingPod$'
./harness/internal/executor`; the test skipped because no cluster variables were
set. This is compile evidence only.

`run-core.sh` regenerates the overlay for this checkout. It defaults to the
90-second output diagnostics; select `TestNitroSpike_ExistingPod` explicitly
for the original core suite, whose 30-second cap case failed at default throttle.
