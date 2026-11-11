# Nitro spike: executor, sizing, storage and lifecycle results

Observed 2026-11-11 in the dedicated `nitro-spike` namespace, EKS Auto Mode,
us-east-1, IPv4, amd64, `m5d.xlarge`. These are live research results, not release
acceptance. No Nitro implementation candidate was built. The research Go overlay
calls the current production `podExecCore` on a manually created Pod; it bypasses
`NewK8sExecutor`, config validation, admission comparisons and constructor cleanup.

## Tested versions and shape

- Stirrup checkout: `cc8d529de659eaa65628464ce910c4d5c7599f16`.
- Client library: `k8s.io/client-go v0.37.2`; local kubectl v1.37.0.
- Node kubelet: `v1.36.4-eks-cfb47f5`.
- Node OS: Bottlerocket `(EKS Auto, Standard) 2026.10.31 (aws-k8s-1.36-standard)`;
  node kernel `6.18.56`; containerd `2.2.9+bottlerocket`.
- Guest kernel: `6.18.54`, x86_64, build date 2026-10-22.
- Image: `public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4`
  (BusyBox 1.37.0-glibc, amd64 manifest). Registry manifest and layer checksums
  were verified during probe preparation; the actual admitted image is recorded.
- Default tested Pod: `runtimeClassName: isolated-sandbox`,
  `sandbox.amazonaws.com/network-isolated: "true"`, equal requests/limits of
  1 CPU, 1 GiB memory and 64 MiB ephemeral storage; UID 65532, fsGroup 65532,
  all capabilities dropped, no privilege escalation, RuntimeDefault seccomp,
  no service-account token. `/workspace` is emptyDir. The separate identity
  directory uses a 64 KiB memory emptyDir.
- Admission left those container resource requests/limits intact. RuntimeClass
  added overhead of 256 MiB memory and one `eks.amazonaws.com/nitro-sandbox`
  resource. The RuntimeClass node selector is
  `eks.amazonaws.com/nitro-sandbox: "true"`.

Evidence: `nodes-first.json`, `initial-runtimeclasses.json`,
`offline/nitro-offline-manifest.json`, `offline/nitro-offline-admitted.json` and
`offline/nitro-offline-guest.json`. Restrict these conclusions to the tested
family, image, architecture and versions. No other family or Graviton was tested.

## V1: core executor I/O — passed cases and a preserved failure

`offline/go-core.txt` records the first run. Passed:

- stdout, stderr and nonzero exit status;
- production tar-based file write/read for ASCII, UTF-8, embedded NUL and nested
  paths;
- timeout handling and process-tree termination through both `Exec` and
  `ExecStream`, including checking that a delayed grandchild did not create its
  marker;
- dummy identity-token delivery through the production writer and a tmpfs mount
  assertion.

The output-cap subtest **failed** with the original 30-second command timeout.
It emitted 11 MiB using `yes x | head -c 11534336` and returned a timeout instead
of `errK8sOutputCap`. This failure is retained, together with the first probe
revision at `offline/nitro_spike_existing-v1_test.go` (assertion at line 85).

Targeted diagnostics repeated the same command with a 90-second limit and
received the expected 10 MiB cap error after about 44 seconds over both
transports. This establishes that the cap functions with enough transfer time;
it does not turn the original 30-second test into a pass.

Source explains the error precedence: `writeCapBuffer.Write` stops retaining
bytes after its cap but reports successful writes to keep draining the stream.
It does not return a writer error. `podExecCore.Exec` waits for stream completion,
then checks context expiry before the cap flag. Slow output can therefore cause
a timeout even when the buffer limit was reached. The first run's cleanup
reported a missing process-group pid file, consistent with the producer shell
having already completed; that observation alone would not prove completion.
The later successful full-stream diagnostics resolve the immediate cap question.

Not tested: candidate Nitro allowlist rejection before cluster calls, all existing
integration cases, constructor admission/rollback, or full harness execution.
Do not mark V1's complete release gate passed on this evidence alone.

