# Observability platform assessment (October 2026)

> **Status note (2026-10-13).** This is a point-in-time assessment. Sections 1 to 5 and 7
> describe Stirrup as it stood at `main` `6ab1b41a` on 2026-10-12 and are not updated as the
> code changes: statements such as "Stirrup emits no `gen_ai.client.*` metrics", "all spans
> are INTERNAL", "log records carry no trace or span id" or the span names `run`, `turn[N]` and
> `tool.<name>` were true of that commit and are corrected by the changes below. Operator
> guidance lives in [`docs/observability-cloud.md`](../docs/observability-cloud.md); section
> 6.5 maps each recommendation to what shipped.
>
> Implemented by the six-link pull request stack that followed the assessment:
>
> 1. `fix/otel-genai-content-conformance` (#768): tool results under the `response` key,
>    `gen_ai.tool.call.id` on every tool span, the per-turn tool-call count, and a Basic-auth
>    scrubber that no longer redacts prose.
> 2. `feat/otel-semconv-metrics` (#769): the GenAI client metrics, model and provider labels on
>    the token counters, the stable `deployment.environment.name` attribute, and `.duration`
>    names for the histograms that carried an `_ms` suffix.
> 3. `feat/otel-live-genai-spans` (#770): live `invoke_agent`, `chat` (CLIENT kind) and
>    `execute_tool` spans, sub-agent runs nested under the spawning tool call, `error.type`
>    on failures, and log records correlated with the run span (X5): every record during a run
>    carries `trace_id` and `span_id`, and the loop's per-request and per-tool records link to
>    their own `chat` or `execute_tool` span.
> 4. `feat/otlp-exporter-unification`: one OTLP exporter package for all three signals
>    (including OTLP/HTTP logs), `observability.serviceName`, `traceEmitter.metricsTemporality`
>    and `traceEmitter.captureContentMaxBytes`. The same package validates endpoints when the
>    RunConfig loads, uses a per-signal URL as given instead of appending the signal segment
>    twice, lets an explicit `https://` endpoint decide TLS even when
>    `OTEL_EXPORTER_OTLP_INSECURE` is set, and dials a plaintext local collector when log export
>    has no endpoint anywhere.
> 5. `feat/otel-platform-profiles`: the `traceEmitter.platform` configuration surface and the
>    `cloudwatch` (in-process SigV4) and `phoenix` (OpenInference attributes) profiles. A
>    profile's headers and signing apply only to signals sent to the profile's own endpoint,
>    and the AWS credentials are checked once at start.
> 6. `docs/observability-platforms-2026-10`: this document and the rewritten operator
>    recipes.
>
> Deferred:
>
> - A Datadog profile, pending an account trial (#771). Datadog is documented as a recipe only.
> - The Grafana Cloud generation export API (#772).
> - Datadog cache-token mapping and OpenLIT compatibility, which have no tracking issue; the
>   reasons are in section 6.4.
> - The `semantic-conventions-genai` renames, until that repository publishes a tagged release
>   (#774).
> - Smaller span and content follow-ups (#775), and the code scanner's raw Basic-auth pattern
>   (#773).

Assessment of how Stirrup's OpenTelemetry output renders in five observability platforms,
what each platform needs beyond it, and which changes to Stirrup are worth making. Written
against `main` at `6ab1b41a`, dated 2026-10-12. Operator setup recipes stay in
[`docs/observability-cloud.md`](../docs/observability-cloud.md); this document is the
evidence base behind proposed changes to that page and to the emitter.

Evidence labels: **VERIFIED** (executed or observed in a live run, API response or captured
file), **SOURCE** (read from vendor or specification source at a named commit), **DOC-ONLY**
(vendor documentation, not exercised), **INFERRED** (reasoning from the above).

## 1. Summary

| Platform | Verdict | Evidence |
|---|---|---|
| Langfuse | Works today. One code fix: session id on every span. | Live, v4.57.0 |
| Arize Phoenix | Works today. A collector adds span kinds, root I/O and users. One code fix: tool results. | Live, 20.21.1 |
| Grafana OSS (Tempo, Prometheus, Loki) | Works today for traces, metrics and logs. | Live |
| Grafana Cloud GenAI products | Needs code: semconv client metrics. Conversations needs a non-OTLP API. | Docs and source |
| Amazon CloudWatch | Works with a SigV4 sidecar collector or in-process signing. Sessions, model edge and agent identity need code. Console views not viewed. | Live, stored data |
| Datadog | Works today by configuration. Never run against an account. | Docs and source |

Most of the value is vendor-neutral conformance with the OpenTelemetry GenAI conventions. Every
platform types spans from `gen_ai.operation.name` alone, so the worst defects are specification
deviations: tool results under the wrong key, missing `error.type`, session id only on the root,
uncorrelated logs, duplicate tool spans and no semconv client metrics. Genuinely vendor-specific
needs: SigV4 signing for CloudWatch traces, the Datadog routing header with delta metrics,
Phoenix span kinds and root `input.value`, Langfuse time-to-first-token, and Grafana's non-OTLP
generation export API. Except signing, first-token time and the Grafana API, a collector covers each.

## 2. Method and verification status

Runs used Stirrup built from `main` (`6ab1b41a`) against AWS Bedrock with
`global.anthropic.claude-haiku-4-5-20251001-v1:0`, in planning mode with one read-only tool call
over two turns. Haiku 5.5 and GPT-6 Luna returned 403 "not available for this account" and were
not retried. All work took place on 2026-10-12.

| Platform | Level | What ran |
|---|---|---|
| Phoenix 20.21.1 | Live | gRPC and HTTP, with and without `captureContent`, auth on a second instance, collector `transform`, GraphQL read-back, screenshots |
| Langfuse v4.57.0 | Live, self-hosted | Normal, tool-error and provider-error runs; ClickHouse and v2 API read-back; Langfuse's own I/O normaliser on stored data; collector `transform`. No browser inspection. |
| Collector Contrib 0.162.0, Prometheus, Tempo, Loki | Live | Full dump of traces, metrics, logs; name and label translation; Tempo truncation |
| CloudWatch | Live, stored data | Traces, logs, metrics via a sidecar collector with `sigv4authextension`; traces via an in-process signing transport; Logs Insights and Application Signals read-back across seven variants. GenAI console views not viewed. |
| Datadog | Doc-only | Vendor docs and open-source code reading; unauthenticated host probes only |
| Grafana Cloud | Doc-only | Docs, dashboard JSON and SDK source; no credentials, nothing sent to a Cloud stack |
| OTel GenAI conventions | Source | Specification repositories cloned and diffed; three OTLP captures audited |

Not verified anywhere: browser rendering beyond API and screenshot evidence (including the CloudWatch
GenAI console), sub-agent rendering on any platform, cache and reasoning tokens from a real provider, and every Cloud-hosted service.

## 3. Per-platform findings

### 3.1 Langfuse

**Ingest path.** `POST {host}/api/public/otel/v1/traces`, HTTP/protobuf or JSON; gRPC is not
supported (DOC-ONLY). `endpoint` is the base `http://host/api/public/otel` because Stirrup appends
the signal path. Auth is `Authorization: Basic base64(pk:sk)` via `headers` (VERIFIED).

- The metrics route returns 200 and discards the body (VERIFIED), so omitting `metricsEndpoint`
  is sensible.
- `x-langfuse-ingestion-version: 4` is documented as required for real-time visibility. Self-hosted
  v4.57.0 returned data within 2 s with and without it (VERIFIED); Cloud is untested.
- On v4 the public `traces` and v1 `observations` query APIs return 404; the v2 observations API
  answers (VERIFIED).
- On first boot the worker can start before migrations finish, leaving the default price list
  unloaded and cost empty until the worker restarts (VERIFIED in worker logs).

**What current emission renders as** (VERIFIED, several runs):

- 16 to 23 observations per run: one AGENT (`run`), a GENERATION per turn, TOOL for
  `execute_tool`, generic SPAN for every component span. The tree is flat, and each tool call
  appears twice (TOOL and `tool.<name>` SPAN).
- Usage, I/O, environment, version and error levels populate. Cost populates once the default
  price list is loaded; the `global.` model prefix matches the managed Haiku 4.5 definition.
  Cache and reasoning keys map correctly (VERIFIED with injected values).
- A failed run shows status message `error` on `run` and `turn[0]`; the provider error text is
  only on `provider.stream`.
- Langfuse's `normalizeSpanIO` parses the parts format into system, user, tool-call and
  tool-result messages (parser output VERIFIED, browser SOURCE). The June finding that parts
  render as raw JSON is stale.
- Not populated: user, tags, release, trace name, time to first token, session on child spans.

| Need | Unlocks | Emit how | Collector alternative |
|---|---|---|---|
| `gen_ai.conversation.id` on every span | Session filter (v4 filters per observation; 1 of 20 matched) | Stamp in-process | No: a processor cannot read the parent |
| `langfuse.user.id`, `langfuse.trace.tags`, `.trace.name`, `.release`, `.trace.metadata.*` | User, tags, names, release, filterable metadata | Span attributes, operator-supplied | Yes, VERIFIED |
| `langfuse.observation.type` on component spans | `guard.*` GUARDRAIL, `verifier.verify` EVALUATOR, rest CHAIN | Span attribute by name | Yes, VERIFIED |
| `langfuse.observation.completion_start_time` | Time-to-first-token column | Provider adapter timing | No |
| `gen_ai.usage.cost` | Cost for unpriced models | Emit | Custom model definition instead |

**Limits.** Body size is set by `LANGFUSE_OTEL_INGESTION_MAX_BODY_BYTES` (SOURCE). Reasoning tokens
are subtracted from output but not costed if the model definition has no reasoning price
(VERIFIED for Haiku 4.5 only).

### 3.2 Grafana

**Ingest path.** Cloud: `https://otlp-gateway-<zone>.grafana.net/otlp`, HTTP/protobuf,
`Basic base64(instanceId:glc_token)` with `traces:write`, `metrics:write`, `logs:write` (DOC-ONLY);
gRPC is not documented. Limits (DOC-ONLY): span attribute values 2048 bytes, truncated; metric and
resource attribute values 2048 bytes, 40 attributes; 5 MB per trace; log line 256 KB.

The local OSS stack (VERIFIED) took Stirrup over gRPC through a collector into Prometheus, Tempo
and Loki.

- Prometheus names: `stirrup_harness_tokens_input_total`,
  `stirrup_harness_turn_duration_milliseconds_*`, and repeated suffixes such as
  `stirrup_guard_duration_ms_milliseconds_*`. Labels are `job`, `instance` (random per run),
  `deployment_environment`, `service_version`. Token counters have no model or provider label.
  Cloud names are INFERRED from the same translation rules.
- Loki receives logs with resource attributes as labels; records carry no trace or span id.

**What current emission renders as.** Three distinct consumers:

1. *AI Observability (OpenLIT-based).* 28 of 30 GenAI queries filter on
   `telemetry_sdk_name="openlit"` and read `gen_ai.client.*` plus OpenLIT-defined cost and agent
   metrics (SOURCE, `grafana-aio11y-app`). Stirrup renders nothing.
2. *Agent Observability (newer).* Conversations, catalog, evaluation and guards come from a
   vendor-specific generation export API (`/api/v1/generations:export`), not OTLP. Analytics and
   cost need `gen_ai.client.operation.duration` and `gen_ai.client.token.usage`; cost is derived
   only from the token metric (DOC-ONLY and SDK source). Stirrup renders nothing.
3. *OSS Tempo, Mimir, Loki.* Everything is stored and queryable by TraceQL, PromQL and LogQL
   (VERIFIED). Tempo truncates attribute values at 2048 bytes by default (VERIFIED):
   `gen_ai.input.messages` was cut mid-JSON and a 4459-byte tool result to 2048.

| Need | Unlocks | Emit how | Collector alternative |
|---|---|---|---|
| `gen_ai.client.operation.duration` and `gen_ai.client.token.usage` with model, provider, operation, token type, `error.type` | Agent Observability analytics and cost; any semconv-metric consumer | Provider call sites | No for the dimensions |
| Model and provider on existing token counters | Splitting dashboards by model | Add attributes | No |
| Generation export API | Conversations, catalog, evaluation | New non-OTLP exporter | No |
| `telemetry.sdk.name=openlit` | OpenLIT dashboards only | Rewrite | Technically; spoofing, and the cost and agent metrics are absent anyway |
| Larger attribute values | Readable `captureContent` | Tempo `max_attribute_bytes` | Self-hosted only |
| OTLP/HTTP logs | Direct logs to Cloud | `otlploghttp` | Yes: collector or Alloy |

Collector recipes should use `otlp_http`; `otlphttp` is a deprecated alias in 0.162.0.

### 3.3 Arize Phoenix

**Ingest path** (VERIFIED unless marked):

| Item | Finding |
|---|---|
| gRPC | `:4317`, traces only. Works with `protocol=grpc`. |
| HTTP | `POST /v1/traces` on `:6006`, protobuf only (JSON gets 415). `endpoint` is the base URL. |
| Metrics and logs | `/v1/metrics` and `/v1/logs` return 405. Stirrup logs nothing when metrics go there. |
| Project routing | `x-project-name` header (HTTP only), else resource attribute `openinference.project.name`, else `default`. `OTEL_RESOURCE_ATTRIBUTES` works with no Stirrup change. |
| Auth, self-hosted | `Authorization: Bearer <key>` works; `api_key` and `x-api-key` returned 401 on 20.21.1. A bad token gives a loud `ForceFlush failed ... 401` warning, exit 0. |
| gRPC with auth | Not possible: the gRPC path is plaintext and `headers` requires `http/protobuf` (SOURCE, `types/runconfig.go`). |
| Phoenix Cloud | Endpoint and header name unverified; sources conflict (DOC-ONLY, one search). |

**Semconv support.** Phoenix 15.10.0 and later converts `gen_ai.*` to OpenInference at ingest
(SOURCE, `trace/gen_ai/conversion.py`), reading parts-format attributes and filling only keys the
span lacks. Verified: `invoke_agent` to AGENT, `chat` to LLM, `execute_tool` to TOOL; token counts,
model, provider, `gen_ai.conversation.id` as `session.id`, messages, finish reasons, tool name,
arguments and result. Cost is computed server-side from model name and tokens (the test run matched
7221 x $1/M + 125 x $5/M exactly). Reasoning tokens and `gen_ai.agent.id` are not read (SOURCE).

**What current emission renders as** (VERIFIED):

- `run` (AGENT) has 18 direct children; `turn[N]` are LLM leaves; the 14 component spans show as
  UNKNOWN; `tool.<name>` is a sibling of `execute_tool`.
- The tool-result message in the LLM view is empty, because Stirrup writes `result` where the
  schema and Phoenix use `response`.
- The root span has no Input/Output; user, tags and metadata are empty.
- Without `captureContent`, kinds, tokens and cost are identical; messages and tool arguments are
  absent. gRPC and HTTP give identical spans. Sessions need `--name`, and Phoenix keys sessions on
  the id alone across projects: a trace in a second project joined the first project's session.

All rows below ran through an unmodified collector `transform` and were read back (VERIFIED).

| Need | Unlocks | Emit how | Collector alternative |
|---|---|---|---|
| `openinference.span.kind` on component spans | Icons and filters instead of 14 UNKNOWN spans | By name: `GUARDRAIL` for `guard.*`, `EVALUATOR` for `verifier.verify`, else `CHAIN` | Yes |
| `input.value` / `output.value` on root | Trace-table columns, root Info panel | First text part of messages; needs `captureContent` | Yes (first text part only) |
| `response` key in tool-result parts | Tool output in the LLM view | Fix in Stirrup | Regex rewrite worked; a fix is cleaner |
| `user.id`, `metadata`, `tag.tags` | User filter, metadata panel, tags | Span attributes | Yes (tag display not inspected) |
| Project name | One project per deployment | Resource attribute or header | Env var, header or resource transform |

Not collector-fixable: the flat tree. Not tested: cache tokens, sub-agents, error-span rendering.

### 3.4 Amazon CloudWatch generative AI observability

**Ingest path.** HTTP only; gRPC is not supported (DOC-ONLY).

| Signal | URL | SigV4 service | Bearer token |
|---|---|---|---|
| Traces | `https://xray.<region>.amazonaws.com/v1/traces` | `xray` | No |
| Logs | `https://logs.<region>.amazonaws.com/v1/logs` | `logs` | Yes; needs `x-aws-log-group` and `x-aws-log-stream` |
| Metrics | `https://monitoring.<region>.amazonaws.com/v1/metrics` | `monitoring` | Yes (API key) |

Verified against the live endpoints:

- Unsigned requests return 403. A bearer header on traces returns `IncompleteSignatureException`,
  so Stirrup's static `headers` cannot authenticate traces. On logs and metrics it is evaluated as
  an API key.
- A request signed for the wrong service is rejected. A correctly signed request passes
  authentication, then returns 400 until Transaction Search sets CloudWatch Logs as the trace
  segment destination. Signed empty payloads to `monitoring` and `logs` return 200.

Transaction Search was enabled with user approval (destination `CloudWatchLogs`, indexing 100
percent, logs resource policy); the destination went from PENDING to ACTIVE in about 6 minutes
(VERIFIED). The setting is account-wide per region and changes trace handling for every X-Ray user
in the account (DOC-ONLY).

Documented limits (DOC-ONLY): 5 MB and 10,000 spans per request, 200 KB per span, 16 KB per resource
plus scope. **Observed (VERIFIED, one sender, us-east-1, uncompressed):** attribute sizes of 150 KB,
250 KB and 600 KB were stored whole. At about 1.2 MB and 5.5 MB (request over 5 MB) the span was
accepted with no exporter error and the attribute was truncated at about 1 MiB, leaving invalid JSON;
sibling spans were stored normally. The documented limits were not enforced as written.

**Semconv support.** The Application Signals `aws/spans` store enriches every span server-side
(VERIFIED by Logs Insights read-back of unmodified Stirrup output with INTERNAL kinds and no AWS
attributes):

| Stored field | Derived from | Note |
|---|---|---|
| `aws.genai.span_kind` AGENT / LLM / TOOL | `gen_ai.operation.name` `invoke_agent` / `chat` / `execute_tool` | Component spans stay unclassified |
| `aws.genai.token_count_total` | `gen_ai.usage.input_tokens` + `output_tokens` | 3564 + 67 stored as 3631 |
| Environment | `deployment.environment` and `deployment.environment.name`, both accepted | |
| `GenAI::Model` remote resource | `gen_ai.request.model` | Only when the chat span is CLIENT kind |

- Token usage and model name are read from span attributes. No `gen_ai.client.token.usage` metric is
  involved, and Stirrup emits none.
- Content stays on spans in the parts format, matching ADOT's documented opt-out mode.
- Failed tool calls store `status.code=ERROR`, `error.type` and the message (which contained a local
  filesystem path).
- Stirrup log records arrived as OTLP log events in the log group, but none carried a trace id (0 of
  14).
- The AWS statement that the ADOT Collector is unsupported outside AgentCore (DOC-ONLY) did not stop
  data being stored and enriched through an upstream collector or directly.

**What does not work from current emission** (VERIFIED):

- *Sessions.* `gen_ai.conversation.id` is not derived into `session.id`. A `session.id` span attribute
  is retained and a sessions-and-traces aggregation by it worked, so Stirrup would need to stamp it.
- *Model dependency edge.* Only CLIENT-kind `chat` spans produce the `GenAI::Model` edge.
- *Service registration.* Application Signals registered a service when a CLIENT span was present or
  `aws.service.type=gen_ai_agent` with `aws.log.group.names` was set. An INTERNAL-only variant with
  neither did not register after more than 8 minutes. The unmodified `stirrup` service is not
  separable from a later variant with the same name, so unmodified registration is likely absent.
- *Agent identity.* `service.name` is always `stirrup`.
- *Logs.* No trace correlation, and the logs endpoint is HTTP-only while Stirrup's export is gRPC.

The GenAI console views (Agents, Sessions, Traces) were not viewed, because no browser was available
and no API exposes them. What was verified is the stored data those views are built from. Docs state
Agent-view metrics are "derived from sampled spans" (DOC-ONLY).

| Need | Status | Emit how | Collector alternative |
|---|---|---|---|
| SigV4 per signal | Required to ingest | In-process `RoundTripper`, or sidecar; both VERIFIED end to end | `sigv4authextension` |
| `gen_ai.operation.name`, `gen_ai.usage.*`, `gen_ai.request.model` | Required, already emitted | n/a | n/a |
| CLIENT kind on `chat` | Needed for the model edge and for registration without AWS attributes | Emitter | Yes: OTTL, VERIFIED |
| `session.id` on every span | Needed for sessions | Stamp in-process | Partial: a per-run constant worked |
| Distinct `service.name` | Without it all deployments merge into one service | Configurable name | Yes: `resource` processor |
| `aws.service.type`, `aws.log.group.names` | Not needed for enrichment or tokens; sufficient to register a service; console effect unknown | Resource attributes | Yes |
| Content size cap | Not needed to avoid rejection; truncation at about 1 MiB breaks JSON and span bytes are billed | Content cap | Weak |
| OTLP/HTTP logs | Needed only to ship logs | `otlploghttp` | Yes: collector bridge, VERIFIED |

Both signing routes ingested traces; the sidecar also ingested logs, and metrics were accepted but
not read back (the CLI has no PromQL query). The in-process transport is about 60 lines using the
already-present `aws-sdk-go-v2` v4 signer and `WithHTTPClient` on the OTel HTTP exporters. It re-signs
per retry, buffers the body for hashing and needs a different service name per signal. The sidecar
needs a flush handshake for short-lived jobs. The CloudWatch agent route is DOC-ONLY and untested.
One shared `headers` map cannot carry a trace credential and a metrics API key together.

**Cost** (VERIFIED via the AWS Pricing API, us-east-1): span ingestion $0.35/GB for the first
10 TB; indexed spans $0.75 per million, first 1 percent free. `captureContent` multiplies span bytes
because every turn resends history. In a two-turn, one-tool run 16 of 20 spans were component spans.

**Verification.** Executed: real OTLP capture, endpoint and SigV4 probes, sidecar route for traces,
logs and metrics, in-process route for traces, Logs Insights read-back of `aws/spans`, and
Application Signals service, dependency and metric listing across seven variants. Not done: console
views, operations tab content (`list-service-operations` returned none), metrics read-back, the
CloudWatch agent route, and other regions. Transaction Search remains enabled in the sandbox, as do
two test log groups created for the runs.

### 3.5 Datadog

Evidence is DOC-ONLY plus open-source code reading. The conversion from OTel GenAI to Agent
Observability runs server-side and is closed source, so rendering claims rest on Datadog's docs.

**Ingest path** (docs fetched 2026-10-12):

- Direct intake: `https://otlp.<site>/v1/traces` and `/v1/metrics`, HTTP protobuf or JSON;
  "`grpc` is not supported". Auth is `dd-api-key`. Logs are HTTP only, so Stirrup's gRPC log export
  cannot go direct.
- Optional headers: `compute_stats`, `dd-otel-span-mapping`, `dd-otel-metric-config`, and
  `dd-otlp-source: llmobs` to route to Agent Observability. Limits: traces 15 MiB, metrics
  512 KiB compressed, logs 5.1 MiB.
- The Agent accepts OTLP on 4317 and 4318 (off by default) and is the only path that takes
  Stirrup's gRPC logs. A collector exporter is also supported.
- `endpoint` must be the bare host; a pasted full URL yields `/v1/traces/v1/traces` (INFERRED).
- The docs warn the Go exporter logs a false failure on 202; the pinned v1.46.0 exporters treat any
  2xx as success (SOURCE).

**Semconv support.** Agent Observability reads OTel GenAI 1.37 and later, OpenInference, OpenLLMetry
and Langfuse OTel. Messages come from `gen_ai.input.messages`, `gen_ai.output.messages` and
`gen_ai.system_instructions`; kind from `gen_ai.operation.name`. A trace is dropped unless some span
carries a recognised marker attribute, and spans with no `gen_ai.*` attribute are dropped.
`service.name` becomes `ml_app`. Cache and reasoning token keys appear only in the OpenInference and
Langfuse mapping tables, not the GenAI one.

**What current emission renders as** (INFERRED): an agent span `run`, llm spans `turn[N]`, tool
spans per call. Component spans leave Agent Observability but stay in APM, where `service` is
`stirrup`, `env` is read from either environment key, and every attribute becomes a tag. Likely
degraded: cost on cache-heavy runs, `ml_app` fixed at `stirrup`, tool output text if the mapper
requires `response`, and message JSON if the Agent's 25,000-character meta limit applies before
conversion (unknown on the direct intake).

**Conversion switch (unresolved).** On 2026-10-05 the docs changed from "disable conversion with
`dd_llmobs_enabled=false`" to "enable conversion with `dd_llmobs_enabled=true`", while another page
still calls conversion automatic. The safe course is to set both the header and the resource
attribute and confirm in a trial.

| Need | Unlocks | Emit how | Collector alternative |
|---|---|---|---|
| `dd-otlp-source: llmobs` | Routing to Agent Observability (direct) | `headers`, but shared by all signals | Exporter `headers` (not shown in docs) |
| `dd_llmobs_enabled=true` | Conversion per the 2026-10-05 docs | `OTEL_RESOURCE_ATTRIBUTES` today | `resource` processor |
| Delta temporality | Any metric on the direct intake (cumulative is rejected) | `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`, honoured by the pinned exporter (SOURCE) | `cumulativetodelta` |
| Configurable `service.name` | Separate apps (`ml_app`) | Code change | `resource` processor |
| Cache-token mapping | Cost accuracy | Shim keys in another vendor's namespace | OTTL, untested |
| `gen_ai.tool.definitions` | Tool-schema panel | New opt-in attribute | No |

**Metrics.** Names pass through verbatim (no `_total`); delta sums become counts; histograms are
stored natively since 2026-09-01; non-conventional resource attributes need `dd-otel-metric-config`.

**Cost.** Agent Observability bills LLM spans only: free tier 40K a month, Pro from $160 a month with
100K included (pricing page). Component spans count toward APM. `stirrup.*` metrics become billable
custom metrics.

## 4. Cross-platform findings

Every defect or gap found, deduplicated. "Raised by" uses the five research tracks: P (Phoenix),
D (Datadog), C (CloudWatch), LG (Langfuse and Grafana), S (semconv audit). N is vendor-neutral, V
vendor-specific. Effort: S under a day, M a few days, L a week or more.

| ID | Finding | Raised by | N/V | Effort | Value |
|---|---|---|---|---|---|
| X1 | `tool_call_response` parts use key `result`; the schema requires `response` (`harness/internal/trace/otel_content.go`). Phoenix tool results render empty (VERIFIED); Datadog impact INFERRED; Langfuse's parser yields a tool-result message from the current shape, content display unconfirmed. | P, D, S | N | S | High |
| X2 | Log scrubber turns prose "basic documentation" into `[REDACTED]` in captured content (observed; cause inferred: `Basic\s+...` in `security/logscrubber.go`). | P | N | S | Medium |
| X3 | `turn.tool_calls` is always 0; no production code assigns `TurnTrace.ToolCalls` (VERIFIED in captures and a re-read of the call sites). | S | N | S | Low |
| X4 | No `error.type` on failed `chat` and `run` spans; status message is the literal `error`; exception event type is `*errors.errorString`. | S, LG | N | S | High |
| X5 | Exported log records never carry trace or span id (0 of 7 and 0 of 15); `docs/observability-cloud.md` claims they do. Loop log calls pass no span context. | LG, S | N | S to M | High |
| X6 | All spans are INTERNAL; the specification says `chat` SHOULD be CLIENT. CloudWatch Application Signals needs CLIENT chat spans for the `GenAI::Model` edge and for service registration without AWS attributes (VERIFIED). No other platform was verified to depend on it. | C, LG, S | N | S | Medium |
| X7 | Duplicate tool spans: live `tool.<name>` plus retrospective `execute_tool <name>`. | S, LG, P (observed) | N | M | Medium |
| X8 | `turn[N]`, tool and `provider.stream` spans are siblings under `run`; the LLM span is a leaf with 18 siblings. Names `run` and `turn[N]` deviate from `invoke_agent <name>` and `chat <model>`. | P, LG, S, D (names) | N | M | Medium |
| X9 | Sub-agent spans flatten onto the parent root with no child `invoke_agent` or run id (VERIFIED). Issue #89 was closed as stale without a fix. | S, D, LG (June) | N | M | Medium |
| X10 | `gen_ai.conversation.id` is on the root only, and only with `--name`. Langfuse v4 filters per observation (VERIFIED). CloudWatch does not derive `session.id` from it, so sessions stay empty until `session.id` is stamped on every span (VERIFIED). | LG, C, P, D, S | N | S | High |
| X11 | No `gen_ai.client.*` metrics. Only Grafana's Agent Observability needs them (disagreement 1). | LG, S | N | M | Medium; High for Grafana |
| X12 | Token counters have no model or provider attributes. | LG, S | N | S | Medium |
| X13 | `service.name` is hard-coded to `stirrup` and overrides `OTEL_SERVICE_NAME`; agents collapse in Datadog (`ml_app`) and CloudWatch. | D, C, S | N | S (env) to M (field) | High |
| X14 | Resource emits deprecated `deployment.environment`. `docs/architecture.md` calls it "the legacy stable key"; it was deprecated in v1.28.0, and `deployment.environment.name` is stable since v1.41.0. Datadog, ADOT and Langfuse read both (Langfuse prefers `.name`). | S, D, C, LG | N | S | Low to Medium |
| X15 | Log export is OTLP/gRPC only; CloudWatch, Datadog direct and Grafana Cloud document HTTP only. A collector bridges it. | D, C, LG, S | N | M | Low |
| X16 | Metrics temporality is cumulative; Datadog's direct intake needs delta. Works today through the environment variable. | D | V | S (docs) | Medium |
| X17 | Content attributes exceed backend limits: Grafana Cloud and default Tempo 2048 B (Tempo VERIFIED), Datadog Agent 25,000 chars (SOURCE, intake unknown), CloudWatch documents 200 KB per span but was observed to accept 5.5 MB and truncate at about 1 MiB (VERIFIED). Truncation cuts JSON mid-string. | LG, D, C | N | M | High for Grafana; Medium elsewhere |
| X18 | CloudWatch traces require SigV4; static headers cannot authenticate (VERIFIED). Both a sidecar and an in-process signing transport ingested traces. | C | V | M | Medium |
| X19 | `docs/observability-cloud.md` lists the Datadog host as `trace.agent.datadoghq.com`; current docs use `otlp.<site>`. | D | N (docs) | S | Medium |
| X20 | Docs `base64` examples omit `tr -d '\n'`; Grafana's docs say the trailing newline invalidates the header. GNU wrapping of long tokens is general knowledge, not tested. | LG | N (docs) | S | Medium |
| X21 | GenAI conventions moved to `semantic-conventions-genai`, which has no release. Recommended floor: released v1.41. | LG, S | N | S | Medium |
| X22 | One `headers` map goes to all signals, so per-signal credentials and routing headers cannot be separated; gRPC carries no headers. | D, C, P | N | S (docs), M (code) | Medium |
| X23 | Absent optional attributes: `gen_ai.agent.name`, `gen_ai.tool.call.id` without capture, `gen_ai.tool.type`, request parameters, `gen_ai.response.id`, `server.address`, time to first chunk, `gen_ai.tool.definitions`; deprecated `http.status_code`. | S, D | N | S to M | Low to Medium |
| X24 | Component spans lack `gen_ai.*` attributes: UNKNOWN in Phoenix (14 per run), generic SPAN in Langfuse, dropped from Datadog Agent Observability. | P, LG, D | V | S (collector) | Low |
| X25 | Prometheus naming repeats unit suffixes (`..._ms_milliseconds`); random `service.instance.id` creates new series per job. | LG | N | S | Low |
| X26 | Datadog cost may be overstated on cache-heavy runs; cache keys are absent from the GenAI mapping table (INFERRED). | D | V | unknown | Unknown until trial |
| X27 | Operator docs gaps: bare-host `endpoint` rule (P, D); Phoenix, CloudWatch and Datadog recipes; Langfuse v4 notes (v1 query APIs 404, metrics discarded, ingestion header, worker race); `otlp_http` over `otlphttp`; Phoenix auth needs HTTP. | P, D, C, LG | N (docs) | S | High |

### Disagreements and unresolved points

1. **Value of semconv client metrics (X11).** The semconv audit rated them High because "every
   platform's dashboard reads these". The platform reports show one consumer. Grafana Agent
   Observability reads `gen_ai.client.operation.duration` and `gen_ai.client.token.usage` (v1.41
   names, which settles the audit's open question on names for Grafana). Datadog derives its
   metrics from spans, CloudWatch computes token totals from span attributes (VERIFIED), and Phoenix and Langfuse ingest no
   metrics. Rated Medium overall, High for Grafana.
2. **CLIENT span kind (X6).** CloudWatch needs it for the model edge and unaided service
   registration (VERIFIED), and the audit favours it as specification-correct. Langfuse, Phoenix
   and Datadog type spans by operation name. Datadog treats CLIENT spans as measured under one
   Agent setting, an effect not studied.
3. **Effect of X1 on Langfuse.** The Phoenix report predicted improvement; the Langfuse report shows
   the parser already yields a tool-result message. Unresolved.
4. **OpenInference mode.** The audit judged it needs a translation layer. Phoenix converts
   `gen_ai.*` natively at ingest, so Phoenix needs none.
5. **Datadog conversion switch.** Datadog's own pages disagree (section 3.5).
6. **Instrument count.** Reports counted 19 and 21 `stirrup.*` instruments; "about 20" is used here.

## 5. The semconv situation

- **Where the specification lives.** Since core semconv v1.42.0 (2026-06-12) every `gen_ai.*`
  attribute, metric and span is marked deprecated and moved to
  `open-telemetry/semantic-conventions-genai`. That repository has no tag, no schema URL and 11
  pending breaking changes (SOURCE). All GenAI conventions are Development stability.
- **Go packages.** otel-go v1.46.0 (Stirrup's pin) and v1.47.0 ship semconv packages up to v1.43.0,
  but only v1.40.0 and v1.41.0 contain GenAI symbols, and no package exists for the new repository.
  Stirrup already uses its own string constants for GenAI keys, which needs no change.
- **What to target.** Released v1.41 is the compatibility floor. Platform mappings in October 2026
  still key off v1.41 names: OpenLIT, Grafana dashboards, Phoenix's converter and Datadog (1.37
  and later).
- **What to defer until a tagged release.** Renaming `gen_ai.usage.cache_creation.input_tokens` to
  `cache_write`, moving metrics to `gen_ai.client.inference.*` (replacing the token histogram and
  `gen_ai.token.type`), dropping output-message `finish_reason`, and adopting the schema URL, all in
  one change.
- **Safe now (additive).** `gen_ai.request.reasoning.level`, `gen_ai.conversation.id` and
  `gen_ai.agent.name` on `execute_tool`, and the `gen_ai.invoke_agent.*` and
  `gen_ai.execute_tool.duration` metrics. The advisory top bucket of `invoke_agent.duration` (about
  7 minutes) is shorter than many unattended runs.

## 6. Recommendation

### 6.1 Ordered changes (vendor-neutral first)

1. **Specification fixes, each small.** X1 (rename to `response`; always emit
   `gen_ai.tool.call.id`), X4, X3, X2, X5 (inject the run's span context in the logging handler and
   correct the doc), X10 (stamp the session id on every span), X13 (honour `OTEL_SERVICE_NAME`
   first; a config field needs the proto and `grpc_translate.go` mirror), X14 (emit
   `deployment.environment.name` only, with a changelog line, and correct the architecture note),
   and the interim half of X6 (set CLIENT kind on the `chat` span), which CloudWatch needs for the
   model edge and unaided service registration.
2. **Documentation batch, no code.** X19, X20, X27 and the platform recipes: Phoenix, CloudWatch
   sidecar with `sigv4authextension`, Datadog (host, delta variable, `dd_llmobs_enabled`), Langfuse
   v4 notes, the Grafana 2048-byte caveat. Verified collector `transform` snippets for Phoenix and
   Langfuse exist and replace the "needs an unspecified rewrite" wording.
3. **Span shape, one coordinated change (M).** X7, X8, X6, X9: one live `execute_tool` span per
   call, a live `chat <model>` CLIENT span with `provider.stream` attempts as children, and a child
   `invoke_agent` for sub-agents. These share a constraint: content capture buffers turn spans. An
   interim step (item 1) sets CLIENT on the retrospective span and renames `turn[N]`.
4. **Content size cap (M).** X17: a configurable limit on message attributes that truncates on a
   message boundary so the JSON stays valid. Needs a `RunConfig` field with the proto mirror. Required
   for Grafana's 2048-byte limit; a nice-to-have on CloudWatch, which accepted far larger spans.
5. **Semconv client metrics (M).** X11 and X12: v1.41 `gen_ai.client.token.usage`,
   `gen_ai.client.operation.duration` and `gen_ai.client.operation.time_to_first_chunk` in seconds
   with advisory buckets, in one file, plus model and provider labels on the existing counters.
6. **Optional attributes (S to M).** X23 and `gen_ai.tool.definitions` behind capture.
7. **Later.** X15 and X22 while a collector covers both; X25.

Synthesizer observation (not examined in the reports): in the OTel Go SDK, `SpanProcessor.OnEnd`
receives a read-only span. Enrichment that depends on values known only at span end, such as the
Phoenix root `input.value` or attributes set by retrospective turn and tool spans, would need an
exporter wrapper or placement inside the emitter. This should be confirmed before the design below
is fixed.

### 6.2 Design principle for vendor-specific extensions

The evidence shows that nearly everything platform-specific is reachable with a collector, so a
platform selector is a convenience. In-process SigV4 is the one case that removes a deployment
component. If built:

- A single `traceEmitter.platform` selector, validated in `types/runconfig.go`, with the proto field
  and `grpc_translate.go` mapping shipped together.
- One package of profiles. A profile may set resource attributes, default endpoints and headers,
  span-attribute enrichment, an HTTP transport, metrics temporality and a content cap. Nothing
  vendor-named appears outside this package.
- Off unless selected; with no selector, output stays pure semconv.
- Credentials remain `secret://` references or the default cloud credential chain, never `RunConfig`
  values. Transports use explicit timeouts and never `http.DefaultClient`.

### 6.3 Contents of each profile

This table is the proposal. Only the `cloudwatch` and `phoenix` profiles shipped, and the
Phoenix profile does not set `user.id` or tags; section 6.5 records the shipped contents.

| Platform | Resource attributes | Endpoint and headers | Span enrichment | Transport | Metrics | Content cap |
|---|---|---|---|---|---|---|
| CloudWatch | `aws.service.type=gen_ai_agent`, `aws.log.group.names` (VERIFIED to register a service; console effect unknown) | Region to the three endpoints; optional `x-aws-log-group` | None; CLIENT kind and `session.id` belong in the neutral emitter | SigV4 per signal (traces VERIFIED in-process; metrics and logs via sidecar) | Native OTLP, service `monitoring` (accepted, not read back) | Optional: 200 KB documented, not enforced |
| Datadog | `dd_llmobs_enabled=true` | Site to `https://otlp.<site>`; `dd-api-key`; `dd-otlp-source: llmobs` on traces only | None | None | Delta | 25 KB (unconfirmed on intake) |
| Phoenix | `openinference.project.name` | Base URL; Bearer; no metrics endpoint | `openinference.span.kind` by name; root `input.value` and `output.value`; optional `user.id` | None | None | None |
| Langfuse | None | Basic auth; `x-langfuse-ingestion-version: 4`; no metrics endpoint | `langfuse.observation.type` for `guard.*` and verifier spans only | None | None | None |
| Grafana | None | None | None | None | None | Guidance only: 2048 B on Cloud |

### 6.4 Deferred, and why

- **Grafana generation export API.** Non-OTLP, Cloud-only, effort L, value uncertain; it is unknown
  whether Cloud reads generations from OTel spans.
- **Datadog cache-token shim.** Needs another vendor's attribute namespace and a live trial to show
  it merges with GenAI spans.
- **OpenLIT compatibility.** Needs `telemetry.sdk.name` spoofing and cost and agent metrics Stirrup
  does not produce. `gen_ai.usage.cost` is the one extension more than one consumer reads, but
  Stirrup has no price table.
- **Semconv-genai renames.** Wait for a tagged release (section 5).
- **OpenInference translation layer.** Not needed for Phoenix.
- **Langfuse time-to-first-token.** Needs provider timing plumbed to the span; low value alone.
- **Host and process detectors, MCP spans, reasoning text parts.** Lower value; host detectors must
  never include command arguments.

### 6.5 What shipped

Status as of 2026-10-13, checked against the code on `feat/otel-platform-profiles` and the
commit history of the stack. Link numbers refer to the status note at the top.

| Recommendation | Status |
|---|---|
| X1: `response` key, `gen_ai.tool.call.id` always | Implemented, link 1. |
| X2: Basic-auth scrubber redacting prose | Implemented, link 1. A payload with no colon is not recognised as a credential. The code scanner still uses the raw pattern (#773). |
| X3: `turn.tool_calls` always 0 | Implemented, link 1. |
| X4: `error.type` on failures | Implemented, link 3. |
| X5: log records carrying trace and span id | Implemented, link 3. Every record during a run carries the ids of at least the run's root span; per-request and per-tool records carry their own `chat` or `execute_tool` span. Records before the first run span, records of a run with no span-recording emitter, and security event lines carry none. The operator page lists which span each record kind links to. |
| X6, X7, X8, X9: span shape | Implemented, link 3: CLIENT `chat` spans, one live `execute_tool` span per call, `chat` and `execute_tool` as siblings under `invoke_agent`, sub-agent `invoke_agent` spans under the spawning tool call. |
| X10: session id on every span | Implemented, link 3, as both `gen_ai.conversation.id` and `session.id`. |
| X11, X12: client metrics and model labels | Implemented, link 2. |
| X13: configurable service name | Implemented, link 4: `observability.serviceName`, then `OTEL_SERVICE_NAME`, then `stirrup`. |
| X14: `deployment.environment.name` | Implemented, link 2; the deprecated key is no longer emitted. |
| X15, X22: HTTP logs, per-signal headers | HTTP logs implemented, link 4. Per-signal headers exist only as profile defaults; `traceEmitter.headers` is still one map for all signals. |
| X16: delta temporality | Implemented, link 4: `traceEmitter.metricsTemporality`. |
| X17: content cap | Implemented, link 4: `traceEmitter.captureContentMaxBytes`. |
| X18: CloudWatch SigV4 | Implemented, link 5: the `cloudwatch` profile signs all three signals in-process, as long as each goes to its CloudWatch host. Regions outside the `aws` and `aws-us-gov` partitions are rejected. |
| X19, X20, X27: documentation gaps | Addressed by link 6. |
| X21: semconv floor | Not changed; renames wait for a tagged release (#774). |
| X23: optional attributes | Partly implemented: `gen_ai.agent.name`, `gen_ai.tool.type`, `server.address`, `server.port`, `gen_ai.request.max_tokens` and `gen_ai.request.temperature`. `gen_ai.response.id` and `gen_ai.tool.definitions` are not emitted. |
| X24: component spans without `gen_ai.*` | Addressed for Phoenix only, through the `phoenix` profile's `openinference.span.kind`. |
| X25, X26 | Not addressed. |

Profiles. `cloudwatch` shipped with the three per-signal endpoints, SigV4 per signal, the log
group and stream headers on the logs signal, and the resource attributes `aws.service.type` and
`aws.log.group.names`. `phoenix` shipped with `openinference.span.kind` on every span
(`GUARDRAIL` for `guard.*`, `EVALUATOR` for `verifier.verify`, `CHAIN` for other component
spans), root `input.value`, `output.value` and `metadata`, and the project name; `user.id` and
`tag.tags` were left out (#775). No Datadog, Langfuse or Grafana profile shipped. Their recipes
are collector-free configurations of the neutral knobs, documented in
`docs/observability-cloud.md`.

## 7. Open questions needing a live account

**Datadog trial (free tier, 40K LLM spans a month):**

1. Is `dd_llmobs_enabled=true` required, or is `dd-otlp-source: llmobs` sufficient, or are GenAI
   spans converted unconditionally?
2. Does a collector `otlp_http` exporter honour `dd-otlp-source`, and does the metrics intake
   tolerate it?
3. Does Datadog read `result` or only `response` for tool output?
4. Are cache and reasoning token attributes used for cost despite the mapping table?
5. Is the 25,000-character truncation applied before conversion on the direct intake?
6. Do `aws.bedrock` and `gcp.vertex_ai` resolve to priced providers?
7. Do non-`gen_ai` attributes become tags (the docs contradict themselves)?
8. Does Error Tracking group error spans that lack `error.stack`?

**Grafana Cloud:** gateway host and auth on a real stack, label promotion defaults, whether the Agent
Observability backend reads generations from OTel spans, and how it renders `gen_ai.client.*`
metrics from Stirrup.

**Phoenix Cloud:** the real endpoint and header name; the self-hosted server rejected `api_key` and
accepted Bearer only.

**CloudWatch:** a browser session on the GenAI console, using the stored runs, to record which
widgets populate, whether `aws.service.type` or `aws.log.group.names` change the Agents view, what the
Operations tab shows (the API listed none), and whether native OTLP metrics render.

**Langfuse Cloud and UI:** whether omitting the ingestion-version header still delays a new project,
and a browser check of chat rendering, sessions and the tree.

## 8. Sources

All accessed 2026-10-12.

- **Langfuse:** https://langfuse.com/integrations/native/opentelemetry (and `/migration-to-v4`),
  https://langfuse.com/docs/v4; https://github.com/langfuse/langfuse tag v4.57.0 (`5d866bbe`):
  `OtelIngestionProcessor.ts`, `ObservationTypeMapper.ts`, `attributes.ts`, OTLP route handlers,
  `normalized-io/`, `default-model-prices.json`.
- **Grafana:** https://grafana.com/docs/grafana-cloud/observe-and-act/send-data/otlp/,
  `.../monitor-applications/ai-observability/` and the AI Observability integration reference 1.2.0,
  `.../agent-observability/` (introduction, get-started, generation-ingest-api, dashboards,
  coding-agents); https://github.com/grafana/grafana-aio11y-app `9256a82`;
  https://github.com/grafana/agento11y `50ea72da`.
- **Phoenix:** https://github.com/Arize-ai/phoenix `4db1a7f3` (`trace/gen_ai/conversion.py`,
  `trace/otel.py`, `routers/v1/traces.py`, `grpc_server.py`, `utilities/project.py`, cost manifest,
  authentication, environments and resource docs, 2026-05-15 semconv conversion note); release
  `arize-phoenix-v20.21.1`;
  https://arize.com/docs/phoenix/tracing/how-to-tracing/setup-tracing/setup-using-phoenix-otel;
  one web search for the Cloud endpoint (results unopened).
- **CloudWatch:** https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/ (OTLPEndpoint,
  OTLPGettingStarted, OTLPSimplesetup, OTLP-MetricsBearerTokenAuth, Enable-TransactionSearch,
  Transaction-Search, GenAI-observability, agent-view, model-invocations, coding-agents-insights);
  https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/ (observability-configure,
  -get-started, -telemetry); the AWS Machine Learning blog on AgentCore Observability for
  on-premises and multi-cloud agents; https://aws.amazon.com/cloudwatch/pricing/ and the AWS Pricing
  API; https://github.com/aws-observability/aws-otel-python-instrumentation `8de42ec`.
- **Datadog:** https://docs.datadoghq.com/ (as `.md`): `llm_observability/instrument/otel_instrumentation`,
  `llm_observability/monitoring/cost`, `llm_observability/instrument/agent_observability_and_apm`,
  `opentelemetry/setup/{agentless,otlp_ingest_in_the_agent,collector_exporter}`,
  `opentelemetry/setup/otlp_ingest/{traces,metrics,logs,serverless,managed_platforms}`,
  `opentelemetry/mapping/semantic_mapping`, `opentelemetry/compatibility`,
  `metrics/open_telemetry/otlp_metric_types`, `opentelemetry/guide/otlp_delta_temporality`;
  https://www.datadoghq.com/pricing/?product=agent-observability;
  https://github.com/DataDog/documentation `a1a3902ae9` (PRs #40272, #39854);
  https://github.com/DataDog/datadog-agent (`pkg/trace/semantics`, `pkg/trace/transform`,
  `pkg/trace/agent/truncator.go`, metrics translator); collector-contrib `exporter/datadogexporter`;
  https://github.com/pydantic/genai-prices.
- **OpenTelemetry and related:** https://github.com/open-telemetry/semantic-conventions (`440d10a8`,
  tags v1.37.0 to v1.44.0, GenAI move PR #3696);
  https://github.com/open-telemetry/semantic-conventions-genai (`5293effb`, no tags);
  https://github.com/open-telemetry/opentelemetry-go v1.46.0 and v1.47.0;
  https://github.com/Arize-ai/openinference `spec/semantic_conventions.md` `53b7a0e2`;
  https://github.com/openlit/openlit `f806ffb7` and release openlit-2.1.0; Stirrup issue #89.
- **Stirrup code and docs read:** `harness/internal/trace/{otel,otel_content,nested_jsonl}.go`,
  `harness/internal/observability/{resource,metrics,logexport,logger}.go`,
  `harness/internal/core/{factory,loop,turnrequest,subagent,dispatch}.go`,
  `harness/internal/security/logscrubber.go`, `types/runconfig.go`, `docs/observability-cloud.md`,
  `docs/architecture.md`.
