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
URL to end in `/otlp`. The SDK appends `/v1/traces`, `/v1/metrics`,
and `/v1/logs` per signal.

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
    "serviceName": "stirrup",
    "environment": "production",
    "serviceNamespace": "stirrup"
  }
}
```

`observability.serviceName` (CLI: `--service-name`) sets the
`service.name` resource attribute, which backends use to list and
separate services and agents. Every run reports `stirrup` unless it is
set here or through `OTEL_SERVICE_NAME`, so deployments of different
agents merge into one service until each sets its own name. The
explicit field wins over the environment variable. An
`OTEL_SERVICE_NAME` that does not match `^[A-Za-z0-9._-]{1,64}$` is
ignored with a warning naming the variable, and `stirrup` is used
instead. A `service.name` in `OTEL_RESOURCE_ATTRIBUTES` does not
apply, because Stirrup's own resource attributes win over that
variable.

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

- **Observation typing.** `invoke_agent` spans (the root and each
  sub-agent) render as agent observations, `chat {model}` spans as
  generations, and `execute_tool {name}` spans as tool observations;
  Langfuse types them from `gen_ai.operation.name`. See
  [Span tree](#span-tree).
- **Input/output panels** populate only with
  [`captureContent`](#span-content-capture-opt-in): the root span's
  run-level I/O feeds the trace-level input/output views, chat spans
  feed per-generation I/O, sub-agent spans carry the sub-agent's own
  prompt and answer, and tool spans feed per-tool arguments/results. Without the toggle, traces show structure,
  usage, model, and timing with empty content panels.
- **Cost.** Generations carry `gen_ai.request.model` per request;
  Langfuse prices models it recognises by name and needs a custom
  model definition for others.
- **Error filtering.** Failed runs, requests, and tool calls carry
  OTel error status and `error.type`, which Langfuse maps to
  `level: ERROR` with the status message — filter traces and
  observations by level to triage failures.
- **Sessions.** `--session-name` is emitted on every span as both
  `gen_ai.conversation.id` and `session.id`, which Langfuse maps to its
  session grouping natively.
- **Environments.** `observability.environment` reaches Langfuse via
  the `deployment.environment.name` resource attribute.

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

When the trace emitter records spans (`traceEmitter.type: "otel"`),
every log record emitted during a run is correlated with a span of
that run's trace. On stderr the record carries `trace_id` and
`span_id` attributes in lowercase snake_case, rather than the
camelCase the rest of the Stirrup log schema uses. This spelling is
the OTel/Loki convention that the Tempo↔Loki "Logs for trace"
correlation derived-field is keyed on; any other casing silently
breaks the correlation in Grafana and other OTel/Loki-aware
backends. IDs are the 32-hex-char trace ID and 16-hex-char span ID
forms. Records exported with `--log-export otlp`
(`observability.logsExport`) carry the same IDs in the OTLP log
record's native trace and span ID fields, which Grafana, Datadog, and
CloudWatch key their log-to-trace links on.

The span a record correlates with depends on what it describes:

| Records | Correlated span |
|---|---|
| One provider request: `turn completed`, `provider turn retry`, `provider stream failed`, the context-overflow retry and its exhaustion, an empty or refused stop | that request's `chat {model}` span |
| One tool call: `tool dispatched`, a recovered async tool panic | that call's `execute_tool {name}` span |
| A sub-agent spawn warning | the spawning `execute_tool spawn_agent` span |
| The rest of a sub-agent run's loop records (`run started`, `turn started`, `run finished`, …) | the sub-agent's `invoke_agent stirrup.subagent` span |
| Every other record during a run: the rest of the loop's records, and component records (provider adapters, executors, MCP clients, hooks) logged without a span of their own, including those from components a sub-agent shares with its parent | the run's root `invoke_agent stirrup` span |

Records emitted before the first run's span opens (boot-time and
component construction), and every record of a run whose trace emitter
records no spans, carry no correlation fields. Between follow-up runs,
records keep the previous run's `runId` and root span. Security event
lines are written by a separate writer and carry `runId` but no
correlation fields.

## Span tree

A run with one tool turn, a sub-agent spawn, and a final answer
exports this tree (kind is INTERNAL unless stated):

```
invoke_agent stirrup                       root span
├─ context.prepare, guard.pre_turn         per turn
├─ chat {model}                CLIENT      one per provider request
│  └─ provider.stream                      one per attempt; retries add more
├─ execute_tool read_file                  one per tool call
│  ├─ guard.pre_tool
│  └─ permission.check
├─ execute_tool spawn_agent
│  └─ invoke_agent stirrup.subagent        the sub-agent run
│     ├─ chat {model}          CLIENT
│     │  └─ provider.stream
│     └─ execute_tool {name}
├─ git.checkpoint
├─ chat {model}                CLIENT      next turn
├─ guard.post_turn
└─ verifier.verify, git.finalise, hooks.postRun, workspace.handoff
```

The structure follows the GenAI agent conventions: `chat` and
`execute_tool` spans are siblings under their `invoke_agent` span, and
a `chat` span covers inference only, so its duration is model latency.
A request re-sent after a context-overflow compaction has different
input and gets its own `chat` span; a transient-error retry stays
inside one, as another `provider.stream` child. `turn.number` on
both `chat` and `execute_tool` spans reconstructs the turns: a tool
span carries the number of the request whose response made the call.
With content capture on, `gen_ai.tool.call.id` also links a tool span
to the `tool_call` part of that request's output.

Every span in the run carries:

| Attribute | Value |
|---|---|
| `gen_ai.agent.name` | `stirrup`, or `stirrup.subagent` for spans inside a sub-agent run. |
| `gen_ai.conversation.id`, `session.id` | The `--session-name`, when one is set. |

Span-specific attributes:

| Span | Attributes |
|---|---|
| `invoke_agent` | `gen_ai.operation.name=invoke_agent`, `run.id`, `run.mode`, `gen_ai.provider.name`, `gen_ai.request.model`, `run.outcome`, `run.turns`; sub-agent spans add `run.parent_id`. |
| `chat {model}` | `gen_ai.operation.name=chat`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.max_tokens`, `gen_ai.request.temperature` when the run config sets `temperature` (an adapter that omits sampling parameters for the model does not send it), `server.address` and `server.port` when the adapter knows its host, `turn.number`; at the end, usage (below), `gen_ai.response.finish_reasons`, `gen_ai.response.model`, `turn.tool_calls`, `turn.retries`. |
| `execute_tool {name}` | `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`, `gen_ai.tool.call.id`, `gen_ai.tool.type=function`, `turn.number`, `tool.input_size`, `tool.output_size`, `tool.success`, `tool.duration_ms`, and `tool.queue_ms` when the call waited to run. A control-plane tool call's span opens at dispatch but the call runs only after the turn's synchronous tool calls have run and a parallel slot is free; `tool.queue_ms` is that wait, which the span covers and `tool.duration_ms` excludes. A tool name that resolves to no registered tool is model-controlled, so its span is named bare `execute_tool` and its `gen_ai.tool.name` is scrubbed and cut to 128 bytes. |

