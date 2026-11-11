# V6 network probe results — 2026-11-11

The default Auto Mode network setup allowed direct internet, IMDS endpoint, and Pod Identity endpoint access despite both deny-all and Stirrup-like allowlist NetworkPolicies. Enabling the policy controller constrained steady-state internet and Pod Identity access, but did not prevent IMDS endpoint access. A recreated Pod with policy already installed made three successful internet requests before enforcement took effect.

These findings support keeping the first Nitro executor release offline-only with the network-isolated annotation. They do not establish a working networked allowlist contract.

## Tested environment and scope

Cluster `equestrian-nitro-20261111`, region us-east-1, EKS 1.36, IPv4, node `i-0a0a2ea59026805e3`, exact family m5d.xlarge selected by the parent. NodeClass networkPolicy=DefaultAllow throughout. RuntimeClass isolated-sandbox. No network-isolated annotation. One container, UID65532, no capabilities, no service account token, requests=limits 1 CPU and1Gi. Image `public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4` (amd64 image manifest).

This agent created only Pod `nitro-spike/nitro-network-probe` and same-name NetworkPolicy, sequentially, with activeDeadlineSeconds1200 and terminationGracePeriodSeconds0. The parent alone enabled the controller. No proxy or Pod Identity association was created. HTTP bodies went to /dev/null; no credential path, token request, or authorization header was used.

## Paired controls

| Controller | Pod policy | Literal internet / DNS hostname | IMDS placement/region | Pod Identity `/` |
|---|---|---|---|---|
| Off; ConfigMap absent | None | HTTP200 /200 | HTTP401 | HTTP404 |
| Off | Deny-all, existing Pod | HTTP200 /200 | HTTP401 | HTTP404 |
| Off | Deny-all installed before recreated Pod | HTTP200 /200 | HTTP401 | HTTP404 |
| Off | Stirrup-like DNS+proxy policy | HTTP200 /200 | HTTP401 | HTTP404 |
| On | Same DNS+proxy policy, existing Pod | No HTTP status; bounded failures | HTTP401 | No HTTP status; bounded failure |
| On | DNS+proxy policy before recreated Pod, after reconciliation | No HTTP status; bounded failures | HTTP401 | No HTTP status; bounded failure |
| On | Deny-all | No HTTP status; bounded failures | HTTP401 | No HTTP status; bounded failure |
| On | Policy removed | HTTP200 /200 | HTTP401 | HTTP404 |
| On | DNS+proxy policy restored | No HTTP status; bounded failures | HTTP401 | No HTTP status; bounded failure |

Literal internet target was http://1.1.1.1/ (301 redirects then200), hostname target http://example.com/ (200). IMDS target was http://169.254.169.254/latest/meta-data/placement/region; Pod Identity target was http://169.254.170.23/. HTTP401/404 prove endpoint reachability, not credential access. No response bodies were saved. Requests were bounded by BusyBox timeout4 and wget timeout2. Failures generally returned in2seconds; DNS under deny-all hit the4second process timeout. The recorded runner suppresses raw wget errors, so the table deliberately states bounded failure rather than a specific network error.

The DNS+proxy policy reproduces current Stirrup's rules: TCP/UDP53 to any peer, plus TCP8080 to same-namespace app=stirrup-egress-proxy. No such proxy peer existed, so the admitted PolicyEndpoint only contained DNS rules. This is a direct-path confinement test, not an end-to-end proxy acceptance test. The resolved PolicyEndpoint selected the intended Pod and declared Egress isolation.

## Startup escape

The controller was already enabled and a new DNS+proxy NetworkPolicy was applied at23:03:18UTC. Pod creation followed at23:03:19. Its initial command probed the literal internet target ten times with200ms between attempts. The first3 attempts reached HTTP200; attempts4–10 did not receive an HTTP status. This is direct evidence of an initial fail-open interval under DefaultAllow even when policy precedes Pod creation.

Guest-reported seconds for successes were23:03:25–26UTC. The first failed attempt began23:03:26 and returned23:03:28. Pod Ready and container startedAt are reported23:03:26. These timestamps have one-second resolution and come from different observation paths; do not claim a precise interval length or ordering within that second. The test proves an escape occurred, not its exact duration.

Primary evidence:

- `results/on-deny-startup-logs.json`: complete10-attempt startup log.
- `results/on-allowlike-preinstalled-policy.json`: preinstalled policy.
- `results/on-allowlike-preinstalled-create.json`: creation call and timing.
- `results/on-preinstalled-pod-timing.json`: admitted UID, image ID, Pod and container timing.
- `results/on-preinstalled-admitted-snapshot-policyendpoints.networking.k8s.aws.json`: policy endpoint state.
- `results/network-summary.json`: all HTTP probe outcomes.

## Cleanup and limits

DeletePod requested23:04:37UTC. Pod deletion wait completed23:04:40; policy deleted23:04:41. Snapshot23:04:42 shows no owned Pod or NetworkPolicy. Parent's controller ConfigMap remains enabled, as authorized. `results/after-cleanup-*` holds verification.