## V2 and the output-throttle finding

Successful upgrade evidence is in `offline/go-output-diagnostics.txt`:

- Normal production fallback executor chose HTTP GET, status 101,
  `Upgrade: websocket`, negotiated `v5.channel.k8s.io`.
- A separate forced-SPDY subtest used HTTP POST, status 101,
  `Upgrade: SPDY/3.1`, and correctly returned output/exit status.
- No natural WebSocket-to-SPDY fallback was induced. Forced SPDY demonstrates
  that transport works, not that the fallback branch was exercised.

The first probe's instrumentation inspected the original request's Upgrade
header, which inner transports add later, so it emitted no handshake lines.
Instrumentation was corrected after that run completed; the corrected evidence
logs only method, upgrade, status and negotiated protocol, never auth headers.

| Pod / transport | 1 MiB from `/dev/zero` | Original 11 MiB producer, expected cap error |
|---|---:|---:|
| Default Nitro, WebSocket | 4.196935 s | 44.066589 s |
| Default Nitro, forced SPDY | 3.978693 s | 44.057951 s |
| Nitro with `log-bandwidth: "20M"`, WebSocket | 1.317241 s | 4.483041 s |
| Ordinary Pod, WebSocket | 2.189900 s | 3.390326 s |

The 20M Pod retained the offline annotation, image, CPU/memory and security shape
of the default Nitro Pod. It was created fresh because throttle annotations are
creation-time inputs. Raising the log throttle by 10x reduced the 11 MiB case by
about 9.83x. This directly establishes an effect on exec stdout for this runtime,
not just `kubectl logs`. The ordinary control had no RuntimeClass and requested
100m CPU/128 MiB; it is a useful non-Nitro comparison, not a resource-matched
benchmark. These are individual observations, not latency percentiles.

AWS documents `sandbox.amazonaws.com/log-bandwidth` with default `2M` (250 KB/s)
as a log-streaming control. The adoption plan omitted it. The live A/B supplies
the additional exec-path evidence. Source, read 2026-11-11:
<https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-resource-isolation.html>.

Evidence: `offline/go-output-diagnostics.txt`,
`controls/log-fast/manifest.json`, `controls/log-fast/admitted.json`,
`controls/log-fast/go-diagnostics.txt`, `controls/ordinary-control/go-diagnostics.txt`,
and manifest/source SHA256 files under `controls/`.

## V3: Ready timings

The initial offline Pod took 113.580 seconds from the post-create wait start
until Ready. This includes node provisioning/initial setup effects and must not
be presented as a repeatable cold-start SLA. Cluster creation time is excluded.

Subsequent Pods on the existing node:

| Pod | Post-create wait to Ready |
|---|---:|
| Fractional CPU, 500m / 1 GiB | 5.794851 s |
| No resource requests or limits | 7.843334 s |
| 1 CPU / 1 GiB, log throttle 20M | 6.582493 s |
| Ordinary control, 100m / 128 MiB | 2.779709 s |

These measure kubectl-observed readiness after create returned, with different
resource shapes, warm node and warm BusyBox image. They are not scheduling-free
VM boot measurements. There were no repeated cold/warm distributions.
The initial >60-second observation included IAM/tag repair and is not a normal
startup measurement. The separate clean cold-node sample was 43.836 seconds.
Neither sample establishes a universal ceiling or justifies warm pools; keep
readiness tied to the overall context and report terminal scheduling failures.

Evidence: `offline/nitro-offline-ready-duration.json`,
`sizing/*-ready-duration.json`, `controls/summary.json`, plus admitted Pod
timestamps and `create`/`wait` records.

## V4: guest sizing

| Requested container resources | Admitted resources | Guest online CPUs | `/proc/meminfo` MemTotal |
|---|---|---:|---:|
| none | empty resource map | 1 | 132460 kB |
| CPU 500m, memory 1 GiB | same request and limit | 1 | 1161768 kB |
| CPU 1, memory 1 GiB | same request and limit | 1 | 1161772 kB |

