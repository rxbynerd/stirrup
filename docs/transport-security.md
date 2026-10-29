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

- API keys and other credentials travel as `secret://` references, never as
  values; the harness resolves them inside the job
  (`harness/internal/security/secretstore.go`).
- The harness scrubs secret-shaped strings from the string fields of
  outbound events (`GRPCTransport.Emit` in
  `harness/internal/transport/grpc.go`). The scrub is pattern-based, so it
  misses secrets it has no pattern for, and it does not apply to inbound
  events. The sandbox identity token is inbound and is never scrubbed.

Impersonation is the larger risk. A peer that answers at the dial target
can assign a `RunConfig` of its choosing, approve every permission
request, and point the provider `baseUrl` at an endpoint it controls while
`apiKeyRef` names a secret available to the job; the harness then resolves
that secret and sends it to the peer's endpoint. Only server
authentication — the harness verifying the control plane's certificate —
closes this.

## Choosing a recipe

| | Harness TLS | Harness mutual TLS | Istio strict mutual TLS |
|---|---|---|---|
| Stream encrypted on the network | yes | yes | yes, between sidecars |
| Harness verifies the control plane | yes, against a CA bundle | yes, against a CA bundle | yes, by mesh identity |
| Control plane verifies the job | no | yes, if it requires client certificates | yes, with the AuthorizationPolicy |
| Plaintext inside the job Pod | no | no | yes, between the harness and its sidecar |
| Exercised | locally and on `kind` | locally and on `kind` | manifests validated only |

Harness mutual TLS is the default choice: it needs no mesh, and the
harness itself refuses to fall back to plaintext. The mesh recipe suits
clusters that already run Istio. The two compose: with the control-plane
Service set to `appProtocol: tcp`, the sidecars carry the harness's TLS
connection as opaque bytes. That combination has not been exercised.

## Harness TLS and mutual TLS

The settings are environment variables for `stirrup job` and flags for
`stirrup harness --transport grpc`. They are not `RunConfig` fields,
because the job dials before any `RunConfig` arrives
(`harness/cmd/stirrup/cmd/transporttls.go`).

| `stirrup job` variable | `stirrup harness` flag | Meaning |
|---|---|---|
| `CONTROL_PLANE_TLS_CA_FILE` | `--transport-tls-ca-file` | PEM CA bundle that must have issued the control plane's certificate. Default: the system roots. |
| `CONTROL_PLANE_TLS_CERT_FILE` | `--transport-tls-cert-file` | PEM client certificate for mutual TLS. Set together with the key. |
| `CONTROL_PLANE_TLS_KEY_FILE` | `--transport-tls-key-file` | PEM private key for the client certificate. |
| `CONTROL_PLANE_TLS_SERVER_NAME` | `--transport-tls-server-name` | Name to verify the server certificate against, when it differs from the host in the address. |

Setting any one of them enables TLS. There is no plaintext fallback, and
the following fail the job before it dials:

- a variable that is present but empty (an unrendered template, say);
- a certificate without a key, or a key without a certificate;
- a file that is missing, unreadable, or not valid PEM.

The harness logs one line at startup naming the mode, for example
`stirrup: control plane transport mode=mutual-tls ca=/etc/stirrup/control-plane-tls/ca.crt addr=…`.
The line names file paths, never their contents.

### What the harness verifies

- The connection is TLS 1.3 or nothing (`MinVersion` in
  `harness/internal/transport/tls.go`).
- The server certificate chains to the CA bundle, or to the system roots
  when no bundle is set. With the system roots, any public CA that will
  issue a certificate for the control plane's hostname is trusted, so set
  the CA file whenever the control plane uses a private CA.
- The certificate is valid for the host part of `CONTROL_PLANE_ADDR`, or
  for `CONTROL_PLANE_TLS_SERVER_NAME` when set. An address given as an IP
  needs an IP SAN.
- The server negotiates `h2` through ALPN. grpc-go requires it, so a
  TLS-terminating proxy in front of the control plane must offer `h2`.

