# EKS Nitro Sandboxes live spike — 2026-11-11

Measurements and verified teardown complete. **The offline executor direction is supported by this spike. Nitro is not ready for release:** no implementation candidate was built, and admission, rollback, configuration rejection and identity-expiry gates remain untested.

The main findings are operationally significant:

- The no-network annotation removed guest IP networking while exec, binary tar I/O, timeouts and process-tree termination worked.
- Default network policies failed open. With enforcement enabled and `DefaultAllow`, three startup internet requests escaped a preinstalled policy. `DefaultDeny` blocked all ten sampled startup requests, but IMDS still responded under deny-all.
- A 64 MiB ephemeral-storage limit allowed a 128 MiB write without eviction during the observation period. Do not advertise it as a hard workspace quota.
- The default log throttle also limits exec output. An 11 MiB output-cap probe took about 44 seconds at the default and 4.48 seconds with `log-bandwidth: 20M`.
- Heartbeat arrivals continued for 1.94 seconds after a successful zero-grace DeletePod response. A delete response is not a guest-stop acknowledgement.

## Scope and provenance

The owner authorized sandbox account `786874932855`, a USD 25 soft budget and USD 40 hard ceiling, creation of supporting infrastructure, and teardown. Region: `us-east-1`. Teardown owner: Codex root agent. No permanent environment was requested or created.

| Item | Tested value |
|---|---|
| Stirrup code | `cc8d529de659eaa65628464ce910c4d5c7599f16` |
| EKS | Auto Mode, Kubernetes 1.36, platform `eks.17`, IPv4 |
| Nodes | Two sequentially provisioned `m5d.xlarge`, amd64, on-demand; at most two active |
| AMI | `ami-09ef60d209279212e`, `eks-auto-standard-1.36-x86_64-20261031` |
| Node software | Bottlerocket 2026.10.31, kubelet `v1.36.4-eks-cfb47f5`, kernel 6.18.56, containerd `2.2.9+bottlerocket` |
| Guest kernel | 6.18.54 |
| Runtime | `isolated-sandbox`; DMI `m5d.enclave`, virtio block devices |
| Core image | BusyBox 1.37.0-glibc, `public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4` |
| Workload image | Go 1.27.3/bookworm, `public.ecr.aws/docker/library/golang@sha256:025eefc1e8dbe63201734a72aaa0a55d7a077f2d8410432f8ae2f3e939f32871` |
| Default test shape | 1 CPU/1 GiB, UID 65532, primary GID 0, supplemental/fsGroup 65532, dropped capabilities, no privilege escalation, RuntimeDefault seccomp, no service-account token |
| Infrastructure | Dedicated VPC, two public subnets, no NAT gateway, no public LB, no ECR repository; Terraform plus CloudFormation |

The Go research overlay calls current production `podExecCore` against hand-created Pods. It bypasses executor construction and RunConfig validation. No production code was changed. The full existing integration suite, admission mutation/rollback tests, full harness run, IPv6, Graviton, other instance families, confidential mode, and real identity credentials/expiry were not tested.

## V1–V13 ledger

“Confirmed” below describes a bounded research observation, not candidate release acceptance.