Every sandbox received the RuntimeClass's 256 MiB overhead. The unsized guest
exposed about 129.36 MiB total memory, confirming that unspecified resources do
not produce a general-purpose development environment. Fractional 500m was
accepted and exposed one guest CPU. This does not establish the rounding rule
for every fractional value, CPU scheduling entitlement, or performance isolation.

`MemTotal` is guest-wide kernel reporting, not a measurement of the container's
enforced memory limit. The 1 GiB cases reported about 1134.54 MiB guest MemTotal;
do not describe that as a 1 GiB container quota bypass. No memory-exhaustion or
OOM-enforcement experiment was run. Explicit resource validation remains justified.

Evidence: `sizing/nitro-{unsized,fractional}-{manifest,admitted,guest}.json` and
the offline baseline files.

## V7: runtime diagnostics

The tested Nitro Pods reported `m5d.enclave` from
`/sys/class/dmi/id/product_name` and virtio block devices `vda`, `vdb`, `vdc`.
The ordinary control on m5d reported `m5d.xlarge` and NVMe devices instead.
This supports the proposed diagnostic check for this one tested family.

It is not cryptographic attestation, proof against a malicious image or parent,
or evidence for untested families. Presence of the requested RuntimeClass alone
remains insufficient runtime evidence. The ordinary contrast executed guest
commands; no production runtime-verifier implementation was tested.

Evidence: `offline/nitro-offline-guest.json`, `sizing/*-guest.json`,
`controls/log-fast/guest.json`, `controls/ordinary-control/guest.json`.

## V8: explicit disk quota is not supported by this result

The admitted Pod had equal ephemeral-storage requests/limits of 64 MiB.
`/workspace` mounted `/dev/vdb2` as XFS with `noquota`. A bounded `dd` wrote
134217728 bytes (128 MiB) successfully; `du` reported 131072 KiB for the file.
The Pod remained Running in every status observation, from the immediate check
at 23:03:09 through the final check at 23:04:43 UTC. The file was then removed
successfully. The intended 90-second observation included API overhead and lasted
about 94 seconds between the first and last status snapshots.

Before/after `df` reported 394220 KiB filesystem capacity and used blocks rising
from 33952 to 165400 KiB. The experiment exceeded the declared 64 MiB limit
without a write rejection or observed eviction. It did not fill the filesystem,
establish unlimited storage, or rule out later eviction under other conditions.
The `dd` bandwidth number reflects buffered writes and is not a disk throughput
benchmark. The result does not validate `emptyDir.sizeLimit`, which was absent.

Recommendation: preserve the plan's fail-closed choice to reject explicit Nitro
`diskMb` until a supported enforcement mechanism is measured. Do not map it to
ephemeral-storage and imply a hard quota.

Evidence: `offline/nitro-offline-disk-{before,write,remove}.json`,
`offline/nitro-offline-disk-pod-*.json`, admitted resources and mount output.

## V10: deadline and owner-reference lifecycle

The restricted offline deadline Pod used `activeDeadlineSeconds: 120` and
zero termination grace. It reached Ready. Recorded timestamps:

- Pod startTime: 23:07:45 UTC.
- Container startedAt: 23:07:50 UTC.
- Ready/ContainersReady became false: 23:09:45 UTC.
- Container finishedAt: 23:09:46 UTC, exit 137.
- The polling client observed phase Failed / reason DeadlineExceeded after
  125.642 seconds from its post-create timer start.

The deadline therefore includes startup time and stopped this container at
about 121 seconds after Pod start, rather than 120 seconds after readiness.
This is consistent with the lifecycle design's need to allow for VM boot.

The separate GC case created restricted isolated owner and child Pods, both
Ready, with a real Pod ownerReference on the child. Zero-grace background
deletion of the owner was followed by child API absence within 4.274808 seconds.
Deletion responses and subsequent reads are preserved in `lifecycle/events.json`.