V6 remains partial overall: there is no real proxy path test, no credential issuance test, no DefaultDeny NodeClass test, and no broad timing distribution. These observations must not be converted into a claim that networked execution is safe. IMDS is still reachable with deny-all; HTTP401 is not evidence of a credential leak. V9 was not attempted with BusyBox because the image has no git/package manager. Guest lifetime after deletion was not measured: API deletion is not proof the guest has stopped.

## DefaultDeny follow-up (same date,23:07–23:10UTC)

The parent created a separate `nitro-deny` NodeClass with networkPolicy=DefaultDeny and a tainted NodePool restricted to one m5d.xlarge (4CPU limit). The probe used a nodeSelector and matching toleration. The initial snapshot shows no NodeClaim or Node in that pool, so this was a fresh parent-node creation.

Node `i-00de16c7f9f410e84` in us-east-1a used AMI `ami-09ef60d209279212e`, name `eks-auto-standard-1.36-x86_64-20261031`; EnclaveOptions.Enabled=true, two physical cores/two threads each. Kubernetes reported Bottlerocket2026.10.31, kernel6.18.56, containerd2.2.9+bottlerocket, kubeletv1.36.4-eks-cfb47f5. Raw provenance is in `strict-ready-nodes.json`, `strict-ec2-instance.json`, and `strict-ec2-image.json`.

The Pod create call began about23:07:04.125UTC and returned23:07:05.467; Ready wait returned23:07:47.963. Combined local elapsed time was43.836seconds. Kubernetes creationTimestamp23:07:05 and Ready transition23:07:47 give42seconds at API timestamp resolution. The node launched23:07:09 and registered23:07:26. This is one clean cold-node observation, not a percentile or readiness ceiling.

With the allowlike policy installed before the Pod, all10 startup internet attempts failed (zero HTTP statuses). Steady-state internet and Pod Identity probes failed as expected. Deny-all also blocked those probes. IMDS still returnedHTTP401 under both policies.

Removing the policy under DefaultDeny did not restore connectivity. An explicit allow-all egress policy did: literal internet and example.com returned200, Pod Identity root returned404. Restoring allowlike blocked them again. Thus this run has a healthy same-node positive control. Evidence: `strict-deny-startup-logs.json`, `strict-control-probe.json`, `strict-explicit-control-probe.json`, `strict-post-control-probe.json`.

This single DefaultDeny sample did not reproduce the DefaultAllow startup leak. It does not prove an absent race across all launches. IMDS reachability remains, and no actual proxy-path or credential issuance assertion follows from these probes. The earlier statement that DefaultDeny was untested is superseded by this section; broader release validation remains incomplete.

## V10 external heartbeat follow-up

A bounded TCP receiver ran on the parent's existing ordinary `nitro-v9-proxy` Pod, port19090. A scoped policy allowed only the probe Pod's egress to app=stirrup-egress-proxy TCP19090. The sender emitted a unique run ID and sequence about every200ms. The local collector timestamped receiver output using one monotonic clock. At least5 heartbeats arrived before deletion was requested.

- DeletePod CLI request began23:10:47.791439UTC.
- Successful zero-grace, wait=false response returned23:10:48.481076UTC.
- Last observed heartbeat (sequence18) arrived23:10:50.416652UTC:2.625seconds after request began and1.936seconds after response.
- No further sender heartbeat arrived during the10second post-response observation. A receiver-local control packet arrived23:10:59.585320UTC, proving the collector/receiver path remained alive.
- Sender exec eventually reported exit137.

Raw event/call timing is `results/heartbeat.json`; commands are in `heartbeat.py`. This measures continued observable network activity after a deletion request/response. It does not prove the guest's exact CPU halt time, and transport buffering can affect receipt timing. It also does not establish the API object's disappearance time; no concurrent NotFound observation was taken. Keep policy until termination is confirmed rather than treating the delete response as instantaneous guest halt.

The receiver's outer BusyBox timeout did not reap its nc child. Cleanup inspected the exact PID/command, terminated only `nc -l -p19090`, and verified a loopback connection to19090 failed. The main proxy process on8080 was unchanged. Defunct children remained for the proxy Pod's PID1 to reap; no active listener remained. This instrumentation cleanup issue is separate from Nitro behavior. Before reusing heartbeat.py, replace the listener shell loop with an explicit TERM/INT trap that kills/waits for its nc child, and make the local listener timeout relative to its original start. The saved script is the actual executed version, preserving reproducibility.

The probe Pod and same-name policy were deleted and absent in `strict-after-cleanup` snapshots at23:12:30UTC. No probe-owned cloud resources remain. Parent retains ownership of the second NodePool/NodeClass and the existing proxy Pod teardown. The earlier statement that post-deletion activity was unmeasured is superseded by this bounded observation; a formal guest-stop proof remains unmeasured.