Failures set error status with a scrubbed description and an
`error.type` from a bounded vocabulary: the run outcome on
`invoke_agent` spans, except that the generic `error` outcome carries
the `error.type` of the run's last failed `chat` request when there is
one, and the status description is the scrubbed run error;
`provider_request_failed`,
`provider_stream_failed`, `context_overflow`, `cancelled`, or
`timeout` on `chat` and `provider.stream` spans; the tool failure
category on `execute_tool` spans. The `context_overflow`, `cancelled`
and `timeout` values are span-only: the
`gen_ai.client.operation.duration` metric labels the same failed
request `provider_request_failed` or `provider_stream_failed`, so its
label set stays the failure categories. Exception events carry the Go
type of the underlying error in `exception.type`.

## Token usage on chat spans

Every `chat` span that completed carries `gen_ai.usage.input_tokens`
and `gen_ai.usage.output_tokens`. A failed request carries them only
when usage was counted before the failure, and never carries
`gen_ai.response.finish_reasons`: `error.type` names the failure. With
content capture on, a failed request still records the input messages
that were sent, and no output. Input includes cached tokens and is the
provider's figure where the provider reports one, otherwise the
harness's estimate. When set, a chat span also carries:

| Attribute | Meaning |
|---|---|
| `gen_ai.usage.cache_read.input_tokens` | Part of the input served from the provider's prompt cache. |
| `gen_ai.usage.cache_creation.input_tokens` | Part of the input written to the provider's prompt cache. |
| `gen_ai.usage.reasoning.output_tokens` | Part of the output spent on reasoning or thinking. |
| `stirrup.tokens.input_reported` | `true` when the input is the provider's figure rather than the estimate. |

