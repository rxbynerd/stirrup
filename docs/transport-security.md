# Control-plane transport security

`stirrup job`, and `stirrup harness --transport grpc`, connect outbound to a
control plane and hold one `RunTask` gRPC stream for the whole session. By
default that stream is **plaintext and unauthenticated**: the harness dials
with insecure credentials (`harness/internal/transport/grpc.go`,
`NewGRPCTransport`) and accepts whatever answers at `CONTROL_PLANE_ADDR`.

This page covers two ways to protect it:

- [Harness TLS and mutual TLS](#harness-tls-and-mutual-tls): the harness
  speaks TLS 1.3 itself, configured with environment variables.
- [Service-mesh mutual TLS](#service-mesh-mutual-tls): an Istio sidecar
  encrypts and authenticates the connection, and the harness is unchanged.

A pod-local TLS-terminating proxy (Envoy, say) is not needed for this:
the harness's own TLS gives the same result without an extra container.

**A private address is not authentication.** A cluster-internal Service
name, a private VPC range or a NetworkPolicy limits who can reach the
control plane. It does not prove to the harness that the peer is the
control plane, it does not prove to the control plane that the peer is a
job it launched, and it does not encrypt anything. Treat the stream as
exposed to any workload that can reach the address or observe the path,
unless one of the recipes below is in place.

## What crosses the stream

Without TLS or a mesh, everything below is readable by anything on the
network path, and anything that can answer at the dial target can pose as
the control plane.

| Direction | Event | Contents |
|---|---|---|
| Control plane to harness | `task_assignment` | The full `RunConfig`: prompt, mode, tools, permission policy, provider endpoint and model, executor settings. |
| Control plane to harness | `sandbox_token_response` | `token`: the raw signed sandbox identity JWT, a bearer credential. |
| Control plane to harness | `permission_response`, `user_response`, `tool_result_response`, `batch_result`, `cancel` | Approvals, follow-up prompts, results of control-plane tools and batches, cancellation. |
| Harness to control plane | `text_delta`, `thinking_delta`, `user_message` | Model output, reasoning text and every user-role message, including the prompt. |
| Harness to control plane | `tool_call`, `tool_result`, `permission_request` | Tool inputs and outputs: file contents, command output, fetched pages. |
| Harness to control plane | `batch_submission`, `done`, `ready` and the rest | Batched requests, the run trace, the session ID and harness version. |

Two protections apply with or without TLS, and neither replaces it:

- Provider API keys and other `apiKeyRef` fields must be `secret://`
  references, which the harness resolves inside the job
  (`validateAPIKeyRefs` in `types/runconfig.go`,
  `harness/internal/security/secretstore.go`). Not every field is held to
  this: a `traceEmitter.headers` value may be a literal, and then crosses
  the stream as written.
- The harness scrubs secret-shaped strings from the `text`, `content`,
  `message` and `input` fields of outbound events (`GRPCTransport.Emit` in
  `harness/internal/transport/grpc.go`). The scrub is pattern-based, so it
  misses secrets it has no pattern for, and it does not apply to inbound
  events. The sandbox identity token is inbound and is never scrubbed.

Impersonation is the larger risk. A peer that answers at the dial target
can assign a `RunConfig` of its choosing and approve every permission
request. That `RunConfig` can point the provider `baseUrl` at an endpoint
the peer controls while `apiKeyRef` names any environment variable
(`secret://NAME`) or readable file (`secret://file:///path`) in the job;
the harness resolves the reference and sends the value to that endpoint.
Only server authentication, the harness verifying the control plane's
identity, closes this.

## Choosing a recipe

| | Harness TLS | Harness mutual TLS | Istio strict mutual TLS |
|---|---|---|---|
| Stream encrypted on the network | yes | yes | yes, between sidecars |
| Harness verifies the control plane | yes, against a CA bundle | yes, against a CA bundle | partly: any mesh identity authorised to serve the Service is accepted |
| Control plane verifies the job | no | yes, if it requires client certificates | yes, with the AuthorizationPolicy |
| Control plane knows which job | no | only with per-job certificates | no: every job shares one identity |
| Plaintext inside the job Pod | no | no | yes, between the harness and its sidecar |
| Exercised | locally | locally and on `kind` | manifests validated only |

Harness mutual TLS is the default choice: it needs no mesh, the harness
pins the control plane to the CA in `CONTROL_PLANE_TLS_CA_FILE`, and a job given
the TLS settings refuses to fall back to plaintext. The mesh recipe suits
clusters that already run Istio. The two compose: with the control-plane
Service set to `appProtocol: tcp`, the sidecars carry the harness's TLS
connection as opaque bytes, and the harness's CA check covers the mesh's
weaker server check. That combination has not been exercised. The two
recipes' manifests overlap and must be merged by hand: both define the
`stirrup-jobs` and `stirrup-control` namespaces with different labels
(sidecar injection, Pod Security level) and the `stirrup-control-plane`
Service with different ports (9443 and 9090).

Neither recipe protects the transport credential from the run itself. With
the `local` executor, the agent's commands run as the harness's own user in
the harness's container: they can read the client key, or dial the control
plane through the sidecar with the job's mesh identity. A run with
`run_command` should use a sandboxing executor (`container`, `k8s` or
`k8s-sandbox`), and per-job, short-lived client certificates limit the
usefulness of a leaked key.

## Harness TLS and mutual TLS

The settings are environment variables for `stirrup job` and flags for
`stirrup harness --transport grpc`; the flags are a usage error with any
other transport. They are not `RunConfig` fields, because the job dials
before any `RunConfig` arrives (`harness/cmd/stirrup/cmd/transporttls.go`).

| `stirrup job` variable | `stirrup harness` flag | Meaning |
|---|---|---|
| `CONTROL_PLANE_TLS_CA_FILE` | `--transport-tls-ca-file` | PEM CA bundle that must have issued the control plane's certificate. Default: the system roots. |
| `CONTROL_PLANE_TLS_CERT_FILE` | `--transport-tls-cert-file` | PEM client certificate for mutual TLS. Set together with the key. |
| `CONTROL_PLANE_TLS_KEY_FILE` | `--transport-tls-key-file` | PEM private key for the client certificate. |
| `CONTROL_PLANE_TLS_SERVER_NAME` | `--transport-tls-server-name` | Name to verify the server certificate against, when it differs from the host in the address. |

Setting any one of them enables TLS, with no plaintext fallback. The
following fail the job before it dials:

- a variable that is present but empty (an unrendered template, say);
- a certificate without a key, or a key without a certificate;
- a file that is missing, unreadable, or not valid PEM.

A job with none of the variables set dials plaintext, so the guarantee
holds only while the Job template carries them. Enforce that where Jobs
are admitted (a ValidatingAdmissionPolicy on the `stirrup-jobs` namespace,
for example), or alert on the startup line. The harness logs that line
once, naming the mode and the CA file but never file contents:

```text
stirrup: control plane transport mode=mutual-tls ca=/etc/stirrup/control-plane-tls/ca.crt addr=stirrup-control-plane.stirrup-control.svc:9443
```

### What the harness verifies

- The connection must use TLS 1.3 (`MinVersion` in
  `harness/internal/transport/tls.go`).
- The server certificate chains to the CA bundle, or to the system roots
  when no bundle is set. With the system roots, any public CA that will
  issue a certificate for the control plane's hostname is trusted, so set
  the CA file whenever the control plane uses a private CA.
- The certificate is valid for the host part of `CONTROL_PLANE_ADDR`, or
  for `CONTROL_PLANE_TLS_SERVER_NAME` when set. An address given as an IP
  needs an IP SAN.
- The server negotiates `h2` through ALPN. grpc-go requires it, so a
  TLS-terminating proxy or load balancer in front of the control plane
  must offer both TLS 1.3 and `h2`.

The harness does not check revocation (CRL or OCSP) and does not pin a
certificate beyond the CA bundle. It reads the files once at startup and
performs one handshake per job, so a certificate only has to be valid when the
job connects, and a renewed certificate takes effect in the next job Pod.

### What the control plane must do

Whether the job is authenticated is the control plane's decision. A
control plane that does not request client certificates accepts any TLS
client, and the job's certificate is ignored. To make it mutual TLS, the
server must require and verify client certificates against the CA that
issued them. In Go:

```go
cfg := &tls.Config{
    MinVersion:   tls.VersionTLS13,
    Certificates: []tls.Certificate{serverCert},
    ClientCAs:    jobCAPool,
    ClientAuth:   tls.RequireAndVerifyClientCert,
}
srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
```

The [example control plane](../examples/controlplane/README.md#tls) does
this with `-tls-cert`, `-tls-key` and `-tls-client-ca`
(`examples/controlplane/tls.go`). Without `-tls-client-ca` it serves TLS
with no client authentication, which is the "Harness TLS" column above.

`CONTROL_PLANE_SESSION_ID` and the `ready` event are not authenticated by
TLS. With one client certificate shared by every job, as in the recipe
below, any holder of that certificate can claim another job's session ID
and receive that session's `RunConfig` and sandbox identity token. A
control plane that runs several jobs should map the verified client
certificate's identity to the session it launched, rather than trusting
the session ID the job reports. Issuing one certificate per job, for
example with the run ID in a URI SAN, makes that mapping possible. The
`sandbox_token_request` contract expects it: the control plane derives
the run identity from the authenticated stream the task was assigned on
(`proto/harness/v1/harness.proto`). The sandbox identity token flow
therefore needs per-job certificates, or a control plane that issues a
token only to the stream it assigned the task on.

### Certificates

Any PKI works. For a test or a small deployment, `openssl` (OpenSSL 1.1.1
or later, or LibreSSL) produces a CA, a server certificate for the control
plane's Service name and a client certificate. Go rejects EC keys with
explicit curve parameters and SHA-1 signatures, hence
`ec_param_enc:named_curve` and `-sha256`:

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve \
  -nodes -days 365 -subj "/CN=stirrup control-plane CA" -keyout ca-key.pem -out ca.pem

openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve \
  -nodes -subj "/CN=stirrup-control-plane" -keyout server-key.pem -out server.csr
printf 'subjectAltName=DNS:stirrup-control-plane.stirrup-control.svc\nextendedKeyUsage=serverAuth\n' > server.ext
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -sha256 \
  -days 90 -extfile server.ext -out server.pem

openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve \
  -nodes -subj "/CN=stirrup-job" -keyout client-key.pem -out client.csr
printf 'extendedKeyUsage=clientAuth\n' > client.ext
openssl x509 -req -in client.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -sha256 \
  -days 30 -extfile client.ext -out client.pem
```

This issues one client certificate that every job shares; per-job
certificates need an issuer the control plane calls when it creates each
Job. The server and client certificates may come from different CAs: the
job trusts the server's CA through `CONTROL_PLANE_TLS_CA_FILE`, and the
control plane trusts the job's CA through its own client-CA setting.

With cert-manager, give the control-plane and job certificates a dedicated
issuer rather than a cluster-wide one: anyone who can request a
certificate for the control plane's DNS name from the issuer that the job
trusts can impersonate the control plane. cert-manager's `ca.crt` is the
CA of the certificate's own issuer, so it is the right
`CONTROL_PLANE_TLS_CA_FILE` only when one issuer signs both sides. Set
`usages` to include `client auth` on the job's `Certificate` and
`server auth` on the control plane's; a job certificate without the
client-auth usage fails with the unhelpful `remote error: tls: bad
certificate`.

### Kubernetes

[`examples/k8s/control-transport/native-tls/`](../examples/k8s/control-transport/native-tls/)
holds the recipe:

| File | Applied by | Purpose |
|---|---|---|
| `namespace.yaml` | `kubectl apply -k` | The `stirrup-jobs` namespace (Pod Security `restricted`) and the `stirrup-job` ServiceAccount. |
| `control-plane-service.yaml` | `kubectl apply -k` | The `stirrup-control` namespace and the RunTask Service on port 9443. |
| `job.yaml` | the control plane, per run | A Job that mounts the client Secret and sets the three `CONTROL_PLANE_TLS_*` file variables. |

The names `stirrup-control-plane.stirrup-control.svc` and port 9443 are
placeholders; the server certificate's SAN, the Service and
`CONTROL_PLANE_ADDR` in `job.yaml` must agree. Both Secrets are created
out of band, so key material never sits in a manifest:

```sh
kubectl apply -k examples/k8s/control-transport/native-tls

# The job's client certificate and the CA that issued the server's.
kubectl -n stirrup-jobs create secret generic stirrup-control-plane-client-tls \
  --from-file=ca.crt=ca.pem --from-file=tls.crt=client.pem --from-file=tls.key=client-key.pem

# The control plane's server certificate and the CA that issued the job's.
kubectl -n stirrup-control create secret generic stirrup-control-plane-server-tls \
  --from-file=ca.crt=ca.pem --from-file=tls.crt=server.pem --from-file=tls.key=server-key.pem
```

The control plane mounts its Secret and loads it into the TLS
configuration above. No image of the example control plane is published;
to use it in a cluster, build it from `examples/controlplane` into an
image of its own. Its container section is then:

```yaml
args: ["-config", "/etc/controlplane/runconfig.json", "-listen", "0.0.0.0:9443",
       "-tls-cert", "/tls/tls.crt", "-tls-key", "/tls/tls.key", "-tls-client-ca", "/tls/ca.crt"]
volumeMounts:
  - name: tls
    mountPath: /tls
    readOnly: true
```

`job.yaml` mounts the client Secret with mode `0440` and `fsGroup: 65532`,
the image's group, so the harness can read the key and the files are not
world-readable. Anyone who can read Secrets in `stirrup-jobs`, or create
a Pod there that mounts this one, holds the job identity, so restrict
both. A job that also runs the k8s executor needs a token
(`automountServiceAccountToken: true`) and the orchestrator Role from
[`examples/k8s/rbac.yaml`](../examples/k8s/rbac.yaml) bound to
`system:serviceaccount:stirrup-jobs:stirrup-job` in the sandbox namespace.
Its sandbox Pods then lose owner-reference cleanup, which does not cross
namespaces. The same applies to the mesh recipe.

### Verification

Run each check against a control plane that is not dispatching real work;
a probe that is wrongly accepted is assigned a task.

1. Plaintext must be refused. Remove the three `CONTROL_PLANE_TLS_*`
   variables from a copy of `job.yaml` and run it. The job fails before the
   stream opens:

   ```text
   stirrup: control plane transport mode=plaintext addr=stirrup-control-plane.stirrup-control.svc:9443
   Error: failed to connect to control plane at stirrup-control-plane.stirrup-control.svc:9443: open RunTask stream: rpc error: code = Unavailable desc = connection error: desc = "error reading server preface: EOF"
   ```

2. A job without a client certificate must be refused. Remove only
   `CONTROL_PLANE_TLS_CERT_FILE` and `CONTROL_PLANE_TLS_KEY_FILE`. The
   startup line reads `mode=tls` and the job fails with
   `remote error: tls: certificate required`. A control plane that lets
   this job through is not requiring client certificates.
3. With all three variables, the startup line reads `mode=mutual-tls` and
   the run proceeds. A job that trusts the wrong CA fails with
   `x509: certificate signed by unknown authority`.

This recipe was exercised on 2026-10-29. Locally, `stirrup job` ran
against the example control plane in mutual-TLS mode. A plaintext job, a
job without a client certificate, a job trusting another CA, a job with a
mismatched server name and a job with an empty variable all failed, and a
job with the CA and client certificate completed its run. A control plane
serving TLS without client authentication accepted a job with only the
CA. On a `kind` cluster (Kubernetes 1.37), `job.yaml`, changed only in
its image and a workspace volume, completed a run against the example
control plane serving mutual TLS in the cluster, and its plaintext copy
was refused.

## Service-mesh mutual TLS

With Istio, the harness dials plaintext to its own sidecar, and the
sidecars carry the connection over Istio mutual TLS. The harness needs no
TLS settings, and its startup line reads `mode=plaintext`.

Prerequisites:

- Istio with sidecar injection. The manifests use the `v1` APIs of
  `security.istio.io` and `networking.istio.io`, and the default trust
  domain `cluster.local`.
- Native sidecars, which `job.yaml` requests with the
  `sidecar.istio.io/nativeSidecar` annotation (Alpha in Istio's annotation
  reference) and which need Kubernetes 1.29 or later. The harness then
  starts only after its proxy is ready, and the Job completes when the
  harness exits. If the annotation is not honoured, `istio-proxy` appears
  under `containers` rather than `initContainers`, and the Job Pod keeps
  running after the harness exits.
- The Istio CNI node agent, for the `baseline` Pod Security level on the
  job namespace. Without it, `istio-init` needs `NET_ADMIN` and `NET_RAW`.
- Sidecar mode. Ambient mode and Linkerd are not covered here, and a
  mesh's default (Istio `PERMISSIVE`, Linkerd's opportunistic mTLS) does
  not refuse plaintext; only an explicit `STRICT` policy does.

The job's sidecar carries all of its traffic, not only the control-plane
stream: provider APIs, OTLP, MCP servers. A mesh with
`outboundTrafficPolicy: REGISTRY_ONLY` needs a ServiceEntry for each. An
operator's own control-plane Service needs `appProtocol: tcp` (or a
`tcp-` port name), as in `control-plane-service.yaml`, so the sidecars
treat the long-lived stream as opaque TCP.

[`examples/k8s/control-transport/istio-mtls/`](../examples/k8s/control-transport/istio-mtls/)
holds the manifests:

| File | Applied by | Purpose |
|---|---|---|
| `namespaces.yaml` | `kubectl apply -k` | `stirrup-control` and `stirrup-jobs`, both with sidecar injection. |
| `serviceaccount.yaml` | `kubectl apply -k` | The `stirrup-job` identity that job Pods run as. |
| `control-plane-service.yaml` | `kubectl apply -k` | The RunTask Service on port 9090, marked `appProtocol: tcp`. |
| `peer-authentication.yaml` | `kubectl apply -k` | `STRICT` mutual TLS for every workload in `stirrup-control`. |
| `authorization-policy.yaml` | `kubectl apply -k` | Admits only `cluster.local/ns/stirrup-jobs/sa/stirrup-job` to port 9090. |
| `destination-rule.yaml` | `kubectl apply -k` | Makes job sidecars always originate Istio mutual TLS to the control plane. |
| `job.yaml` | the control plane, per run | A Job running as `stirrup-job` with a native sidecar. |
| `plaintext-probe.yaml` | by hand, to verify | Two Pods that must be refused. |

```sh
kubectl apply -k examples/k8s/control-transport/istio-mtls
```

Each object addresses a different gap. `PeerAuthentication` refuses clients
without a mesh certificate. The `AuthorizationPolicy` restricts the mesh
to the job identity; without it, any meshed workload could connect. The
`DestinationRule` stops a job sidecar from sending plaintext when the
control plane has lost its own sidecar. The job namespace is separate from
the k8s executor's sandbox namespace, so sandbox Pods stay out of the mesh.

`STRICT` applies to the whole `stirrup-control` namespace, so any other
workload there also stops accepting plaintext, and the `AuthorizationPolicy`
denies every other port on the control-plane Pods until a rule admits it.

### What the mesh verifies

The control plane's sidecar verifies the job's mesh identity against the
`AuthorizationPolicy`. The job's sidecar checks only that the server's
identity is one Istio considers authorised to serve
`stirrup-control-plane` (Istio's secure naming). The mesh does not cover:

- Who can serve the control plane. Any Pod that can join the Service's
  endpoints, and any routing rule visible to the job namespace that sends
  the host elsewhere, is accepted as the control plane. Restrict who can
  create Pods, Services and Istio routing objects in `stirrup-control` and
  `stirrup-jobs`, or layer harness TLS on top to pin the control plane's
  CA.
- Changes to the policy objects themselves. A workload-level
  `PERMISSIVE` PeerAuthentication or a deleted AuthorizationPolicy
  silently undoes the recipe, so restrict who can write
  `security.istio.io` and `networking.istio.io` objects in both
  namespaces.
- A control-plane Pod that lost its sidecar. `STRICT` and the
  `AuthorizationPolicy` are both enforced by that sidecar, so without it
  the control plane accepts plaintext from anything that can reach it.
  Jobs refuse to connect to it (the `DestinationRule`), but other clients
  are not stopped. Check that `istio-proxy` is present in control-plane
  Pods.
- The hop between the harness and its sidecar, which is plaintext inside
  the Pod's network namespace, sandbox identity token included. Only the
  Pod's own containers share that namespace, so the job Pod should run
  nothing besides the harness and the proxy.
- A job Pod that was not injected. Its harness dials the control plane
  directly in plaintext, and `STRICT` refuses the connection before the
  gRPC stream opens: only the HTTP/2 connection preface leaves the job,
  and the control plane never sees the connection. Check that
  `istio-proxy` is present in job Pods.
- Which session a job belongs to. The mesh identity is per
  ServiceAccount, so every job shares it; the session ID remains
  unauthenticated.

### Verification

First, run one real job from `job.yaml` to completion. Without that
positive control, a refused probe proves nothing: a mistyped Service name
or a stopped control plane refuses everything. Then apply the probes at a
time the control plane is not dispatching real work, since a probe that
is wrongly accepted is assigned a task:

```sh
kubectl apply -f examples/k8s/control-transport/istio-mtls/plaintext-probe.yaml
kubectl -n stirrup-jobs logs plaintext-probe
kubectl -n stirrup-jobs logs wrong-identity-probe -c stirrup
kubectl -n stirrup-jobs delete pod plaintext-probe wrong-identity-probe
```

Both must fail with `failed to connect to control plane at …` and a
transport error from the control plane's side, such as `error reading
server preface` with `EOF` or `connection reset by peer`. `no such host`,
`connection refused` or `i/o timeout` mean the probe never reached the
control plane's sidecar and prove nothing. These strings are inferred
from the harness's behaviour against a refusing TLS listener; the recipe
has not been run on a live mesh. `plaintext-probe` has no sidecar and is
refused by `PeerAuthentication`; `wrong-identity-probe` is meshed but runs
as the `default` ServiceAccount and is refused by the
`AuthorizationPolicy`. Confirm injection on a real job, and on the
control-plane Pods:

```sh
# Native sidecars are init containers.
kubectl -n stirrup-jobs get pod -l job-name=<job> \
  -o jsonpath='{.items[0].spec.initContainers[*].name}'
kubectl -n stirrup-control get pod -l app=stirrup-control-plane \
  -o jsonpath='{.items[*].spec.initContainers[*].name} {.items[*].spec.containers[*].name}'
```

Each output must include `istio-proxy`.

This recipe has not been run against a live mesh. On 2026-10-29 its
manifests passed `kubeconform -strict` with the Istio CRD schemas, and
passed a server-side apply against a `kind` cluster (Kubernetes 1.37) with
the Istio 1.31.3 CRDs installed but no `istiod`. Istio's own validating
webhook therefore did not run.

## Related

- [`deployment.md`](deployment.md#transport-security-posture-v01): the
  deployment posture and the `stirrup job` environment.
- [`integration-guide.md`](integration-guide.md#transport-security-posture-v01):
  the gRPC contract from the control plane's side.
- [`safety-rings.md`](safety-rings.md) and [`security.md`](security.md):
  the harness's other security layers.
