# Shipping Stirrup telemetry to a managed APM

Stirrup's OTel exporters speak both **OTLP/gRPC** (the default) and
**OTLP/HTTP with binary protobuf** (issue #100). The HTTP path is what
unlocks first-class native support for Grafana Cloud, Honeycomb, GCP
Cloud Trace, Datadog's OTLP intake, Langfuse, and the long tail of
managed APMs that reject gRPC at the edge.

This document is the operator walkthrough. For the underlying signal
model — what spans, metrics, and resource attributes Stirrup emits —
see [`safety-rings.md`](safety-rings.md) and the
"OpenTelemetry trace emitter" section of the repo-root `CLAUDE.md`.

For operators inspecting the JSONL trace emitter's output (offline
pretty-print, follow-tail, aggregate stats, JSON-path filtering),
see [`trace-inspection.md`](trace-inspection.md).

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

The full authenticated path is expressible without a `--config` file:

```sh
export GRAFANA_CLOUD_AUTH="Basic ..."

stirrup harness \
  --prompt "..." \
  --trace-emitter otel \
  --otel-protocol http/protobuf \
  --otel-endpoint https://otlp-gateway-prod-us-east-0.grafana.net/otlp \
  --otel-header Authorization=secret://GRAFANA_CLOUD_AUTH
```

`--otel-header` is repeatable (`key=value`; the value keeps everything
after the first `=`, so base64 padding survives) and mirrors
`traceEmitter.headers`, including the `secret://` resolution above —
the flag carries references, never raw secrets. When passed
explicitly it replaces the entire `headers` map from `--config`
rather than merging, matching the `--query-param` override semantics.
`--otel-endpoint` accepts a full URL when the protocol is
`http/protobuf`.

As an alternative to the flag, the OTel SDK reads the standard
`OTEL_EXPORTER_OTLP_HEADERS` env var (format
`key1=value1,key2=value2`) when no headers are configured on the
RunConfig — useful for injecting credentials from the environment
without touching the invocation. Two caveats: the env var carries the
resolved value, not a `secret://` reference, so it bypasses the
SecretStore entirely — if the SDK logs an export error that includes
request headers, the resolved credential appears in those logs
(stirrup's scrub layer covers harness output, not SDK-internal
logging). It also bypasses the validator's headers-require-HTTP check
(the SDK reads it regardless of protocol), so a credential set this
way can ride the plaintext gRPC path that `--otel-header` refuses.
Prefer `--otel-header` where both are available.

## Other managed APMs

| Vendor | endpoint | header |
|---|---|---|
| Honeycomb | `https://api.honeycomb.io` | `x-honeycomb-team: secret://HC_API_KEY` |
| GCP Cloud Trace (via gateway) | gateway URL | `Authorization: secret://GCP_BEARER` |
| Datadog OTLP intake | `https://trace.agent.datadoghq.com` | `dd-api-key: secret://DD_API_KEY` |
| Langfuse (cloud or self-hosted) | `https://cloud.langfuse.com/api/public/otel` | `Authorization: secret://LANGFUSE_AUTH` |

Stirrup does not vendor-detect — `protocol`, `endpoint`, and `headers`
are sufficient for any OTLP/HTTP-protobuf gateway.

## Langfuse