The harness does not check revocation (CRL or OCSP), does not pin a
certificate beyond the CA bundle, and reads the files once at startup. A
renewed client certificate takes effect in the next job Pod; keep each
certificate valid for longer than `activeDeadlineSeconds`.

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
(`examples/controlplane/tls.go`).

`CONTROL_PLANE_SESSION_ID` and the `ready` event are not authenticated by
TLS. A control plane that runs several jobs should map the verified client
certificate's identity to the session it launched, rather than trusting
the session ID the job reports. Issuing one certificate per job, for
example with the run ID in a URI SAN, makes that mapping possible. The
`sandbox_token_request` contract already assumes this: the control plane
derives the run identity from the authenticated stream the task was
assigned on (`proto/harness/v1/harness.proto`).

### Certificates

Any PKI works. For a test or a small deployment, `openssl` produces a CA,
a server certificate for the control plane's Service name and a client
certificate:

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
  -subj "/CN=stirrup control-plane CA" -keyout ca-key.pem -out ca.pem

openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=stirrup-control-plane" -keyout server-key.pem -out server.csr
printf 'subjectAltName=DNS:stirrup-control-plane.stirrup-control.svc\nextendedKeyUsage=serverAuth\n' > server.ext
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -days 90 -extfile server.ext -out server.pem

openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=stirrup-job" -keyout client-key.pem -out client.csr
printf 'extendedKeyUsage=clientAuth\n' > client.ext
openssl x509 -req -in client.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -days 1 -extfile client.ext -out client.pem
```

In production, cert-manager or the organisation's CA issues these
instead. The server and client certificates may come from different CAs:
the job trusts the server's CA through `CONTROL_PLANE_TLS_CA_FILE`, and
the control plane trusts the job's CA through its own client-CA setting.

### Kubernetes

[`examples/k8s/control-transport/native-tls/`](../examples/k8s/control-transport/native-tls/)
holds the recipe:

| File | Applied by | Purpose |
|---|---|---|
| `namespace.yaml` | `kubectl apply -k` | The `stirrup-jobs` namespace (Pod Security `restricted`) and the `stirrup-job` ServiceAccount. |
| `job.yaml` | the control plane, per run | A Job that mounts the client Secret and sets the three `CONTROL_PLANE_TLS_*` file variables. |

The client Secret is created out of band, so key material never sits in
a manifest:

```sh
kubectl apply -k examples/k8s/control-transport/native-tls
kubectl -n stirrup-jobs create secret generic stirrup-control-plane-client-tls \
  --from-file=ca.crt=ca.pem --from-file=tls.crt=client.pem --from-file=tls.key=client-key.pem