| Gate | Research outcome | Evidence and limit |
|---|---|---|
| V1 offline core | Core cases confirmed; full release gate open | stdout/stderr/exit status, ASCII/UTF-8/NUL/nested tar, Exec/ExecStream process-tree timeout tests passed. Original output-cap test failed at 30 s; same producer returned the expected10 MiB cap error at ~44 s with90 s budget. No candidate allowlist-rejection test. |
| V2 transport | Confirmed | Normal GET/HTTP101 WebSocket `v5.channel.k8s.io`; explicit POST/HTTP101 SPDY/3.1 also worked. Forced SPDY is not an observed automatic fallback. |
| V3 startup | Measured samples | Clean empty-pool creation to Ready43.836 s; warm BusyBox samples~6–8 s. Go image warm-node pull/start32.55 s. EKS itself took~13 min. First Pod's114s wait included the IAM/tag setup failure and is not a clean cold-start measure. No percentile or worst-case ceiling inferred. |
| V4 sizing | Confirmed | No requests:1 vCPU,132460 KiB MemTotal. 1 CPU/1 Gi:1 vCPU,1161772 KiB. 500m/1Gi:1 vCPU,1161768 KiB. Guest MemTotal is not the container's cgroup limit. |
| V5 offline network | Confirmed for tested targets | Only `lo`; internet literals, DNS and both metadata IPs failed while exec/tar worked. Networked positive controls reached the same services. Pod metadata still had a PodIP: do not use its presence as a guest-network check. |
| V6 networked confinement | Unsafe defaults; release gate open | Controller absent: deny-all and allowlike policies inert. Enabled+DefaultAllow:3 startup escapes, later blocked. DefaultDeny:0/10 sampled startup escapes with explicit allow-all positive control. IMDS returned401 under deny-all in both profiles; this is reachability, not credential disclosure. |
| V7 runtime diagnostic | Confirmed on m5d only | Sandbox `m5d.enclave`; ordinary control `m5d.xlarge` with NVMe rather than virtio. Diagnostic is not attestation against a malicious image. |
| V8 disk quota | Hard-limit claim disproved for tested setting | 128 MiB write succeeded against64 MiB limit; Pod remained Running at 90-second follow-up. Workspace filesystem reported394220 KiB. No unlimited-storage claim or general sizing formula inferred. Reject explicit Nitro diskMb until a truthful enforcement contract exists. |
| V9 representative work | Confirmed with fixture correction | Clone1.224 s; full harness dependency download10.581 s,321056 KiB cache, with a fresh Pod declaring 4 GiB ephemeral storage. Default throttles; actual proxy logs retained. First attempt failed because the ordinary proxy lacked working outbound access; a proxy-only explicit egress policy restored it. |
| V10 lifecycle | API behavior and post-delete traffic measured | activeDeadline120 s terminated a Ready container after ~121 s; owner deletion removed child API object in4.275 s. Last heartbeat arrived1.936 s after delete response; receiver remained healthy. Exact CPU halt and simultaneous API NotFound were not measured. |
| V11 filesystem user | Confirmed | UID 65532/fsGroup 65532 could write workspace and identity directory. Primary GID was0. |
| V12 memory volume | Mount/write confirmed; identity acceptance open | Memory emptyDir mounted as tmpfs,64 KiB limit; guest SwapTotal0. Production dummy-token writer passed. No real token exchange, refresh or expiry tested. |
| V13 region | Confirmed for us-east-1 | Both RuntimeClasses present on this cluster. No regional survey. Current AWS overview now says all commercial regions. |

Detailed [core results](core-results.md), [network and heartbeat results](network/RESULTS.md), and [raw evidence](evidence/) preserve failures as well as successes. The network report retains its initial-stage text followed by clearly labelled follow-ups; this ledger reflects the final scope.

## Corrections the implementation plan needs

1. Keep the initial release offline-only, explicitly sized, IPv4/amd64 and pinned to tested `m5d.xlarge` plus image digest. The annotation is essential; an accepted NetworkPolicy is insufficient.
2. Keep PR1 readiness/admission/cleanup and PR4 read-only preflight ahead of Nitro enablement. The clean cold sample fits60 s, but it does not justify a60 s universal ceiling; large-image startup and node readiness are separate inputs.
3. Add `sandbox.amazonaws.com/log-bandwidth` to the sizing/performance discussion. The tested default2M affects exec stdout too. Resolve timeout/annotation policy explicitly; do not silently change the original failing30 s probe into a pass. Retained output stays bounded even when timeout takes precedence over the cap error.
4. Reject explicit Nitro `diskMb` for the initial recipe. V8 did not establish requested-byte enforcement.
5. Memory emptyDir works in the tested recipe. Keep identity enablement conditional on its remaining token lifecycle tests; the mount result alone is not complete acceptance.
6. Keep policies through confirmed termination. Post-delete network observations strengthen G14; API deletion, response success and guest halt are different observations.
7. For networked follow-up, require controller enablement and DefaultDeny startup tests, explicit healthy controls, and an IMDS/credential-path contract. HTTP 401 does not settle IMDSv2 or credential issuance; neither was requested in this spike.
8. The live RuntimeClass had a scheduling node selector and memory/extended-resource overhead, but no toleration. Do not invent a required taint/toleration from documentation examples.

## Setup failures and fixture limits

Optional NodeClass `Project`/`Expires` tags caused `CreateLaunchTemplateAuthCheckFailed`. The managed `AmazonEKSComputePolicy` v7 permits only its prescribed tag keys at creation. Removing the optional tags resolved it; IAM was not broadened. Mandatory AWS ownership tags and explicit resource IDs were retained for cleanup. See [AWS contract notes](aws-contract.md).

The proxy's first clone returned a TLS-handshake failure; proxy logs identified `upstream_dial_failed`. DNS worked, but direct HTTP/HTTPS from the ordinary proxy timed out. Applying an explicit egress-allow policy selecting only the trusted proxy restored its connectivity; the sandbox's restrictive policy remained. The exact reason this ordinary Pod needed that policy despite its NodeClass reporting DefaultAllow was not isolated. This is a fixture finding, not a claim that Git/TLS or Nitro was broken.

