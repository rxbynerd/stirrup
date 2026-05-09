# Shipping Stirrup telemetry to a managed APM

Stirrup's OTel exporters speak both **OTLP/gRPC** (the default) and
**OTLP/HTTP with binary protobuf** (issue #100). The HTTP path is what
unlocks first-class native support for Grafana Cloud, Honeycomb, GCP
Cloud Trace, Datadog's OTLP intake, and the long tail of managed APMs
that reject gRPC at the edge.

This document is the operator walkthrough. For the underlying signal
model — what spans, metrics, and resource attributes Stirrup emits —
see [`safety-rings.md`](safety-rings.md) and the
"OpenTelemetry trace emitter" section of the repo-root `CLAUDE.md`.

## Quick choice

| Where do you want telemetry to land? | Use |
|---|---|
| Grafana Cloud, Honeycomb, GCP Cloud Trace, any managed OTLP gateway | **Native HTTP** (this doc) |
| Local OpenTelemetry Collector, Tempo, or in-cluster collector | gRPC (default) |
| You also need sampling, fan-out, multi-tenant routing, or you're already running a sidecar for other reasons | Grafana Alloy (see "When you'd still want Alloy" below) |

The native HTTP path replaces the Alloy bridge for the simple case.
There is no need to deploy Alloy, no extra failure mode, no extra
config to keep in sync — Stirrup posts OTLP/protobuf directly to the
managed gateway over HTTPS.

## Native HTTP: Grafana Cloud example

Grafana Cloud's OTLP gateway is HTTP-only and expects the configured
URL to end in `/otlp`. The SDK appends `/v1/traces` and `/v1/metrics`
per signal.

### RunConfig

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
    "metricsEndpoint": "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
    "headers": {
      "Authorization": "secret://GRAFANA_CLOUD_AUTH"
    }
  },
  "observability": {
    "environment": "production",
    "serviceNamespace": "stirrup"
  }
}
```

The `Authorization` header value resolves through the SecretStore at
exporter init time:

- `secret://GRAFANA_CLOUD_AUTH` → `os.Getenv("GRAFANA_CLOUD_AUTH")`
- `secret://file:///var/run/secrets/grafana/auth` → file contents,
  trimmed.

The resolved bearer never enters logs (`security.LogScrubber` strips
`Bearer …`, `Basic …`, and other secret-shaped patterns) and the
unresolved reference is rewritten to `secret://[REDACTED]` by
`RunConfig.Redact()` before any `RunTrace` is persisted.

The header value Grafana Cloud expects is `Basic <base64(instanceID:apiToken)>`:

```sh
export GRAFANA_CLOUD_AUTH="Basic $(echo -n '123456:glc_eyJv...' | base64)"
```

### CLI

```sh
export GRAFANA_CLOUD_AUTH="Basic ..."

stirrup harness \
  --prompt "..." \
  --trace-emitter otel \
  --otel-protocol http/protobuf \
  --otel-endpoint https://otlp-gateway-prod-us-east-0.grafana.net/otlp \
  --config grafana-cloud.json    # for the headers map; flags don't expose it
```

The `--otel-protocol` flag is the only new wire-protocol surface; the
existing `--otel-endpoint` accepts a full URL when the protocol is
`http/protobuf`.

## Other managed APMs

| Vendor | endpoint | header |
|---|---|---|
| Honeycomb | `https://api.honeycomb.io` | `x-honeycomb-team: secret://HC_API_KEY` |
| GCP Cloud Trace (via gateway) | gateway URL | `Authorization: secret://GCP_BEARER` |
| Datadog OTLP intake | `https://trace.agent.datadoghq.com` | `dd-api-key: secret://DD_API_KEY` |

Stirrup does not vendor-detect — `protocol`, `endpoint`, and `headers`
are sufficient for any OTLP/HTTP-protobuf gateway.

## Precedence

Three places can set the wire protocol. They resolve in this order:

1. `--config` file (`traceEmitter.protocol` field)
2. `--otel-protocol` CLI flag — only when explicitly passed; an unset
   flag does **not** clobber the file value (mirrors every other
   override flag)
3. `OTEL_EXPORTER_OTLP_PROTOCOL` env var — read by the OTel SDK when
   neither of the above is set
4. SDK default: `grpc`

The same precedence chain applies to endpoints
(`OTEL_EXPORTER_OTLP_ENDPOINT`) and headers
(`OTEL_EXPORTER_OTLP_HEADERS`). When you supply `traceEmitter.headers`
in the RunConfig, the SDK env var is **not** merged — the harness's
resolved map is the only source. This is deliberate: silently merging
two header maps would let an out-of-band env var override a
config-file Authorization and surprise the operator.

## URL appending

The SDK appends per-signal segments to the configured base path:

| Configured | Traces POST | Metrics POST |
|---|---|---|
| `https://gateway/otlp` | `https://gateway/otlp/v1/traces` | `https://gateway/otlp/v1/metrics` |
| `https://gateway` | `https://gateway/v1/traces` | `https://gateway/v1/metrics` |
| `localhost:4318` | `http://localhost:4318/v1/traces` | `http://localhost:4318/v1/metrics` |