Each is omitted when zero or false. The per-provider sources are
listed in [`trace-inspection.md`](trace-inspection.md#token-usage).
The `stirrup.harness.tokens.cache_read` and `.cache_write` counters
carry the same cache figures as metrics, and
`stirrup.subagent.tokens.cache_read` and `.cache_write` carry a
sub-agent's totals under the parent's `parent.mode`. Each cache
counter records only non-zero figures.

## GenAI client metrics

Alongside the `stirrup.*` instruments, every run exports the
OpenTelemetry GenAI client metrics, which managed APMs read for their
out-of-the-box GenAI dashboards and cost views. The metric names and
attributes follow semantic conventions v1.41, while the Go semconv
package the harness imports is pinned at v1.40.0:

| Metric | Unit | Recorded |
|---|---|---|
| `gen_ai.client.token.usage` | `{token}` | Per provider request attempt, as reported by the provider: `gen_ai.token.type=input` (total input including cached tokens) and `gen_ai.token.type=output`. A count the provider did not report is not recorded, and harness estimates never appear. A failed attempt records the usage it reported before failing. |
| `gen_ai.client.operation.duration` | `s` | Once per provider request attempt. Turn-level retries appear as separate points; HTTP retries inside an adapter (429 and 5xx responses) and their backoff are part of one point's duration. Failed attempts carry `error.type` (`provider_request_failed` or `provider_stream_failed`). |
| `gen_ai.client.operation.time_to_first_chunk` | `s` | Once per streamed request, when the first chunk arrives. |

All three carry `gen_ai.operation.name` (always `chat`),
`gen_ai.provider.name`, `gen_ai.request.model`, and, when known,
`gen_ai.response.model`, `server.address` and `server.port`. The
response model is unknown when the first chunk arrives, so
`time_to_first_chunk` omits it. The server attributes derive from the
provider endpoint; for Bedrock they name the regional Bedrock Runtime
host. Run IDs, conversation
IDs, and tool names are never attached, which keeps the series count
proportional to the configured providers and models. Sub-agent turns
and the auxiliary model calls made outside the agentic loop (the
`summarise` context strategy, the `llm-judge` verifier, and the
`cloud-judge` guard) are inference calls and record the same metrics.

The histograms use the specification's advisory bucket boundaries.
Durations are in seconds, whereas the `stirrup.*` duration histograms
(`stirrup.harness.provider_latency`, `provider_ttfb`, and the rest)
are in milliseconds and are unchanged. The token counters
`stirrup.harness.tokens.*` and `stirrup.subagent.tokens.*` also carry
`gen_ai.provider.name` and `gen_ai.request.model`, so token totals
split by provider and model without the histograms.

Metrics are exported with cumulative temporality unless configured
otherwise. `traceEmitter.metricsTemporality` (CLI:
`--otel-metrics-temporality`) selects `cumulative` or `delta`; when it
is empty, the SDK reads
`OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`. An explicit field
wins over the environment variable. Under `delta`, counters and
histograms report deltas and up-down counters stay cumulative, as the
OpenTelemetry specification defines for that preference.

## Span content capture (opt-in)

By default the otel emitter records per-request counters (tokens,
duration, stop reason) and per-tool spans but **no prompt or
completion content** — an LLM-observability backend (Langfuse, SigNoz,
Datadog LLM Observability, Phoenix) shows correct structure, token
usage, model, and timing, with an empty prompt/IO view.

`traceEmitter.captureContent: true` (CLI: `--otel-capture-content`)
opts the run into recording content using the standard GenAI
semantic-convention attributes those backends read natively:

| Attribute | On | Carries |
|---|---|---|
| `gen_ai.input.messages` | `chat {model}` | The message history the model saw on the request (JSON, semconv message schema). |
| `gen_ai.output.messages` | `chat {model}` | The model's response blocks, including tool calls, with `finish_reason`. |
| `gen_ai.system_instructions` | `chat {model}` | The run's built system prompt; top-level requests only. |
| `gen_ai.input.messages` / `gen_ai.output.messages` | `invoke_agent stirrup` (root) | Run-level I/O — the seed prompt and the final assistant message — feeding backends' trace-level input/output views. |
| `gen_ai.input.messages` / `gen_ai.output.messages` | `invoke_agent stirrup.subagent` | The sub-agent's seed prompt and final assistant message. |
| `gen_ai.tool.call.arguments` / `.result` | `execute_tool {name}` | Each tool call's input arguments and result text. |

The tool call identifier (`gen_ai.tool.call.id`) is a correlation key
rather than content and is emitted on every `execute_tool` span
whether or not capture is on.

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
history is serialised on every request), and backends limit attribute
size. Grafana Cloud Traces (Tempo) keeps at most 2048 bytes of an
attribute value, Datadog truncates attribute values at 25 KB, and
CloudWatch cuts span content at about 1 MiB without regard to JSON
structure, so the truncated value no longer parses. Truncation done by
a backend mid-value leaves invalid JSON that GenAI views cannot render.