V9 used the public default branch at `eeaad430cc7674a8bf59a045489f25226c30ac04`, recorded go.mod/go.sum hashes, Go 1.27.3 and Git 2.39.5. Fresh clone/cache directories were used. No downloaded code was built or executed. This single fetch experiment is not a workload-performance guarantee or proof that a malicious workload cannot bypass the proxy. Proxy logs show allowed CONNECTs to GitHub, proxy.golang.org and sum.golang.org.

The heartbeat receiver's BusyBox timeout left an nc child; the agent terminated that exact listener and checked its port before deleting the proxy Pod. The retained script is the executed instrument, with its cleanup limitation documented. Do not reuse it unattended without the cleanup hardening described in the network report.

## Costs, teardown and handoff

The planned envelope was USD 5 within the authorized USD 25/USD 40 limits. Live pricing and SSO facts are in [auth-budget.md](auth-budget.md); the later [cost review](evidence/cost-review.md) uses observed node and EBS inventory. The final conservative modeled base cost is **USD 0.26**; charging a whole hour for each recorded node and the control plane would be about USD 0.62. These are estimates, not a settled AWS bill. Transfer and tax were not metered. See [final cost calculation](evidence/cleanup-cost-final.json). No dedicated resources remain for the owner to tear down.

CloudFormation stack/cluster: `equestrian-nitro-20261111`. VPC: `vpc-028a9b46aba88edba`. A Scheduler installed before the stack was set to start deletion at 00:45 UTC using an independent CloudFormation service role. It was removed after manual teardown succeeded. Manual teardown began around 23:19 UTC, well before that deadline. Final audit at 23:25:29 UTC confirmed the stack and exact VPC were deleted, both known instances terminated, all four known volumes and associated ENIs/templates absent, and dedicated roles, generated instance profiles and schedule removed. Terraform state is empty. See [cleanup review](cleanup.md) and [final inventory](evidence/inventory-final.json).

Auto Mode managed resources are hidden from this installed AWS CLI's default EC2 listings. The initial empty inventory was corrected to incomplete. Cleanup must query the two known instance IDs, four EBS IDs and associated ENI/template IDs, then verify the exact VPC, cluster, stack, generated instance profiles, dedicated roles and schedule. The ID manifest is [known-managed-resources.json](evidence/known-managed-resources.json); the read-only audit is [cleanup-audit.sh](evidence/cleanup-audit.sh).

Reusable Terraform source is in `~/Developer/equestrian-infra-lab`. Names, account and cleanup timestamp are deliberately frozen to this run; its README requires updating them before reuse. No remote repository was published. State, kubeconfig, credentials and compiled binaries are not committed.

## Reproducing or extending the evidence

The saved [probe sources](probes/README.md), [network runner](network/README.md) and [V9 fixture](v9/README.md) are research instruments. Their `/tmp/nitro-*` paths refer to this run; recreate those staging directories or adapt paths before reuse. `evidence/` mirrors `/tmp/nitro-evidence`, `network/` mirrors `/tmp/nitro-network`, and `v9/` excludes compiled binaries. Build the proxy from the recorded Stirrup revision; verify its recorded SHA256 before injection. The Go overlay source is retained, including the original failing revision, but its absolute-path overlay JSON must be regenerated for the current checkout.

Further paid runs require fresh authorization and cleanup deadlines. Preserve exact images, manifests, NodeClass policy, instance family, versions and actual failure outputs. Candidate admission/rollback/validation, identity expiry, node loss, repeat startup-race tests and exact guest-stop semantics remain future acceptance work.


## Alexandria handoff

The measured findings were saved to the `equestrianism` space as self-contained,
version-bound facts: [offline/sizing/identity mount](kb://fragment/01a1ccef-38ae-77a7-8f66-f7c0f85b686c),
[network policy](kb://fragment/01a1ccef-3d26-74c9-95ca-20489b669a00),
[exec log throttle](kb://fragment/01a1ccef-3ef3-7973-9e56-46f1d810fdec),
[disk quota](kb://fragment/01a1ccef-408a-79b7-9b1b-8d5c6d58927f), and
[teardown semantics](kb://fragment/01a1ccef-4201-7ca9-8db9-e64b849b4ad4).
The [lab cleanup procedure](kb://fragment/01a1ccf1-832d-70dd-8a11-ab7d51822cd9) records the separate cost and resource-audit lessons.
They retain the research-versus-release boundary and point back to this evidence.


## Verification of the handoff

The probes were compiled and executed as recorded above; the original failing
30-second output test remains in the evidence. Terraform formatting/validation
passed, and the expired-deadline expression evaluates false for a time after its
cutoff. Shell syntax and local Markdown links were checked. Two independent
reviews checked result claims and cleanup. No full `just check` was run: this
change contains research documentation/instruments, no production implementation,
and nothing was pushed. `SHA256SUMS` fingerprints this saved report bundle.
