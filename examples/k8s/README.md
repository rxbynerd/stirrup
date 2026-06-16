# Kubernetes reference manifests

A working operator starting point for running the stirrup k8s executor, and an
inline record of what the executor expects from the cluster. These are reference
manifests: apply the standing objects, adapt the namespace and image to the
cluster, and let the executor create sandbox Pods at runtime.

The executor itself is documented in
[`harness/internal/executor/k8s.go`](../../harness/internal/executor/k8s.go);
every field shown here is cross-checked against it.

## Files

| File | Kind | Apply? | Purpose |
|---|---|---|---|
| `namespace.yaml` | Namespace | yes | The `stirrup-sandbox` namespace with sandbox/monitoring labels and `restricted` PodSecurity. |
| `rbac.yaml` | ServiceAccount + Role + RoleBinding | yes | The `stirrup-orchestrator` identity and the pod / pods/exec / networkpolicy verbs the executor's lifecycle needs. |
| `runtimeclass.yaml` | RuntimeClass ×5 | yes (per available runtime) | The closed set `runc / gvisor / kata-qemu / kata-fc / kata-clh`, with per-class node prerequisites. |
| `taint-and-toleration.yaml` | (doc only) | no | The pattern for isolating sandbox nodes — and the note that the executor does not yet inject tolerations. |
| `sample-sandbox-pod.yaml` | (doc only) | no | The exact Pod the executor produces, annotated field-by-field. The reference for "what a real sandbox Pod looks like". |
| `egress-proxy/` | Deployment + Service + ConfigMap + NetworkPolicy | yes (allowlist mode) | The shared egress allowlist proxy. See its own [README](egress-proxy/README.md). |
| `agent-sandbox/` | RBAC (apply) + Sandbox (doc only) | partial | Manifests for the `k8s-sandbox` executor (Agent Sandbox CRD): an orchestrator Role and a reference Sandbox CR. See [`docs/executors/k8s-agent-sandbox.md`](../../docs/executors/k8s-agent-sandbox.md). |

`sample-sandbox-pod.yaml` and `taint-and-toleration.yaml` are documentation, not
appliable objects: the executor creates Pods at runtime, so a static Pod manifest
would not be one the executor manages. The same holds for
`agent-sandbox/sandbox.yaml` (the executor builds the Sandbox CR in Go);
`agent-sandbox/rbac-agent-sandbox.yaml` is appliable.

## Applying the standing objects

```sh
kubectl apply -f examples/k8s/namespace.yaml
kubectl apply -f examples/k8s/rbac.yaml
kubectl apply -f examples/k8s/runtimeclass.yaml   # registered handlers only
```

Register only the RuntimeClasses whose handlers are actually installed on the
nodes — a RuntimeClass whose handler is missing makes Pod scheduling fail. The
egress-proxy objects are applied separately and only when running in network
mode `allowlist`; see [`egress-proxy/README.md`](egress-proxy/README.md).

### Namespace alignment — required before applying the egress proxy

The files in this directory place the sandbox in the `stirrup-sandbox`
namespace, but the `egress-proxy/` manifests ship with `namespace: default`
(they are written to stand alone). **These two halves MUST share one namespace.**
The allowlist NetworkPolicy the executor installs selects the proxy by a
`PodSelector` with no `NamespaceSelector`, so a proxy in a different namespace is
not matched: under an enforcing CNI the sandbox then gets *no* egress at all (a
silent deny, not a bypass).

Before applying `egress-proxy/` for use with this sandbox, edit the
`namespace:` field in each of its four manifests (deployment, service,
network-policy, configmap) from `default` to `stirrup-sandbox`, then apply and
point `--k8s-egress-proxy-url` at the matching name:

```sh
# After editing the egress-proxy manifests' namespace to stirrup-sandbox:
kubectl apply -f examples/k8s/egress-proxy/configmap.yaml
kubectl apply -f examples/k8s/egress-proxy/deployment.yaml
kubectl apply -f examples/k8s/egress-proxy/service.yaml
kubectl apply -f examples/k8s/egress-proxy/network-policy.yaml

# …and point the executor at the proxy in that namespace:
#   --k8s-egress-proxy-url http://stirrup-egress-proxy.stirrup-sandbox.svc:8080
```

The `namespace:` fields in `egress-proxy/` are set explicitly to `default`, so a
`kubectl apply -n stirrup-sandbox` override does NOT work — kubectl rejects an
`-n` that disagrees with the object's own `metadata.namespace`. Edit the field
in the file (or delete it and supply `-n` on apply). Either way, the proxy and
the sandbox Pods must end up in the same namespace.
`sample-sandbox-pod.yaml` already shows the proxy URL in the `stirrup-sandbox`
namespace to match this workflow.

## How the pieces fit together

The executor authenticates as `stirrup-orchestrator` (`rbac.yaml`), creates a
sandbox Pod (`sample-sandbox-pod.yaml`) in the `stirrup-sandbox` namespace
(`namespace.yaml`) with the RuntimeClass named by `executor.runtime`
(`runtimeclass.yaml`), and installs a per-Pod egress NetworkPolicy before the
Pod starts. In network mode `allowlist`, that policy confines the Pod to DNS and
the egress proxy (`egress-proxy/`), and the executor injects `HTTP_PROXY` /
`HTTPS_PROXY` pointing at the proxy.

The label contract knits these together: sandbox Pods carry
`stirrup-sandbox=true` and `stirrup.dev/pod=<name>`, and the egress policy
selects the proxy by `app=stirrup-egress-proxy` on TCP 8080. These labels are
fixed in the executor (`k8s_netpol.go`); the manifests here and under
`egress-proxy/` must keep them in sync.

## Enforcement caveat

NetworkPolicy is only enforced by a CNI that implements it. kindnet — the
default CNI for `kind` — accepts NetworkPolicy objects but does not enforce
them, so on a stock kind cluster the sandbox retains cluster-default egress. A
NetworkPolicy-enforcing CNI (Cilium, Calico) is required for the confinement to
hold. See [`egress-proxy/README.md`](egress-proxy/README.md) for the full note.

## Out of scope

Automated application (a Helm chart is tracked separately), HA for the egress
proxy, the CNI installation itself, and node-level RuntimeClass setup. This
directory is the minimal, readable starting point those build on.