This result establishes deadline/status and owner-GC behavior for the tested
objects. It does not establish how long a guest continued running after its Pod
vanished from the API. That requires the separately owned heartbeat experiment.
Do not merge that later evidence into this claim without inspecting it. It also
does not test Stirrup constructor rollback or cleanup after harness failure.

Evidence: `lifecycle/summary.json`, `lifecycle/events.json`, generated manifests
and their SHA256 files. All five control/lifecycle Pods were absent in the final
namespace read; `controls/final-owned-pods.json` lists the names checked.

## V11: UID/fsGroup workspace access

Inside the offline guest, `id` reported UID 65532, primary GID 0 and supplemental
groups 0 and 65532. `/workspace` was root-owned with group 65532 and writable;
files written by the probe had UID/GID 65532. Both the direct write and production
tar round trips passed. This validates the tested UID/fsGroup shape.

Do not describe the process as having primary GID 65532, or claim that the
workspace is a memory filesystem: its observed filesystem was XFS on `/dev/vdb2`.

Evidence: `offline/nitro-offline-guest.json` and `offline/go-core.txt`.

## V12: identity directory mount

`/run/stirrup/sandbox-identity` was a separate `tmpfs` mount with `size=64k`,
matching the manifest's memory emptyDir. The production
`WriteSandboxIdentityToken` accepted a dummy token, and the test explicitly
asserted that this mount's filesystem type was `tmpfs`.

This resolves the memory-medium mapping question for this tested runtime/image.
It does not test real token exchange, expiry/rotation, credential-helper behavior,
revocation, or an identity-enabled candidate constructor. The direct inspection
used dummy data; no provider or control-plane credential was injected.
Retain the plan's expiry-handling and identity admission gates before enabling
real sandboxIdentity configuration.

Evidence: `offline/nitro-offline-guest.json`,
`offline/go-core.txt` (`identity_tmpfs` subtest), and Pod volume declarations.

## Recommended plan amendments

These are proposed documentation/design changes, not production implementation:

1. Add the measured exec-output throttle to G7 and the deployment recipe.
   `log-bandwidth` affects Stirrup's only I/O channel even with networking
   disabled. Make an explicit operator-controlled throttle decision or document
   timeout consequences; do not silently disable resource protection. Test the
   chosen setting and its admission behavior.
2. Keep the original 30-second output-cap failure visible. Distinguish output
   retention bounds from time to drain a stream and error precedence. Add a
   supported-configuration test with a measured timeout, rather than blanket
   timeout increases or reporting the failed test as passed.
3. Retain explicit positive CPU/memory requirements. Record 500m acceptance and
   one visible guest CPU without promising a universal rounding or scheduling
   rule. Treat guest MemTotal separately from container limit enforcement.
4. Reject explicit Nitro `diskMb`: the measured ephemeral-storage declaration
   did not prevent a 2x overrun. Retain bounded evidence instead of saying disk
   is unlimited.
5. Update the identity mount question from unverified to measured tmpfs for
   this recipe. Keep real identity lifecycle and candidate checks open.
6. Use the measured runtime identity only for `m5d` in the initial family list.
   Preserve the non-attestation caveat and the ordinary-Pod negative control.
7. Base readiness handling on an overall context and useful Pod conditions.
   The 113.58-second initial wait included setup repair; the clean cold-node
   sample was 43.836 seconds. Neither defines a new universal ceiling or
   justifies warm pools without repeated measurements.
8. Retain startup time in Pod lifetime accounting. Separate API deletion/GC
   from guest-stop evidence. Keep the later heartbeat finding distinct until
   it has been reviewed.

The experiment supports the k8s-executor direction and exposes concrete recipe
and timeout issues. It does not authorize declaring the Nitro release gates
complete: candidate validation/admission/rollback and full integration acceptance
remain implementation work, while networked release support remains a separate
design gate.
