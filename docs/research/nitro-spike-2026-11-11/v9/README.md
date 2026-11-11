# V9 preparation

No cluster mutations performed during preparation. The proxy binary is the
current checkout's static linux/amd64 build. `golang-images.json` records public
ECR manifest/config metadata: 1.27.3-bookworm is pinned and its build history
installs git. The image sets GOTOOLCHAIN=local and Go 1.27.3 on PATH. Image content
and commands still require live verification; image history alone is not that.

Requested Go 1.26 image is also recorded but unsuitable for the current types
module, whose go directive is 1.27.3. The selected image downloads about 300.6 MB
compressed. Separate image/node startup timing from clone/dependency timing.

`prepare.py --namespace nitro-spike` generates restricted proxy/workload Pod,
ClusterIP Service, allowlist ConfigMap and a workload-scoped NetworkPolicy.
It also produces a gzipped tar of the static binary. Both Pods have 1800s active
deadlines; this does not clean up instances. Proxy is an ordinary Pod, no runtime
class, with 50m CPU/64Mi request and 500m/256Mi limit. Workload is isolated-sandbox
with 1 CPU/1Gi memory and a 1Gi ephemeral-storage request/limit (not an enforcement
claim). No public load balancer or ingress is created. Add a precise toleration
to the proxy manifest only if the tested node carries a sandbox taint.

Apply allowlist, service, proxy and network-policy JSON with the dedicated
kubeconfig. Wait for proxy Pod phase Running, not Ready: it waits for the binary
and its readiness probe cannot pass until that arrives. Inject:

```sh
kubectl -n nitro-spike exec -i nitro-v9-proxy -c proxy -- \
  tar xzf - -C /workspace < /tmp/nitro-v9/proxy-binary.tar.gz
kubectl -n nitro-spike exec nitro-v9-proxy -c proxy -- \
  sha256sum /workspace/stirrup
# Compare with binary-sha256.txt before starting.
kubectl -n nitro-spike exec nitro-v9-proxy -c proxy -- touch /workspace/start
kubectl -n nitro-spike wait --for=condition=Ready pod/nitro-v9-proxy --timeout=50s
```

Apply workload.json and record image pull/readiness separately. Verify commands
with `go version` and `git --version`. Run workload.sh via `kubectl exec -i ... --
/bin/sh -s` and capture stdout/stderr and exit status. Each clone/download has a
180-second in-guest timeout; run the outer exec asynchronously and poll tools at
most 60 seconds. The script clones the public default branch at depth 1, records
the actual commit and module file hashes, then downloads dependencies for the
types module with GOWORK=off and fresh caches. It neither builds nor executes
downloaded code. Proxy allowlist permits GitHub, GitHub content, proxy.golang.org,
sum.golang.org and storage.googleapis.com for module archive redirects. Both
uppercase/lowercase proxy env variables are set and NO_PROXY is empty.

Capture proxy logs to prove requests traversed it. Confinement is a separate V6
claim: an accepted NetworkPolicy is not evidence of enforcement. Default sandbox
network/disk throttles are unchanged; keep exact resource/version evidence.

Cleanup: delete the two named Pods, then Service/stirrup-egress-proxy,
ConfigMap/nitro-v9-allowlist and NetworkPolicy/nitro-v9-allowlist. Delete only
objects created for this run; do not overwrite a preexisting service of that name.


## Final measured workload

The headline 1.224-second clone and 10.581-second dependency download came from
[workload-large.sh](../evidence/v9/workload-large.sh), using the fresh
[nitro-v9-large Pod](../evidence/v9/workload-large.json) with a 4 GiB
`ephemeral-storage` request/limit. That declaration is not a hard-quota claim.
The original script above measures only the smaller types module. The final
script measures the harness module, uses fresh caches and millisecond timing.
See `../evidence/v9/large-result.json` and `large-stdout.txt`.

The initial proxy could resolve DNS but failed outbound connections. A
[proxy-only egress allow policy](../evidence/v9/proxy-egress.json) restored them;
the sandbox policy stayed restrictive. Its underlying DefaultAllow inconsistency
was not isolated. Apply that proxy policy when reproducing the final fixture.
Cleanup must also delete Pod/nitro-v9-large and NetworkPolicy/nitro-v9-proxy-egress,
in addition to the original objects listed above. The completed run deletes the
entire dedicated namespace and infrastructure, not only these named objects.
