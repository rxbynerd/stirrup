# Shipping Stirrup telemetry to a managed APM

Stirrup's OTel exporters speak both **OTLP/gRPC** (the default) and
**OTLP/HTTP with binary protobuf** (issue #100). The HTTP path is what
unlocks first-class native support for Grafana Cloud, Honeycomb, GCP
Cloud Trace, Datadog's OTLP intake, Langfuse, Arize Phoenix, and the
long tail of managed APMs that reject gRPC at the edge. Traces,
metrics, and logs all use the selected protocol.

This document is the operator walkthrough. For the underlying signal
model, see the sections on the [span tree](#span-tree),
[GenAI client metrics](#genai-client-metrics), and
[log-trace correlation](#log-trace-correlation) below, and the
[trace emitter section of `architecture.md`](architecture.md#trace-emitters). The
evidence behind the platform recipes, including which statements were
verified against a running backend and which come from vendor
documentation only, is in
[`research/observability-platforms-2026-10.md`](../research/observability-platforms-2026-10.md).

For operators inspecting the JSONL trace emitter's output (offline
pretty-print, follow-tail, aggregate stats, JSON-path filtering),
see [`trace-inspection.md`](trace-inspection.md).

## Quick choice

| Where should telemetry land? | Use |
|---|---|
| Grafana Cloud, Honeycomb, GCP Cloud Trace, any managed OTLP gateway | **Native HTTP** ([Grafana Cloud example](#native-http-grafana-cloud-example)) |
| Langfuse | Native HTTP ([Langfuse](#langfuse)) |
| Arize Phoenix | Native HTTP or gRPC, plus the `phoenix` profile ([Arize Phoenix](#arize-phoenix)) |
| Amazon CloudWatch generative AI observability | The `cloudwatch` profile, which signs requests in process ([Amazon CloudWatch](#amazon-cloudwatch)) |
| Datadog | Native HTTP to the OTLP intake ([Datadog](#datadog)) |
| Local OpenTelemetry Collector, Tempo, or in-cluster collector | gRPC (default) |
| Sampling, fan-out, multi-tenant routing, per-signal credentials, or attribute rewriting beyond what a profile provides | A collector ([When a collector is still the better tool](#when-a-collector-is-still-the-better-tool)) |

The native HTTP path replaces a collector for the simple case. There
is nothing to deploy and no extra failure mode: Stirrup posts
OTLP/protobuf directly to the backend over HTTPS. A
[platform profile](#platform-profiles) (`traceEmitter.platform`)
extends this to backends that need more than an endpoint and a
header, namely request signing and backend-specific span attributes.
Only `cloudwatch` and `phoenix` profiles exist. The other backends in
this document are configured through the neutral settings alone.

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
    },
    "captureContent": true,
    "captureContentMaxBytes": 2048
  },
  "observability": {
    "serviceName": "stirrup",
    "environment": "production",
    "serviceNamespace": "stirrup"
  }
}
```

`captureContent` and `captureContentMaxBytes` are optional. Grafana
Cloud keeps at most 2048 bytes of a span attribute value and truncates
the rest, so the message-history attributes that `captureContent`
records would be cut mid-JSON. A cap of `2048` makes the harness
shorten them at a message boundary instead, and the result stays valid
JSON (see [Span content capture](#span-content-capture-opt-in)). The
2048-byte figure comes from Grafana's documentation and was not
checked against a Cloud stack; a self-hosted Tempo applies the same
default and was observed to truncate.

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
export GRAFANA_CLOUD_AUTH="Basic $(printf '%s' '123456:glc_eyJv...' | base64 | tr -d '\n')"
```

`tr -d '\n'` matters: Grafana documents that a trailing newline
invalidates the header value, and GNU `base64` also wraps long output
at 76 characters. The command strips line breaks on every platform.

The token needs the `traces:write`, `metrics:write`, and `logs:write`
scopes for the signals that are exported.

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

### What Grafana Cloud reads

The harness exports the three signals to the one gateway: spans
(Cloud Traces), metrics (Cloud Metrics), and logs when
`observability.logsExport.type` is `otlp` (Cloud Logs). These points
come from Grafana's documentation and SDK source. No telemetry was
sent to a Cloud stack. A local stack of a collector, Prometheus,
Tempo, and Loki, fed over gRPC, was run, and every signal was stored
and queryable with TraceQL, PromQL, and LogQL.

- **Agent Observability** computes analytics and cost from the
  OpenTelemetry GenAI client metrics. The harness exports
  `gen_ai.client.token.usage`, `gen_ai.client.operation.duration`, and
  `gen_ai.client.operation.time_to_first_chunk` for that purpose (see
  [GenAI client metrics](#genai-client-metrics)). Cost is derived from
  the token metric alone. Its conversation, catalog, and evaluation
  views are fed by a Grafana-specific generation export API rather
  than OTLP, which the harness does not call (see issue #772).
- **Log-to-trace links.** Every record exported to Cloud Logs carries
  the trace and span IDs of the run span it was logged under, in the
  OTLP log record's native fields (see
  [Log-trace correlation](#log-trace-correlation)). A backend that
  joins logs to traces on those IDs, or on the `trace_id` and `span_id`
  attributes of the stderr JSON through a Loki derived field, can open
  a record's trace. Neither join was exercised against a Cloud stack;
  the local stack stored the logs.
- **AI Observability** (the OpenLIT-based integration) filters its
  queries on `telemetry_sdk_name="openlit"`, which the harness does not
  set, so its GenAI dashboards stay empty.
- **Self-hosted Tempo** truncates attribute values at 2048 bytes by
  default; set `captureContentMaxBytes` as described above.

## Other managed APMs

| Vendor | endpoint | header |
|---|---|---|
| Honeycomb | `https://api.honeycomb.io` | `x-honeycomb-team: secret://HC_API_KEY` |
| GCP Cloud Trace (via gateway) | gateway URL | `Authorization: secret://GCP_BEARER` |
| Datadog OTLP intake (HTTP only) | `https://otlp.<site>`, for example `https://otlp.datadoghq.com` | `dd-api-key: secret://DD_API_KEY` (recipe: [Datadog](#datadog)) |
| Langfuse (cloud or self-hosted) | `https://cloud.langfuse.com/api/public/otel` | `Authorization: secret://LANGFUSE_AUTH` (recipe: [Langfuse](#langfuse)) |
| Arize Phoenix | `http://localhost:6006` (HTTP) or `localhost:4317` (gRPC) | `Authorization: secret://PHOENIX_AUTH`, HTTP only (recipe: [Arize Phoenix](#arize-phoenix)) |
| Amazon CloudWatch | set by the `cloudwatch` profile | none; requests are signed (recipe: [Amazon CloudWatch](#amazon-cloudwatch)) |

The endpoint is the base URL, not a per-signal URL. The exporter
appends `/v1/traces`, `/v1/metrics`, or `/v1/logs`. A path that already
ends in the signal's own segment is used as given, so a pasted
`https://otlp.datadoghq.com/v1/traces` is not doubled for traces. The
same endpoint is also the default for metrics and logs, though, and
those signals would post to `.../v1/traces/v1/metrics` (see
[URL appending](#url-appending)).

Without `traceEmitter.platform`, Stirrup does not detect the vendor:
`protocol`, `endpoint`, and `headers` are sufficient for any
OTLP/HTTP-protobuf gateway. The Honeycomb and GCP rows were not run
against an account for this document.

## Langfuse

Langfuse is an LLM-observability backend that ingests OTLP natively
and reads the GenAI semantic-convention attributes Stirrup emits. No
profile exists or is needed for the core views. Self-hosted requires
v3.22.0 or later; the behaviour below was observed on a self-hosted
v4.57.0.

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "http://localhost:3000/api/public/otel",
    "headers": {
      "Authorization": "secret://LANGFUSE_AUTH",
      "x-langfuse-ingestion-version": "4"
    },
    "captureContent": true
  }
}
```

where the secret resolves to Basic auth over the project's API key
pair:

```sh
export LANGFUSE_AUTH="Basic $(printf '%s' 'pk-lf-...:sk-lf-...' | base64 | tr -d '\n')"
```

**`protocol: "http/protobuf"` is required.** Langfuse's OTLP ingest is
HTTP-only; on the default gRPC path the spans are silently dropped.
The per-signal URL appending (below) lands exactly on Langfuse's
`/api/public/otel/v1/traces` route.

Langfuse v4 notes:

- **Metrics are discarded.** Langfuse answers `POST
  /api/public/otel/v1/metrics` with `200` and drops the body. The
  example therefore omits `metricsEndpoint`. Metrics still follow
  `endpoint` when it is set, and no setting turns metric export off, so
  the harness posts them and Langfuse discards them.
- **The ingestion-version header.** Langfuse documents
  `x-langfuse-ingestion-version: 4` as required for real-time
  visibility on v4. A self-hosted v4.57.0 showed data within two
  seconds with and without it; Langfuse Cloud was not tested, so the
  header is kept in the example. `headers` is sent with every
  request, so the header also reaches the metrics route.
- **Query APIs.** On v4 the public `traces` and v1 `observations` query
  APIs return `404`. The v2 observations API answers. Anything that
  reads Langfuse back, such as an evaluation harness, needs the v2
  API.
- **First boot.** A freshly started self-hosted worker can start
  before the database migrations finish, which leaves the default
  model price list unloaded and cost empty until the worker restarts.

How Stirrup's signal model surfaces in Langfuse (checked on stored data
from several runs: a normal run, a tool failure, and a provider
failure):

- **Observation typing.** `invoke_agent` spans (the root and each
  sub-agent) render as agent observations, `chat {model}` spans as
  generations, and `execute_tool {name}` spans as tool observations;
  Langfuse types them from `gen_ai.operation.name`. Component spans
  (`guard.*`, `verifier.verify`, `context.prepare`, and the rest)
  render as generic spans. See [Span tree](#span-tree).
- **Input/output panels** populate only with
  [`captureContent`](#span-content-capture-opt-in): the root span's
  run-level I/O feeds the trace-level input/output views, chat spans
  feed per-generation I/O, sub-agent spans carry the sub-agent's own
  prompt and answer, and tool spans feed per-tool arguments/results.
  Without the toggle, traces show structure, usage, model, and timing
  with empty content panels.
- **Cost.** Generations carry `gen_ai.request.model` per request;
  Langfuse prices models it recognises by name and needs a custom
  model definition for others. A `global.` Bedrock model prefix matched
  Langfuse's managed definition for Claude Haiku 4.5.
- **Error filtering.** Failed runs, requests, and tool calls carry
  OTel error status and `error.type`, which Langfuse maps to
  `level: ERROR` with the status message. Filter traces and
  observations by level to triage failures.
- **Sessions.** `correlation.sessionId`, or `--name` when no session
  ID is set, is emitted on every span as both `gen_ai.conversation.id`
  and `session.id`. Langfuse v4 filters sessions per observation, so
  the id on every span is what makes the session filter match. Without
  either there is no session. Langfuse drops session IDs of 200
  characters or more, which is why validation caps
  `correlation.sessionId` at 199.
- **Users.** `correlation.userId` is emitted on every span as
  `user.id`, which Langfuse reads natively. Without it there is no
  user. See [Correlation](configuration.md#correlation).
- **Environments.** `observability.environment` reaches Langfuse via
  the `deployment.environment.name` resource attribute.
- **Chat rendering.** Stirrup serialises message content in the
  semconv parts schema (`{role, parts: [...]}`). Langfuse's own
  input/output normaliser parses that shape into system, user,
  tool-call, and tool-result messages; that was verified on the
  stored data. How the Langfuse web interface draws those messages
  was not viewed in a browser.

Langfuse reads `user.id` and `session.id` natively, so the run
correlation block alone populates users and sessions
([attribute mapping](https://langfuse.com/integrations/native/opentelemetry)).
The remaining backend-native concepts (tags, trace name, release,
filterable metadata, and observation types) live in the `langfuse.*`
attribute namespace, which Stirrup deliberately does not emit. An
OpenTelemetry Collector `transform` processor sets them while keeping
the vendor mapping in operator config. This configuration
populated the trace name, user, release, tags, filterable metadata,
and the guardrail, evaluator, and chain observation types in a
self-hosted v4.57.0 (collector contrib 0.162.0):

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
processors:
  transform/langfuse:
    error_mode: propagate
    trace_statements:
      - context: span
        statements:
          - set(attributes["langfuse.trace.name"], "stirrup-run")
          - set(attributes["langfuse.user.id"], "user-1")
          - set(attributes["langfuse.release"], resource.attributes["service.version"])
          - set(attributes["langfuse.trace.tags"], ["stirrup", resource.attributes["harness.run.mode"]])
          - set(attributes["langfuse.trace.metadata.run_mode"], resource.attributes["harness.run.mode"])
          - set(attributes["langfuse.observation.type"], "guardrail") where IsMatch(name, "^guard\\.")
          - set(attributes["langfuse.observation.type"], "evaluator") where name == "verifier.verify"
          - set(attributes["langfuse.observation.type"], "chain") where IsMatch(name, "^(context\\.prepare|hooks\\.|git\\.|workspace\\.)")
exporters:
  otlp_http/langfuse:
    endpoint: http://localhost:3000/api/public/otel
    headers:
      Authorization: "${env:LANGFUSE_AUTH}"
      x-langfuse-ingestion-version: "4"
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [transform/langfuse]
      exporters: [otlp_http/langfuse]
```

Stirrup then points at the collector over gRPC (`endpoint:
localhost:4317`, no headers). The assessment ran this file with the
exporter spelled `otlphttp/langfuse`, the deprecated alias, and
against the span shape that preceded the live `chat` and
`execute_tool` spans. The matched span names are unchanged since. The
`otlp_http` spelling was run against another backend, not Langfuse.

**Scoring a run.** A control plane that collects feedback on a run
scores the run span, not the whole trace, so the score stays on the
execution when the trace also holds orchestration or other runs.
Stirrup reports the run span's identity as a trace reference on the
`run_started` event, the `done` summary and `RunResult.traceReference`. The reference's
`traceId` is the Langfuse trace ID and its `runSpanId` is the
observation ID, since Langfuse uses an OTLP span's ID as its
observation ID. A score through the
[Langfuse scores API](https://langfuse.com/docs/evaluation/evaluation-methods/scores-via-sdk)
(`POST /api/public/scores`) then targets the run:

```json
{
  "id": "feedback-8f2e-reviewer-1",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "observationId": "00f067aa0ba902b7",
  "name": "helpfulness",
  "value": 1,
  "dataType": "NUMERIC",
  "comment": "Fixed the failing test on the first attempt."
}
```

A stable `id` makes a retried or edited score update the existing one.
A reference with `sampled: false` names a span that was not exported,
so Langfuse holds no observation for it. Stirrup submits no scores
itself.

## Arize Phoenix

Phoenix ingests OTLP traces and, from version 15.10.0, converts the
`gen_ai.*` attributes to OpenInference at ingest. The behaviour below
was observed on a self-hosted Phoenix 20.21.1 over both gRPC and HTTP,
with and without `captureContent`, and read back through its GraphQL
API.

### Plain recipe

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "http://localhost:6006",
    "captureContent": true
  }
}
```

- **The endpoint is the base URL.** Phoenix serves OTLP/HTTP on port
  `6006` (`POST /v1/traces`, protobuf only) and OTLP/gRPC on port
  `4317`. For gRPC, leave `protocol` empty and set `endpoint` to
  `localhost:4317`; the two protocols produce identical spans.
- **Traces only.** Phoenix answers `/v1/metrics` and `/v1/logs` with
  `405`. With only `endpoint` set, the metrics exporter targets the
  same host and Phoenix rejects the metrics, which the harness does not
  report. Leave `observability.logsExport` off.
- **Authentication needs HTTP.** An authenticated Phoenix accepts
  `Authorization: Bearer <key>`; the `api_key` and `x-api-key` headers
  returned `401` on 20.21.1. Because `traceEmitter.headers` requires
  `http/protobuf`, an authenticated Phoenix cannot be reached over
  gRPC from the harness. A rejected token surfaces as a `ForceFlush
  failed ... 401` warning at the end of the run, and the exit code is
  still `0`. The endpoint and header name of Phoenix Cloud were not
  verified.

  ```json
  "headers": {"Authorization": "secret://PHOENIX_AUTH"}
  ```

  ```sh
  export PHOENIX_AUTH="Bearer ..."
  ```

- **Project routing.** Phoenix files a trace under the first of: the
  `x-project-name` request header (HTTP only), the
  `openinference.project.name` resource attribute, and the `default`
  project. The attribute is settable without the profile through
  `OTEL_RESOURCE_ATTRIBUTES`:

  ```sh
  export OTEL_RESOURCE_ATTRIBUTES="openinference.project.name=stirrup"
  ```

  The `phoenix` profile's `project` option sets the same attribute.
- **Sessions.** Phoenix maps `gen_ai.conversation.id`, which the
  harness sets on every span from `correlation.sessionId` or else
  `--name`, to its session id. A run with neither has no session.
  Phoenix keys a session on the id alone, so a trace in a second
  project that reuses the id joins the first project's session.

Without a profile, Phoenix types `invoke_agent` spans as AGENT, `chat`
as LLM, and `execute_tool` as TOOL, and reads token counts, model,
provider, session id, finish reasons, and tool name from the GenAI
attributes. It computes cost from the model name and token counts.
Messages and tool arguments and results appear only with
`captureContent`; kinds, tokens, and cost do not depend on it.
Phoenix does not read reasoning-token counts or `gen_ai.agent.id`.
The harness's component spans (`guard.*`, `context.prepare`, the
verifier and workspace spans) have no `gen_ai.*` attributes and show
as UNKNOWN, and the root span has no input or output.

### The `phoenix` profile

`traceEmitter.platform: "phoenix"` adds the OpenInference attributes
that Phoenix reads beyond what it derives:

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "http://localhost:6006",
    "captureContent": true,
    "platform": "phoenix",
    "platformOptions": {"project": "stirrup"}
  }
}
```

| Option | Required | Meaning |
|---|---|---|
| `project` | no | Phoenix project, set as the `openinference.project.name` resource attribute. Without it, spans land in the `default` project unless `OTEL_RESOURCE_ATTRIBUTES` names one. |

At export time the profile adds, to each span that does not already
carry the attribute:

- `openinference.span.kind` on every span: `AGENT` for `invoke_agent`,
  `LLM` for `chat`, `TOOL` for `execute_tool`, `GUARDRAIL` for
  `guard.*` spans, `EVALUATOR` for `verifier.verify`, and `CHAIN` for
  every other span;
- on the root span, `input.value` (the text of the first user
  message) and `output.value` (the text of the final assistant
  message), which Phoenix shows as the trace's input and output. Both
  are derived from the captured messages and need
  `captureContent: true`;
- on the root span, `metadata`, a JSON object with the keys `run_id`
  and `run_mode`.

The profile sets no endpoint or protocol, so traces go to
`traceEmitter.endpoint`, or to the default `localhost:4317` over gRPC
when it is unset. It sets no tag attributes, and Phoenix's tag display
stays empty (see issue #775). `correlation.userId` stamps `user.id`,
the OpenInference user attribute, on every span without the profile;
Phoenix's user filter has not been checked against it. A collector
`transform` processor set `user.id`, `metadata`, and tags in the
assessment.

## Amazon CloudWatch

CloudWatch generative AI observability accepts OTLP over HTTP only,
at a different host and SigV4 service name per signal. Static
`headers` cannot authenticate the traces endpoint: a bearer header
there fails with `IncompleteSignatureException`. The `cloudwatch`
profile therefore signs every export request in process, with no
collector in between. Traces were verified end to end through this
path in the assessment against stored data; the console views built on
that data were not viewed.

### Prerequisites

Run these once per account and region. **Transaction Search is
account-wide for the region and changes trace handling for every X-Ray
user in the account**; the change takes up to ten minutes to become
active (about six in the assessment).

**Transaction Search.** Allow X-Ray to write spans to CloudWatch
Logs, then make CloudWatch Logs the trace segment destination:

```sh
REGION=us-east-1
ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"

cat > transaction-search-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "TransactionSearchXRayAccess",
      "Effect": "Allow",
      "Principal": {"Service": "xray.amazonaws.com"},
      "Action": "logs:PutLogEvents",
      "Resource": [
        "arn:aws:logs:${REGION}:${ACCOUNT_ID}:log-group:aws/spans:*",
        "arn:aws:logs:${REGION}:${ACCOUNT_ID}:log-group:/aws/application-signals/data:*"
      ],
      "Condition": {
        "ArnLike": {"aws:SourceArn": "arn:aws:xray:${REGION}:${ACCOUNT_ID}:*"},
        "StringEquals": {"aws:SourceAccount": "${ACCOUNT_ID}"}
      }
    }
  ]
}
EOF

aws logs put-resource-policy --region "$REGION" \
  --policy-name TransactionSearchAccess \
  --policy-document file://transaction-search-policy.json
aws xray update-trace-segment-destination --region "$REGION" \
  --destination CloudWatchLogs
aws xray update-indexing-rule --region "$REGION" --name Default \
  --rule '{"Probabilistic": {"DesiredSamplingPercentage": 1}}'
aws xray get-trace-segment-destination --region "$REGION"
```

The last command reports `"Status": "ACTIVE"` once the destination
is ready. Until then, the traces endpoint answers `400 The OTLP API
is supported with CloudWatch Logs as a Trace Segment Destination`.
The policy follows AWS's documented example. Indexing 1 percent of
spans is the free tier; a higher percentage is billed.

**Log group and stream.** Create the log group and the log stream
that the logs signal writes to. The logs endpoint creates neither: it answers `400 The specified log
group does not exist` (or log stream) until both exist. Only runs that
export logs need them:

```sh
aws logs create-log-group --region "$REGION" --log-group-name /stirrup/agents
aws logs create-log-stream --region "$REGION" \
  --log-group-name /stirrup/agents --log-stream-name default
```

**Permissions.** The credential chain's principal needs permission to
write to X-Ray, CloudWatch metrics, and CloudWatch Logs.

### Recipe

```json
{
  "traceEmitter": {
    "type": "otel",
    "captureContent": true,
    "platform": "cloudwatch",
    "platformOptions": {"region": "us-east-1", "logGroup": "/stirrup/agents"}
  },
  "observability": {
    "serviceName": "triage-agent",
    "logsExport": {"type": "otlp"}
  },
  "sessionName": "nightly-triage"
}
```

The equivalent flags are `--trace-emitter otel --otel-platform
cloudwatch --otel-platform-option region=us-east-1
--otel-platform-option logGroup=/stirrup/agents --service-name
triage-agent --log-export otlp --name nightly-triage`.

| Option | Required | Meaning |
|---|---|---|
| `region` | no | AWS region of the endpoints. Defaults to the region of the AWS SDK's default configuration (`AWS_REGION`, the shared config profile); with neither, the run fails at start. Only regions in the `aws` and `aws-us-gov` partitions are accepted (GovCloud is allowed): China, ISO and European Sovereign Cloud regions are rejected because their endpoints are not under `amazonaws.com`. |
| `logGroup` | yes | Log group the logs signal writes to (`x-aws-log-group`). Also set as the `aws.log.group.names` resource attribute. Required even when log export is off. |
| `logStream` | no | Log stream within `logGroup` (`x-aws-log-stream`). Default `default`. |

The profile sets:

- protocol `http/protobuf` (CloudWatch has no gRPC intake);
- endpoints `https://xray.<region>.amazonaws.com` for traces,
  `https://monitoring.<region>.amazonaws.com` for metrics and
  `https://logs.<region>.amazonaws.com` for logs, each with the
  standard `/v1/<signal>` path appended. Each signal has its own
  endpoint, so an explicit `traceEmitter.endpoint` redirects traces
  only (see [Per-signal endpoints](#a-profile-only-fills-blanks));
- SigV4 signing of every export attempt for the `xray`, `monitoring`,
  and `logs` services, with credentials from the AWS default
  credential chain (environment, shared config and `AWS_PROFILE`, IRSA,
  EKS Pod Identity, ECS, IMDS), the same chain the Bedrock provider
  uses. No key material enters the RunConfig. The harness retrieves
  credentials once while resolving the profile, with a 5 s limit, so
  missing credentials fail the run at start and `--dry-run` reports
  them. When the chain finds none within the limit, the error says so
  and names `AWS_PROFILE`, `AWS_ACCESS_KEY_ID`, and an instance or pod
  role as the ways to supply them. A failing `credential_process` is
  reported without its output, which can contain the secret key;
- the `x-aws-log-group` and `x-aws-log-stream` headers on the logs
  signal only;
- the resource attributes `aws.service.type=gen_ai_agent` and
  `aws.log.group.names=<logGroup>`, which the AWS Distro for
  OpenTelemetry sets for agent observability.

Signing and these headers apply only to a signal sent to its
CloudWatch host. A signal redirected elsewhere (an explicit
`traceEmitter.endpoint`, `metricsEndpoint`, or `logsExport.endpoint`,
or for logs the `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` or
`OTEL_EXPORTER_OTLP_ENDPOINT` variable) is neither signed nor given the
profile's headers, so AWS credentials never reach that collector. The
signing client does not follow HTTP redirects.

Export failures after start are logged and do not fail the run.
Setting an `Authorization` header in `traceEmitter.headers` is
rejected, because the signature owns that header. Selecting the
profile with `protocol: "grpc"` is rejected too: signing wraps the
OTLP/HTTP transport, which gRPC does not use.

What CloudWatch does with the spans, read back from the stored
`aws/spans` data (the console views themselves were not viewed):

- **Typing and tokens.** CloudWatch derives `aws.genai.span_kind`
  (`AGENT`, `LLM`, `TOOL`) from `gen_ai.operation.name`, and the token
  total from `gen_ai.usage.input_tokens` plus `output_tokens`. Token
  usage is read from span attributes, not from a metric. Component
  spans stay unclassified.
- **Model edge.** Only `CLIENT` spans produce the `GenAI::Model`
  dependency. The harness emits `chat` spans as `CLIENT`. That edge was
  verified in the assessment with a `CLIENT` kind applied by a
  collector rewrite to the earlier span shape; the shipped `chat`
  spans were not re-read in CloudWatch.
- **What registers a service.** In the assessment, Application Signals
  registered a service when a `CLIENT` span was present, or when
  `aws.service.type=gen_ai_agent` and `aws.log.group.names` were set.
  A run with neither did not register one. The profile's resource
  attributes and the `CLIENT` `chat` spans each suffice. Whether they
  change the console's Agents view is unknown.
- **Sessions.** CloudWatch does not derive sessions from
  `gen_ai.conversation.id`; it keeps a `session.id` span attribute, by
  which a sessions-and-traces aggregation worked. The harness sets
  `session.id` on every span from `correlation.sessionId`, or else
  `sessionName`. A run with neither has no session.
- **Agent identity.** The service is `service.name`, so deployments
  that share the default `stirrup` name merge into one service. Set
  `observability.serviceName` per agent.
- **Errors.** A failed tool call stores `status.code=ERROR`,
  `error.type`, and the status message.
- **Metrics.** Native OTLP metrics are accepted at the `monitoring`
  endpoint. They were not read back.
- **Logs.** With `observability.logsExport.type: "otlp"`, each record
  goes to the log group and stream named by the profile and carries the
  trace and span IDs of the run span it was logged under (see
  [Log-trace correlation](#log-trace-correlation)). That is the field
  CloudWatch keys its log-to-trace links on, per its documentation.
  Whether the console shows the link was not checked, and the logs
  endpoint answers `400` while the log group or stream does not exist.

**Cost and size.** Span ingestion was priced at $0.35 per GB for the
first 10 TB, and indexed spans at $0.75 per million with the first 1
percent free (AWS Pricing API, `us-east-1`, 2026-10-12; see
<https://aws.amazon.com/cloudwatch/pricing/> for current figures).
`captureContent` multiplies span bytes, because every turn resends the
message history. CloudWatch documents a 200 KB limit per span, yet
accepted attribute values of 600 KB whole, and truncated an attribute
at about 1 MiB without regard to JSON structure. A
`captureContentMaxBytes` cap keeps the content valid JSON and bounds
the billed bytes.

### Sidecar alternative

An OpenTelemetry Collector with the `sigv4authextension` signs the
same requests when a collector is already deployed, or when the
credential chain is unavailable to the harness. The harness exports to
the collector over gRPC and the collector signs the onward request.
This traces configuration ran against the live endpoint (collector
contrib 0.162.0, `us-east-1`):

```yaml
extensions:
  sigv4auth/traces: {region: us-east-1, service: xray}
receivers:
  otlp:
    protocols:
      grpc: {endpoint: 0.0.0.0:4317}
processors:
  batch: {timeout: 2s}
  resource/genai:
    attributes:
      - {key: aws.service.type, value: gen_ai_agent, action: upsert}
      - {key: aws.log.group.names, value: "${env:LOG_GROUP}", action: upsert}
exporters:
  otlp_http/xray:
    traces_endpoint: https://xray.us-east-1.amazonaws.com/v1/traces
    compression: gzip
    auth: {authenticator: sigv4auth/traces}
    retry_on_failure: {enabled: false}
service:
  extensions: [sigv4auth/traces]
  pipelines:
    traces: {receivers: [otlp], processors: [resource/genai, batch], exporters: [otlp_http/xray]}
```

The harness then needs no profile: `endpoint` is the collector
(`localhost:4317`, the default protocol, no headers). Metrics follow
`endpoint` unless `metricsEndpoint` is set, so the collector needs a
metrics pipeline to receive them. A short-lived job
must give the collector time to flush its batch before the container
exits. Logs and metrics also reached CloudWatch through a sidecar, with
`sigv4auth` extensions for the `logs` and `monitoring` services, but
that configuration is not reproduced here. The CloudWatch agent route
was not tested.

## Datadog

**Documentation only.** This recipe was written from Datadog's
documentation and open-source code. It was not run against a Datadog
account, and the profile is deferred until a trial can confirm the open
points below (see issue #771). Datadog's OTLP intake accepts HTTP
only: `grpc` is not supported.

```json
{
  "traceEmitter": {
    "type": "otel",
    "protocol": "http/protobuf",
    "endpoint": "https://otlp.datadoghq.com",
    "metricsTemporality": "delta",
    "headers": {
      "dd-api-key": "secret://DD_API_KEY",
      "dd-otlp-source": "llmobs"
    }
  },
  "observability": {
    "serviceName": "triage-agent",
    "environment": "production"
  }
}
```

```sh
export OTEL_RESOURCE_ATTRIBUTES="dd_llmobs_enabled=true"
```

- **Endpoint.** `https://otlp.<site>` is the intake host, for example
  `otlp.datadoghq.com` (US1) or `otlp.datadoghq.eu` (EU). It is the
  bare host with no path: the harness appends `/v1/traces` and
  `/v1/metrics`. Datadog's documented traces URL, used as
  `endpoint`, is posted as given for traces, but metrics and logs
  inherit the same endpoint and would post to `.../v1/traces/v1/metrics`.
  The intake answers `403` to a missing or wrong `dd-api-key`.
- **Metrics need delta temporality.** The direct metrics intake
  rejects cumulative metrics, and the harness exports cumulative by
  default. `metricsTemporality: "delta"` (`--otel-metrics-temporality
  delta`) switches counters and histograms to delta.
- **Routing to Agent Observability.** `dd-otlp-source: llmobs` routes
  spans to Datadog's agent observability product. Datadog's own pages
  disagree on whether the resource attribute `dd_llmobs_enabled=true`
  is also needed: a 2026-10-05 change describes it as the switch that
  enables conversion, while another page still calls conversion
  automatic. The recipe sets both. **`headers` is sent with every
  signal**, so the routing header also reaches the metrics route, and
  the logs route when log export is on. Whether the metrics intake
  tolerates it is unconfirmed. A collector can split headers per
  signal.
- **Application name.** `observability.serviceName` becomes the
  Agent Observability application (`ml_app`). Without it every
  deployment appears as `stirrup`.
- **Logs.** Datadog's logs intake accepts OTLP/HTTP, and the harness's
  log export (`observability.logsExport.type: "otlp"`) uses the
  traces protocol and headers, so it can target the same host. That
  path is untested. Each exported record carries the trace and span
  IDs of the run span it was logged under (see
  [Log-trace correlation](#log-trace-correlation)), the fields
  Datadog's log-to-trace link reads; whether the direct intake
  preserves them is part of what a trial must confirm. The Datadog Agent, which accepts OTLP on ports
  `4317` and `4318` when enabled, and a collector are the other
  routes.
- **Content size.** The Datadog Agent truncates attribute values at
  25,000 characters. Whether the direct intake applies the same limit before
  conversion is unknown; `captureContentMaxBytes` keeps the value
  valid JSON if a cap is wanted.
- **Cost.** Agent Observability bills LLM spans only: a free tier of
  40K spans a month, then paid plans from $160 a month with 100K
  included (Datadog's pricing page, 2026-10-12). Component spans count
  toward APM, and each `stirrup.*` metric is a billable custom metric.

Unconfirmed until a trial: whether `dd-otlp-source` alone suffices,
whether Datadog reads tool results under the `response` key the
harness emits, whether cache-token attributes feed cost (the GenAI
mapping table omits them, so cost may be overstated on cache-heavy
runs), and whether `aws.bedrock` and `gcp.vertex_ai` provider names
resolve to priced providers.

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

```json
{"time":"2026-10-13T01:28:18.197692+01:00","level":"INFO","msg":"tool dispatched","runId":"run-123","tool":"read_file","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
```

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
| `gen_ai.conversation.id`, `session.id` | `correlation.sessionId`, else the `--name` session name, when either is set. |
| `user.id` | `correlation.userId`, when set. |
| `stirrup.task.id`, `stirrup.attempt.id` | `correlation.taskId` and `correlation.attemptId`, when set. |
| `stirrup.control_plane.url` | `correlation.url`, when set. |
| `stirrup.label.<key>` | One per `correlation.labels` entry. |

These run identity attributes are span attributes only: they are not
resource attributes, metric labels, or prompt content. See
[Correlation](configuration.md#correlation).

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
conventions gets the same view. The `phoenix` profile reads the
captured messages to fill the root span's input and output, but the
toggle itself adds no vendor attributes.

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
attribute value, the Datadog Agent truncates attribute values at
25,000 characters, and
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
`gen_ai.system_instructions` (see issue #775). The semconv GenAI
conventions are Development-status — attribute names track the pinned
semconv version (v1.40.0) and may evolve with it. The conventions have
since moved to the `semantic-conventions-genai` repository, which has
no tagged release; renames are tracked in issue #774.

## Precedence

Each exporter setting resolves independently. In every row an explicit
`RunConfig` field or CLI flag comes first; a flag applies only when it
is passed explicitly, so an unset flag does not clobber a value from the
`--config` file. A [platform profile](#platform-profiles) fills only
what the configuration and the standard OpenTelemetry environment
variables below leave empty.

| Setting | Resolution order, first match wins |
|---|---|
| Protocol | `traceEmitter.protocol`, then the protocol the profile requires, then `grpc`. `OTEL_EXPORTER_OTLP_PROTOCOL` is never consulted. |
| Trace endpoint | `traceEmitter.endpoint`, then the profile's trace endpoint, then `localhost:4317`. The `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` variables are never consulted, because the harness always passes an endpoint. |
| Metrics endpoint | `traceEmitter.metricsEndpoint`, then the profile's metrics endpoint, then `traceEmitter.endpoint`. With none of the three, metrics are not exported. The environment variables are never consulted. |
| Logs endpoint | `observability.logsExport.endpoint` (the harness CLI fills it from `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` when that variable is set, even over a value in the file). Without a profile endpoint for logs, then `traceEmitter.endpoint`. With one, the SDK's `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` when set, otherwise the profile's logs endpoint. When all of these are empty, the SDK reads the same two variables and then falls back to its own default. |
| Headers | `traceEmitter.headers` with `secret://` references resolved. Headers merge per key over a profile's header defaults for a signal; names compare case-insensitively. When `headers` is empty the SDK reads `OTEL_EXPORTER_OTLP_<SIGNAL>_HEADERS`, then `OTEL_EXPORTER_OTLP_HEADERS`, and a profile's defaults are merged under those. |
| Metrics temporality | `traceEmitter.metricsTemporality`, then `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`, then `cumulative`. |
| Resource attributes | The harness's identity attributes (`service.name`, `service.namespace`, `deployment.environment.name`, `service.version`, `service.instance.id`, `harness.run.mode`), then `OTEL_RESOURCE_ATTRIBUTES` and the SDK defaults, then the profile's attributes. |
| `service.name` | `observability.serviceName`, then `OTEL_SERVICE_NAME`, then `stirrup`. |

Two consequences follow from the table:

- **A non-empty `headers` map is the only header source.** The SDK
  variables are not merged into it. This is deliberate: silently
  merging two header maps would let an out-of-band environment variable
  override a config-file `Authorization` and surprise the operator.
  Headers supplied through the environment bypass the SecretStore and
  the validator's headers-require-HTTP check (see [CLI](#cli)).
- **Per-signal endpoints stop cascading when a profile defines them.**
  Without a profile, `metricsEndpoint` and `logsExport.endpoint` fall
  back to `traceEmitter.endpoint`. A signal for which the profile has an
  endpoint of its own no longer inherits it, so an explicit `endpoint`
  redirects traces only.

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
shape of the options: at most 8 entries, keys that start with a
lowercase letter and run to at most 32 letters and digits, and values
of at most 256 bytes of printable UTF-8. Which keys a platform accepts, whether required
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
2. A standard OpenTelemetry environment variable, for the settings
   where the exporter honours one (the log endpoint, headers, metrics
   temporality, and resource attributes; the [Precedence](#precedence)
   table lists them).
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

### Available profiles

| Profile | Backend | Reference |
|---|---|---|
| `cloudwatch` | Amazon CloudWatch generative AI observability: per-signal endpoints, SigV4 signing, log group headers, and the agent resource attributes. | [Amazon CloudWatch](#amazon-cloudwatch) |
| `phoenix` | Arize Phoenix: OpenInference span kinds, root input and output, and the project name. | [The `phoenix` profile](#the-phoenix-profile) |

No profile exists for Datadog, Langfuse, or Grafana. Their sections
configure the neutral settings only. A Datadog profile is deferred
until an account trial can confirm the open points listed there (see
issue #771).

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

## When a collector is still the better tool

The native HTTP path and the platform profiles are sufficient for the
common case. A collector (Alloy, the OpenTelemetry Collector, Vector)
remains the better tool when the deployment needs:

- **Sampling.** Tail-based sampling, head-based sampling, or rate
  limiting per-route. Stirrup ships every span; the collector
  decides what to keep. Useful when an eval suite produces 10k runs
  in an hour and only regressions belong in the long-term store.
- **Fan-out.** Sending the same telemetry to two backends (Grafana
  Cloud and an internal Tempo, for example) without authoring two
  RunConfigs.
- **Per-signal credentials or headers.** `traceEmitter.headers` is one
  map sent with every signal, so a routing header meant for traces
  reaches the metrics and logs routes too, and a credential cannot
  differ per signal. A collector exporter per signal can.
- **Attribute rewriting.** Backend-specific attributes that no profile
  provides, such as the `langfuse.*` set in the [Langfuse](#langfuse)
  recipe, and conversions such as `cumulativetodelta`, are collector
  processors.
- **Multi-tenant routing.** Re-writing resource attributes
  (`service.namespace`, `deployment.environment.name`) per route so a
  shared cluster can serve multiple tenants without each pod knowing
  its own tenant.
- **Protocol translation for sub-systems.** Some collectors also
  ingest Prometheus scrapes, syslog, or proprietary formats. Stirrup
  itself only emits OTLP, but a collector can normalise a wider
  fleet.
- **Cluster-local buffering.** Disk-backed queues for outage
  resilience, ahead of cross-region egress.

The collector configurations in this document spell the OTLP/HTTP
exporter `otlp_http`. Collector contrib 0.162.0 deprecates the older
`otlphttp` alias.

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

The endpoint fields (`traceEmitter.endpoint`,
`traceEmitter.metricsEndpoint`, and `observability.logsExport.endpoint`)
are checked at the same point: each must be `host:port` or an `http` or
`https` URL with no embedded credentials, query string, or fragment
(see [URL appending](#url-appending)).

Carrying an OTLP-only field (`protocol`, `headers`, `captureContent`,
`captureContentMaxBytes`, `metricsTemporality`, `platform`, or
`platformOptions`) on a `jsonl` emitter is also rejected — those
fields only have meaning for `type: "otel"`, and a leftover from a
migration should fail loudly rather than silently keep the wrong
config working.

`headers` requires `protocol: http/protobuf`. The gRPC exporter dials
the documented `host:port` endpoint form in plaintext, so any bearer
or Basic credential supplied via `headers` would ride a plaintext gRPC
channel to the collector. To make this footgun unreachable, the validator
rejects `headers` whenever `protocol` is `""` (which defaults to gRPC
at exporter construction) or `"grpc"`. The gRPC path is the
local/sidecar collector flow and assumes the collector itself does
not require auth. A gRPC link that needs auth goes through a sidecar
collector that handles the credential locally and forwards over a
trusted boundary, or switches to `http/protobuf`.

## Out of scope

- **gRPC ↔ HTTP fallback retry.** A misconfigured deployment should
  fail loudly. There is one transport per run.
- **HTTP/JSON encoding.** Binary protobuf only.
- **TLS client certificate / mTLS.** Use Bearer/Basic via the
  `headers` map; mTLS support is a separate request.