Grafana Cloud expects `/otlp` (the gateway routes per-tenant from
that prefix). Honeycomb expects no extra path; their gateway routes
on the dataset header.

TLS is on by default for `https://` URLs. The exporter falls back to
plaintext (`WithInsecure()`) only for `http://` URLs or scheme-less
endpoints (the typical local-collector flow). An `https://` URL never
silently downgrades.

## Kubernetes Job — native HTTP, no Alloy sidecar

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: grafana-cloud-auth
type: Opaque
stringData:
  # base64("instanceID:glc_apiToken")
  auth: "Basic MTIzNDU2OmdsY18uLi4="
---
apiVersion: batch/v1
kind: Job
metadata:
  name: stirrup-run
spec:
  template:
    spec:
      containers:
        - name: stirrup
          image: ghcr.io/rxbynerd/stirrup:latest
          args:
            - harness
            - --config
            - /etc/stirrup/run.json
            - --prompt
            - "Refactor the auth middleware to use Cedar."
          env:
            - name: GRAFANA_CLOUD_AUTH
              valueFrom:
                secretKeyRef:
                  name: grafana-cloud-auth
                  key: auth
            - name: ANTHROPIC_API_KEY
              valueFrom:
                secretKeyRef:
                  name: anthropic-api-key
                  key: token
          volumeMounts:
            - name: run-config
              mountPath: /etc/stirrup
              readOnly: true
      volumes:
        - name: run-config
          configMap:
            name: stirrup-run-config
      restartPolicy: Never
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: stirrup-run-config
data:
  run.json: |
    {
      "mode": "execution",
      "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
      "traceEmitter": {
        "type": "otel",
        "protocol": "http/protobuf",
        "endpoint": "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
        "metricsEndpoint": "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
        "headers": {
          "Authorization": "secret://GRAFANA_CLOUD_AUTH"
        }
      },
      "observability": {
        "environment": "production",
        "serviceNamespace": "stirrup"
      }
    }
```

No Alloy sidecar, no DaemonSet, no extra Service object. The pod
posts directly to Grafana Cloud over the cluster's egress.

## When you'd still want Grafana Alloy

The native HTTP path is sufficient for the common case. You'd still
want a sidecar collector (Alloy, the OTel Collector, Vector) when you
need:

- **Sampling.** Tail-based sampling, head-based sampling, or rate
  limiting per-route. Stirrup ships every span; the collector
  decides what to keep. Useful when an eval suite produces 10k runs
  in an hour and you only want regressions in the long-term store.
- **Fan-out.** Send the same telemetry to two backends (Grafana
  Cloud + an internal Tempo, for example) without authoring two
  RunConfigs.
- **Multi-tenant routing.** Re-write resource attributes
  (`service.namespace`, `deployment.environment`) per route so a
  shared cluster can serve multiple tenants without each pod knowing
  its own tenant.
- **Protocol translation for sub-systems.** Some collectors also
  ingest Prometheus scrapes, syslog, or proprietary formats. Stirrup
  itself only emits OTLP, but a collector can normalise a wider
  fleet.
- **Cluster-local buffering.** Disk-backed queues for outage
  resilience, ahead of cross-region egress.

If none of those apply, ship native HTTP and skip the sidecar.

## Validation

The closed set of accepted protocol values is enforced at config-load
time:

```
unsupported traceEmitter.protocol "http/json" (allowed: "", grpc, http/protobuf)
```

A typo surfaces at boot, not as a silent "no exporter could be
created" log line at the first export. HTTP/JSON is intentionally not
supported — Grafana Cloud and the other managed APMs we target prefer
binary protobuf, and adding the JSON variant would double the surface
area without operator demand to justify it.

Carrying `protocol` or `headers` on a `jsonl` emitter is also
rejected — those fields only have meaning for `type: "otel"`, and a
leftover from a migration should fail loudly rather than silently
keep the wrong config working.

`headers` requires `protocol: http/protobuf`. The gRPC exporter path
calls `WithInsecure()` unconditionally, so any bearer or Basic
credential supplied via `headers` would ride a plaintext gRPC channel
to the collector. To make this footgun unreachable, the validator
rejects `headers` whenever `protocol` is `""` (which defaults to gRPC
at exporter construction) or `"grpc"`. The gRPC path is the
local/sidecar collector flow and assumes the collector itself does
not require auth; if you need auth on a gRPC link, run a sidecar
collector that handles the credential locally and forwards over a
trusted boundary, or switch to `http/protobuf`.

## Out of scope

- **gRPC ↔ HTTP fallback retry.** A misconfigured deployment should
  fail loudly. There is one transport per run.
- **HTTP/JSON encoding.** Binary protobuf only.
- **TLS client certificate / mTLS.** Use Bearer/Basic via the
  `headers` map; mTLS support is a separate request.
- **OTLP Logs export.** The `otlploghttp` exporter is gated on issue
  #96 (logs export) landing first; the same protocol-switch shape
  will apply.
