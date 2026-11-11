# Bounded network research

Prepared only. Wait for parent to authorize execution and provide KUBECONFIG/namespace.

The script creates at most one owned Pod, `nitro-network-probe`, with 1 CPU/1Gi requests, a 20-minute active deadline, zero termination grace, no service-account token, and the pinned BusyBox amd64 image already inspected by the other probe agent. It creates no cloud infrastructure and does not change controller or NodeClass settings. Only the parent changes network policy enforcement.

HTTP response bodies always go to /dev/null. Saved results contain timestamps, return codes and numeric HTTP statuses only. IMDS probes only the non-sensitive placement/region path; Pod Identity probes `/` without authorization or credential paths. Any HTTP status proves endpoint reachability, including 401/403/404. A timeout does not prove isolation unless a matching healthy control succeeds. The network annotation is deliberately absent here; parent owns offline-annotation testing.

Suggested stages, sequential (use a distinct --stage label for every recorded action):

1. Snapshot controller state and policies. Policy remove. Create Pod. Probe the literal internet IP, DNS hostname, IMDS region and Pod Identity root. Preserve these positive/negative controls.
2. With controller disabled, install deny policy. Probe existing Pod. Then delete and recreate with policy installed before creation to capture 10 startup literal-IP attempts. Compare annotation-free behavior with parent's annotated offline Pod.
3. Change policy to `allowlike`, which reproduces current Stirrup's DNS and proxy selector rules. Probe with enforcement off. No real proxy is created by this script, so this only checks direct-path escapes and is not proxy acceptance.
4. Parent enables controller. Snapshot ConfigMap, PolicyEndpoints and policy. Probe same Pod after reconciliation. Recreate with installed policy to capture startup behavior. This can find an escape window; no success in ten probes does not prove no possible window.
5. Delete policy, repeat positive internet controls while controller is enabled, then restore policy and repeat. This distinguishes enforced denial from broken connectivity.
6. Delete Pod and policy; verify object absence. No global settings changed by this agent.

Example calls:

```
KUBECONFIG=/dedicated/path python3 /tmp/nitro-network/network.py create --stage off-control
KUBECONFIG=/dedicated/path python3 /tmp/nitro-network/network.py probe --stage off-control
KUBECONFIG=/dedicated/path python3 /tmp/nitro-network/network.py policy --policy allowlike --stage off-allowlike
```

A `delete` action removes the policy too. For a restart with a preinstalled policy, use `delete`, then `policy`, then `create` (never create duplicate concurrent probe Pods).

## V9

BusyBox has no git/package manager. Do not call an HTTP GET a representative clone/dependency install. V9 remains skipped unless a pinned suitable image and an actual egress proxy become available.

## Guest termination measurement

Pod disappearance and failure of subsequent exec do not establish when the guest stopped. Stronger practical evidence needs an independent reachable heartbeat receiver. To respect this agent's one-extra-Pod limit, use an existing parent-owned networked receiver Pod only if parent authorizes it. Sender is the same probe Pod.

Receiver runs a bounded TCP server (e.g. repeated BusyBox `nc -l -p 19090`, if that applet is available); its output streams through `kubectl exec` to the local Python process, which stamps monotonic receipt times. Sender emits random run ID + increasing sequence every 200ms over TCP with a one-second connect bound. Require at least five pre-deletion heartbeats, record DeletePod request and response times, continue receiving for ten seconds, and then send a separate control packet to verify the receiver remains alive. Report last observed heartbeat relative to deletion, transport buffering and sampling resolution; don't claim a formal guest-destruction proof. Both server and sender use bounded `timeout`, no Services or LBs.

Without an independent existing receiver, record guest post-deletion lifetime as unmeasured. Do not spend on an extra receiver or introduce a public listener just to claim this gate passed.