```

A cert-manager `Certificate` writes `tls.crt` and `tls.key`, and `ca.crt`
when its issuer provides one, so `job.yaml` works with either. The Secret
is mounted with mode `0440` and `fsGroup: 65532`, the image's group, so the
harness can read the key and the files are not world-readable.

### Verification

The control plane must refuse a plaintext job. Remove the three
`CONTROL_PLANE_TLS_*` variables from a copy of `job.yaml` and run it; the
job fails before any event is exchanged:

```text
stirrup: control plane transport mode=plaintext addr=stirrup-control-plane.stirrup-control.svc:9443
Error: failed to connect to control plane at stirrup-control-plane.stirrup-control.svc:9443: open RunTask stream: rpc error: code = Unavailable desc = connection error: desc = "error reading server preface: EOF"
```

With the variables in place, the startup line reads `mode=mutual-tls` and
the run proceeds. A job that trusts the wrong CA fails with
`x509: certificate signed by unknown authority`; one without a client
certificate fails with `tls: certificate required`.

This recipe was exercised on 2026-10-29: locally with `stirrup job`
against the example control plane, and on a `kind` cluster (Kubernetes
1.37) where `job.yaml`, with only its image and a workspace volume
changed, completed a run against the example control plane serving mutual
TLS in the cluster, and the plaintext copy was refused.

## Service-mesh mutual TLS

With Istio, the harness dials plaintext to its own sidecar, and the
sidecars carry the connection over Istio mutual TLS. The harness needs no
TLS settings, and its startup line reads `mode=plaintext`.

Prerequisites:

- Istio with sidecar injection. The manifests use the `v1` APIs of
  `security.istio.io` and `networking.istio.io`.
- Native sidecars, which `job.yaml` requests with the
  `sidecar.istio.io/nativeSidecar` annotation and which need Kubernetes
  1.29 or later. The harness then starts only after its proxy is ready, and
  the Job completes when the harness exits; a classic sidecar keeps a Job
  Pod running.
- The Istio CNI node agent, for the `baseline` Pod Security level on the
  job namespace. Without it, `istio-init` needs `NET_ADMIN` and `NET_RAW`.

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

Each object closes a different gap. `PeerAuthentication` refuses clients
without a mesh certificate. The `AuthorizationPolicy` restricts the mesh
to the job identity; without it, any meshed workload could connect. The
`DestinationRule` stops a job sidecar from sending plaintext when the
control plane has lost its own sidecar. The job namespace is separate from
the k8s executor's sandbox namespace, so sandbox Pods stay out of the mesh.
A job that also runs the k8s executor needs the orchestrator Role from
[`examples/k8s/rbac.yaml`](../examples/k8s/rbac.yaml) bound to
`stirrup-job` in the sandbox namespace, and its sandbox Pods lose
owner-reference cleanup, which does not cross namespaces.

### What the mesh verifies

The client sidecar verifies the control plane's mesh identity, and the
control plane's sidecar verifies the job's. The mesh does not cover:

- The hop between the harness and its sidecar, which is plaintext inside
  the Pod's network namespace, sandbox identity token included. Only the
  Pod's own containers share that namespace, so the job Pod should run
  nothing besides the harness and the proxy.
- A job Pod that was not injected. Its harness dials the control plane
  directly in plaintext. `STRICT` refuses the connection, so the control
  plane never sends it a `RunConfig` or a token, but the job's opening
  bytes, including its `ready` event, may already have crossed the network.
  Check that `istio-proxy` is present in job Pods.
- Which session a job belongs to. The mesh identity is per
  ServiceAccount, so every job shares it; the session ID remains
  unauthenticated.

### Verification

Apply the probes after the control plane is running:

```sh
kubectl apply -f examples/k8s/control-transport/istio-mtls/plaintext-probe.yaml
kubectl -n stirrup-jobs logs plaintext-probe
kubectl -n stirrup-jobs logs wrong-identity-probe -c stirrup
kubectl -n stirrup-jobs delete pod plaintext-probe wrong-identity-probe
```

Both must end with `failed to connect to control plane` or another
connection error, never with an assignment. `plaintext-probe` has no
sidecar and is refused by `PeerAuthentication`; `wrong-identity-probe` is
meshed but runs as the `default` ServiceAccount and is refused by the
`AuthorizationPolicy`. For a real job, confirm injection with:

```sh
kubectl -n stirrup-jobs get pod -l job-name=<job> \
  -o jsonpath='{.items[0].spec.initContainers[*].name}'
```

The output must include `istio-proxy`.

This recipe has not been run against a live mesh. On 2026-10-29 its
manifests passed `kubeconform -strict` with the Istio CRD schemas, and a
server-side apply against a `kind` cluster (Kubernetes 1.37) with the
Istio 1.31.3 CRDs installed but no `istiod`, so Istio's own validating
webhook did not run.

## Related

- [`deployment.md`](deployment.md#transport-security-posture-v01): the
  deployment posture and the `stirrup job` environment.
- [`integration-guide.md`](integration-guide.md#transport-security-posture-v01):
  the gRPC contract from the control plane's side.
- [`safety-rings.md`](safety-rings.md) and [`security.md`](security.md):
  the harness's other security layers.