Langfuse is an LLM-observability backend that ingests OTLP natively
and reads the GenAI semantic-convention attributes Stirrup emits — no
vendor-specific configuration exists or is needed. Self-hosted
requires v3.22.0+.

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "http://localhost:3000/api/public/otel",
    "metricsEndpoint": "http://localhost:3000/api/public/otel",
    "headers": {
      "Authorization": "secret://LANGFUSE_AUTH"
    }
  }
}
```

where the secret resolves to Basic auth over the project's API key
pair:

```sh
export LANGFUSE_AUTH="Basic $(printf '%s' 'pk-lf-...:sk-lf-...' | base64)"
```

**`protocol: "http/protobuf"` is required.** Langfuse's OTLP ingest is
HTTP-only; on the default gRPC path the spans are silently dropped.
The per-signal URL appending (below) lands exactly on Langfuse's
`/api/public/otel/v1/traces` and `/v1/metrics` routes.

How Stirrup's signal model surfaces in Langfuse:

- **Observation typing.** The root `run` span
  (`gen_ai.operation.name: invoke_agent`) renders as an agent
  observation, `turn[N]` spans (`chat`) as generations, and
  `execute_tool <name>` spans as tool observations.
- **Input/output panels** populate only with
  [`captureContent`](#span-content-capture-opt-in): the root span's
  run-level I/O feeds the trace-level input/output views, turn spans
  feed per-generation I/O, and tool spans feed per-tool
  arguments/results. Without the toggle, traces show structure,
  usage, model, and timing with empty content panels.
- **Cost.** Generations carry `gen_ai.request.model` per turn;
  Langfuse prices models it recognises by name and needs a custom
  model definition for others.
- **Error filtering.** Failed runs, turns, and tool calls carry OTel
  error status, which Langfuse maps to `level: ERROR` with the status
  message — filter traces and observations by level to triage
  failures.
- **Sessions.** `--session-name` is emitted as
  `gen_ai.conversation.id`, which Langfuse maps to its session
  grouping natively.
- **Environments.** `observability.environment` reaches Langfuse via
  the `deployment.environment` resource attribute.

One rendering caveat: Stirrup serialises message content in the
semconv parts schema (`{role, parts: [...]}`), which Langfuse ingests
and displays but does not yet pretty-render as chat bubbles — the
content panels show structured JSON. Backend-native concepts beyond
these (Langfuse users, tags) live in the `langfuse.*` attribute
namespace, which Stirrup deliberately does not emit; operators who
need them can rewrite attributes in a collector
(`transformprocessor`), keeping the vendor mapping in operator
config.

## Log-trace correlation

Log records emitted inside an active span carry `trace_id` and
`span_id` attributes in lowercase snake_case, rather than the
camelCase the rest of the Stirrup log schema uses. This spelling is
the OTel/Loki convention that the Tempo↔Loki "Logs for trace"
correlation derived-field is keyed on; any other casing silently
breaks the correlation in Grafana and other OTel/Loki-aware
backends. IDs are the 32-hex-char trace ID and 16-hex-char span ID
forms. Records emitted outside an active span (boot-time,
context-less logging) carry no correlation fields.

## Span content capture (opt-in)

By default the otel emitter records turn-level counters (tokens,
duration, stop reason) and per-tool spans but **no prompt or
completion content** — an LLM-observability backend (Langfuse, SigNoz,
Datadog LLM Observability, Phoenix) shows correct structure, token
usage, model, and timing, with an empty prompt/IO view.

`traceEmitter.captureContent: true` (CLI: `--otel-capture-content`)
opts the run into recording content using the standard GenAI
semantic-convention attributes those backends read natively:

| Attribute | On | Carries |
|---|---|---|
| `gen_ai.input.messages` | `turn[N]` | The message history the model saw on the turn (JSON, semconv message schema). |
| `gen_ai.output.messages` | `turn[N]` | The model's response blocks, including tool calls, with `finish_reason`. |
| `gen_ai.system_instructions` | `turn[N]` | The run's built system prompt. |
| `gen_ai.input.messages` / `gen_ai.output.messages` | `run` (root) | Run-level I/O — the seed prompt and the final assistant message — feeding backends' trace-level input/output views. |
| `gen_ai.tool.call.id` / `.arguments` / `.result` | `execute_tool <name>` | Each tool call's identifier, input arguments, and result text. |

The toggle is vendor-neutral: no backend detection, no vendor
attribute namespace — any OTLP consumer that understands the GenAI
conventions gets the same view.

Safety properties:

- **Off by default.** The OTel GenAI spec marks message and tool-call
  content Opt-In precisely because it is likely to contain PII. With
  the toggle off, span output is identical to the capture-on output
  minus the content attributes — counters, typing, model, and error
  status are capture-independent.
- **Scrubbed before export.** Content passes through the same
  `security.Scrub` defence-in-depth layer the JSONL emitter applies
  to its `turn_record` lines, so secret-shaped substrings are
  replaced with `[REDACTED]` before any attribute is built.
- **Validated.** `captureContent` is rejected on the `jsonl` and
  `gcs` emitters, like `protocol` and `headers`.

Operational notes: captured attributes can be large (the full message
history is serialised each turn), so size-sensitive collectors may
need span attribute limits configured backend-side. Sub-agent turns
forwarded into the parent's trace are captured with their own
content; sub-agent *system prompts* are not captured. The semconv
GenAI conventions are Development-status — attribute names track the
pinned semconv version (v1.40.0) and may evolve with it.

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

Carrying `protocol`, `headers`, or `captureContent` on a `jsonl`
emitter is also rejected — those fields only have meaning for
`type: "otel"`, and a leftover from a migration should fail loudly
rather than silently keep the wrong config working.

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
- **OTLP/HTTP logs export.** Structured-log export (`--log-export
  otlp`, issue #96) ships OTLP/gRPC only; an `otlploghttp` variant
  following the same protocol-switch shape is a separate request.
