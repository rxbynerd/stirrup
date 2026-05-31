# Kubernetes egress proxy

Manifests for running the stirrup egress allowlist proxy as a Deployment
alongside a k8s-executor sandbox Pod. The proxy gates every outbound
destination against an FQDN allowlist; the sandbox Pod routes its traffic
through the proxy via `HTTP_PROXY` / `HTTPS_PROXY`, and a NetworkPolicy
confines the Pod so the proxy is its only route off-cluster.

This is the Kubernetes counterpart to the in-process proxy the container
executor starts on the host network. A sandbox Pod cannot start its own
host-side proxy, so the proxy runs as a separate, long-lived Deployment that
many sandbox Pods share.

## Files

| File | Purpose |
|---|---|
| `configmap.yaml` | The FQDN allowlist (one entry per line). |
| `deployment.yaml` | The proxy Deployment running `stirrup egress-proxy`. |
| `service.yaml` | A ClusterIP Service exposing the proxy at a stable DNS name. |
| `network-policy.yaml` | A namespace-wide egress baseline for sandbox Pods. |

## Enforcement caveat — read this first

NetworkPolicy is only enforced by a CNI that implements it. **kindnet — the
default CNI for `kind` clusters — accepts NetworkPolicy objects but does not
enforce them.** On a stock kind cluster the deny/allowlist policies (both the
one in `network-policy.yaml` and the per-Pod policy the executor installs at
runtime) are inert, and a sandbox Pod retains cluster-default egress. The
allowlist is genuinely enforced only on a NetworkPolicy-enforcing CNI such as
[Cilium](https://cilium.io/) or [Calico](https://www.tigera.io/project-calico/).

Treat a kind-based smoke test as proof of manifest shape — the objects are
created with the right selectors, ports, and env — not as proof that egress is
actually confined. This mirrors the honest fail-open note the container
executor carries around `host.docker.internal`.

## Applying

```sh
kubectl apply -f examples/k8s/egress-proxy/configmap.yaml
kubectl apply -f examples/k8s/egress-proxy/deployment.yaml
kubectl apply -f examples/k8s/egress-proxy/service.yaml
kubectl apply -f examples/k8s/egress-proxy/network-policy.yaml
```

Edit `configmap.yaml` to list the destinations a run is permitted to reach,
then roll the Deployment so the new allowlist takes effect:

```sh
kubectl rollout restart deployment/stirrup-egress-proxy
```

The proxy reads its allowlist once at startup; there is no hot reload. A
restart is the supported way to change the allowlist.

## Pointing a sandbox run at the proxy

Configure the k8s executor with the proxy's in-cluster URL and an allowlist
network mode. The executor injects `HTTP_PROXY` / `HTTPS_PROXY` into the
sandbox container and installs a per-Pod NetworkPolicy confining egress to the
proxy (plus DNS).

The proxy Deployment must run in the **same namespace** as the sandbox Pod.
The egress NetworkPolicy selects the proxy by a `PodSelector` with no
`NamespaceSelector`, so it matches only proxy Pods in the sandbox's own
namespace. A proxy in a different namespace is denied (more restrictive, not a
bypass) — a confusing misconfiguration that leaves the sandbox unable to reach
the network at all under an enforcing CNI. Set `--k8s-namespace` and the
proxy's namespace to the same value, and point `--k8s-egress-proxy-url` at the
`<service>.<namespace>.svc` name.

From the CLI:

```sh
stirrup harness \
  --executor k8s \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace default \
  --k8s-egress-proxy-url http://stirrup-egress-proxy.default.svc:8080 \
  ...
```

The network mode comes from a RunConfig (`executor.network.mode: "allowlist"`)
— `--k8s-egress-proxy-url` is required whenever that mode is set and rejected
otherwise. From a RunConfig file:

```json
{
  "executor": {
    "type": "k8s",
    "image": "ghcr.io/rxbynerd/stirrup-sandbox:latest",
    "k8sNamespace": "default",
    "k8sEgressProxyUrl": "http://stirrup-egress-proxy.default.svc:8080",
    "network": {
      "mode": "allowlist",
      "allowlist": ["api.anthropic.com", "github.com"]
    }
  }
}
```

The Pod-side `network.allowlist` and the proxy ConfigMap allowlist are
independent surfaces: the proxy enforces the FQDN allowlist for every Pod that
routes through it, so the ConfigMap is the operator-controlled source of truth.
Keep the two consistent.

## Allowlist syntax

Entries match the executor allowlist:

```
example.com           exact match on example.com:443
*.example.com         any subdomain of example.com on :443 (not example.com itself)
example.com:80        exact match on example.com:80
```

Blank lines and lines starting with `#` are ignored. An empty allowlist denies
every destination (fail closed).

## Running the proxy standalone

The `stirrup egress-proxy` subcommand runs the proxy in the foreground until it
receives SIGINT or SIGTERM. It is useful for local testing without a cluster:

```sh
stirrup egress-proxy --listen :8080 --allowlist api.anthropic.com --allowlist github.com
# or read the allowlist from a file:
stirrup egress-proxy --listen :8080 --allowlist-file ./allowlist.txt
```

`egress_allowed` / `egress_blocked` audit events are written to stderr as JSON
lines, so a Deployment's Pod logs carry every gating decision.

## Scope

These manifests are a minimal, single-replica example. A production deployment
should run `replicas >= 2` behind a PodDisruptionBudget so a node drain or
rollout does not sever every sandbox's only egress path at once — see the note
in `deployment.yaml`. HA, the CNI installation, and DNS-level egress filtering
are out of scope here.