`traceEmitter.captureContentMaxBytes` (CLI:
`--otel-capture-content-max-bytes`) caps each content attribute
before export instead. A value longer than the cap is shortened so it
stays valid JSON:

- `gen_ai.input.messages` keeps its newest whole messages.
- `gen_ai.output.messages` and `gen_ai.system_instructions` keep their
  leading whole parts.
- When a single message or part cannot fit, its text is shortened at a
  UTF-8 boundary.
- `gen_ai.tool.call.arguments` becomes a JSON string holding the
  leading bytes of the arguments; `gen_ai.tool.call.result` keeps its
  leading bytes.

Each capped attribute is accompanied by a boolean
`stirrup.<attribute>.truncated` (for example
`stirrup.gen_ai.input.messages.truncated`), so consumers can tell a
shortened value from a complete one. `0`, the default, disables the
cap; a non-zero cap must be at least 256 bytes, and the field is
rejected unless `captureContent` is on. Set it at or below the
backend's per-attribute limit, for example `2048` for Tempo.

Sub-agent requests are captured with their own content; sub-agent
*system prompts* are not captured, so their `chat` spans carry no
`gen_ai.system_instructions`. The semconv
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

Structured-log export (`observability.logsExport.type: otlp`,
`--log-export otlp`) uses the same protocol and headers as traces and
metrics. Its endpoint is `observability.logsExport.endpoint`, falling
back to `traceEmitter.endpoint`, so one `http/protobuf` gateway URL
carries all three signals. A platform profile that defines its own
endpoint for a signal replaces that fallback (see
[Platform profiles](#platform-profiles)). With neither set and no
`OTEL_EXPORTER_OTLP_*` endpoint, insecure, or certificate variable,
logs go in plaintext to the SDK's default local collector
(`localhost:4317` for gRPC, `localhost:4318` for OTLP/HTTP).

## Platform profiles

Some backends need more than an endpoint and a header: Amazon
CloudWatch requires SigV4-signed requests to a different host per
signal, and Arize Phoenix reads OpenInference attributes that the
GenAI semantic conventions do not carry. A platform profile supplies
those settings so the harness can export to the backend directly,
without a collector in between.

`traceEmitter.platform` (CLI: `--otel-platform`) selects the profile
and `traceEmitter.platformOptions` (CLI: repeatable
`--otel-platform-option key=value`) configures it:

```json
"traceEmitter": {
  "type": "otel",
  "platform": "cloudwatch",
  "platformOptions": {"region": "us-east-1", "logGroup": "/stirrup/agents"}
}
```

Both fields require `type: "otel"`. Options are never secrets:
`secret://` values are rejected, and credentials stay in
`traceEmitter.headers` or in the platform's own credential chain.
`ValidateRunConfig` (and so `stirrup run-config`) checks only the
shape of the options. Which keys a platform accepts, whether required
keys are present, and whether the options contradict the rest of the
config are checked when the harness builds its exporters, so the run
fails at start and `stirrup harness --dry-run` reports the problem
under the `trace-emitter` step.

### A profile only fills blanks

A profile never overrides a field the operator set, and never writes
back into the RunConfig: the `Redact()`-ed config stored with a trace
shows only what was declared. Each exporter setting resolves in this
order:

1. The explicit RunConfig field (or CLI flag).
2. A standard OpenTelemetry environment variable, where the exporter
   already honours one: `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` and
   `OTEL_EXPORTER_OTLP_ENDPOINT` for the log endpoint,
   `OTEL_EXPORTER_OTLP_<SIGNAL>_HEADERS` or `OTEL_EXPORTER_OTLP_HEADERS`
   when `traceEmitter.headers` is empty,
   `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`, and
   `OTEL_RESOURCE_ATTRIBUTES`.
3. The profile's default.
4. The SDK default.

Headers merge per key: a profile's header defaults for a signal are
overridden by any header of the same name in `traceEmitter.headers`
(compared case-insensitively). A profile's resource attributes rank
below `OTEL_RESOURCE_ATTRIBUTES` and below stirrup's own identity
attributes (`service.name` and the rest).

**Per-signal endpoints.** Without a profile, `metricsEndpoint` and
`logsExport.endpoint` fall back to `traceEmitter.endpoint`. When a
profile defines its own endpoint for a signal, that signal stops
inheriting `traceEmitter.endpoint`. For example, with the `cloudwatch`
profile an explicit `endpoint` redirects traces only; metrics and logs
still go to their CloudWatch hosts unless `metricsEndpoint` or
`logsExport.endpoint` is set too.

A profile's headers and request signing apply only to signals sent to
the profile's own endpoint. A signal redirected elsewhere, by an
explicit endpoint or, for logs, by `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`
or `OTEL_EXPORTER_OTLP_ENDPOINT`, carries only `traceEmitter.headers`
and is not signed, so the profile's credentials never reach that
collector. A signing client does not follow HTTP redirects.

The harness rejects these combinations at start:

- an option key the platform does not accept, or a missing required
  one;
- `protocol: "grpc"` with a platform that requires `http/protobuf`;
- a platform that signs requests or adds headers, on gRPC (signing
  wraps the OTLP/HTTP transport, which gRPC does not use, and headers
  are refused on gRPC as described under [Validation](#validation));
- an `Authorization` entry in `traceEmitter.headers` with a platform
  that signs requests, because the signature owns that header.

When a platform is selected, the harness logs one
`observability platform exporters resolved` line at start naming the
protocol and the endpoint of each enabled signal. Headers are never
logged.

### `cloudwatch`

Sends traces, metrics and logs to the Amazon CloudWatch OTLP
endpoints, signed with SigV4.

| Option | Required | Meaning |
|---|---|---|
| `region` | no | AWS region of the endpoints. Defaults to the region of the AWS SDK's default configuration (`AWS_REGION`, the shared config profile); with neither, the run fails at start. Only regions in the `aws` and `aws-us-gov` partitions are accepted: China, ISO and European Sovereign Cloud regions are rejected because their endpoints are not under `amazonaws.com`. |
| `logGroup` | yes | Log group the logs signal writes to (`x-aws-log-group`). Also set as the `aws.log.group.names` resource attribute. |
| `logStream` | no | Log stream within `logGroup` (`x-aws-log-stream`). Default `default`. |

The profile sets:

- protocol `http/protobuf` (CloudWatch has no gRPC intake);
- endpoints `https://xray.<region>.amazonaws.com` for traces,
  `https://monitoring.<region>.amazonaws.com` for metrics and
  `https://logs.<region>.amazonaws.com` for logs, each with the
  standard `/v1/<signal>` path appended;
- SigV4 signing of every export attempt for the `xray`, `monitoring`
  and `logs` services, with credentials from the AWS default
  credential chain (environment, shared config and `AWS_PROFILE`, IRSA,
  EKS Pod Identity, ECS, IMDS), the same chain the Bedrock provider
  uses. No key material enters the RunConfig. The harness retrieves
  credentials once while resolving the profile, so missing credentials
  fail the run at start and `--dry-run` reports them;
- the `x-aws-log-group` and `x-aws-log-stream` headers on the logs
  signal only;
- the resource attributes `aws.service.type=gen_ai_agent` and
  `aws.log.group.names=<logGroup>`, which the AWS Distro for
  OpenTelemetry sets for agent observability and which Application
  Signals uses to register the service.

Prerequisites, outside the harness:

- CloudWatch Transaction Search enabled in the account and region,
  with CloudWatch Logs as the trace segment destination. Without it,
  the traces endpoint answers `400 The OTLP API is supported with
  CloudWatch Logs as a Trace Segment Destination`.
- The log group and log stream must already exist. The logs endpoint
  answers `400 The specified log group does not exist` (or log
  stream) and does not create them. Export failures are logged and do
  not fail the run.
- IAM permission for the credential chain's principal to write to
  X-Ray, CloudWatch metrics and CloudWatch Logs.

CloudWatch groups spans into sessions by the `session.id` attribute,
which the harness sets on every span from `sessionName` (`--name`).
Runs without a session name have no session.

### `phoenix`

Adds the OpenInference attributes Arize Phoenix reads beyond what it
derives from the GenAI semantic conventions.

| Option | Required | Meaning |
|---|---|---|
| `project` | no | Phoenix project, set as the `openinference.project.name` resource attribute. Without it, spans land in Phoenix's `default` project unless `OTEL_RESOURCE_ATTRIBUTES` names one. |

At export time the profile adds, to attributes a span does not already
carry:

- `openinference.span.kind`: `AGENT` for `invoke_agent` spans, `LLM`
  for `chat`, `TOOL` for `execute_tool`, `GUARDRAIL` for `guard.*`,
  `EVALUATOR` for `verifier.verify`, and `CHAIN` for every other span;
- on the root span, `input.value` (the first user message's text) and
  `output.value` (the final assistant text), which Phoenix shows as
  the trace's input and output. Both need `captureContent: true`;
- on the root span, `metadata` with the run ID and mode.

The profile sets no endpoint or protocol, so traces go to
`traceEmitter.endpoint`, or to the usual default `localhost:4317` when
it is unset. Phoenix accepts OTLP/gRPC on port 4317 and OTLP/HTTP on
port 6006. An authenticated Phoenix needs `http/protobuf`, because only
that protocol carries `traceEmitter.headers`. Phoenix ingests traces
only; with no `metricsEndpoint`, metrics follow `endpoint` and Phoenix
rejects them with `405`, which the metrics exporter does not report.

## URL appending

The SDK appends per-signal segments to the configured base path:

| Configured | Traces POST | Metrics POST | Logs POST |
|---|---|---|---|
| `https://gateway/otlp` | `https://gateway/otlp/v1/traces` | `https://gateway/otlp/v1/metrics` | `https://gateway/otlp/v1/logs` |
| `https://gateway` | `https://gateway/v1/traces` | `https://gateway/v1/metrics` | `https://gateway/v1/logs` |
| `localhost:4318` | `http://localhost:4318/v1/traces` | `http://localhost:4318/v1/metrics` | `http://localhost:4318/v1/logs` |

A path that already ends in the signal's own segment is used as given,
so a per-signal URL such as
`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://collector:4318/v1/logs` (the
form the OTel specification defines for per-signal variables) posts
to `/v1/logs`, not `/v1/logs/v1/logs`. Any other path is treated as a
base, so a per-signal URL with a non-standard final segment still has
the signal segment appended.

Grafana Cloud expects `/otlp` (the gateway routes per-tenant from
that prefix). Honeycomb expects no extra path; their gateway routes
on the dataset header.

TLS is on by default for `https://` URLs. The exporter falls back to
plaintext only for `http://` URLs or scheme-less endpoints (the
typical local-collector flow). The scheme of a configured endpoint
decides the TLS mode even when the environment says otherwise: an
`https://` URL never silently downgrades, including when a sidecar
injector sets `OTEL_EXPORTER_OTLP_INSECURE=true` or an `http://`
`OTEL_EXPORTER_OTLP_ENDPOINT`. The rule is the same for every signal
and both protocols, so an `https://` gRPC endpoint also dials with
TLS. The scheme is matched case-insensitively. An endpoint with any
other scheme, embedded `user:password@` credentials, a query string,
or a fragment is rejected when the RunConfig is validated (for
`traceEmitter.endpoint`, `traceEmitter.metricsEndpoint`, and
`observability.logsExport.endpoint`) and again at exporter
construction; credentials belong in `traceEmitter.headers`.

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
  (`service.namespace`, `deployment.environment.name`) per route so a
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

`headers` requires `protocol: http/protobuf`. The gRPC exporter dials
the documented `host:port` endpoint form in plaintext, so any bearer
or Basic credential supplied via `headers` would ride a plaintext gRPC
channel to the collector. To make this footgun unreachable, the validator
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
