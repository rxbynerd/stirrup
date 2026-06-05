# Integrating Langfuse with Stirrup — research & recommendation

Date: 2026-06-05
Branch: `langfuse-integration`
Method: deep-research workflow (102 agents, 5 search angles, 20 sources,
98 claims extracted, 25 adversarially verified) cross-checked against
direct reads of both codebases (`/Users/rubynerd/Developer/stirrup`,
`/Users/rubynerd/Developer/langfuse` @ `9eec10511`, 2026-06-05) and `gh`
issue archaeology on `rxbynerd/stirrup`.

---

## Recommendation

**Integrate Langfuse with zero Stirrup code changes. It is a
documentation task, not an engineering one.** Langfuse is just another
OTLP/HTTP-protobuf backend — one new row in
[`docs/observability-cloud.md`](../docs/observability-cloud.md) beside
the Grafana, Honeycomb, GCP, and Datadog recipes that already exist, plus
a short worked example. Stirrup already exposes the exact three knobs
Langfuse needs, and `http/protobuf` is already an accepted protocol
value.

The working configuration is:

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

where the secret resolves to `Basic <base64(publicKey:secretKey)>`:

```sh
export LANGFUSE_AUTH="Basic $(printf '%s' 'pk-lf-...:sk-lf-...' | base64)"
```

Stirrup's `joinTracesPath` appends `/v1/traces` to the configured base,
which lands exactly on Langfuse's route
(`web/src/pages/api/public/otel/v1/traces/index.ts`); the sibling
`/v1/metrics` route exists too, so `metricsEndpoint` on the same base is
safe.

**The one and only gotcha: Stirrup defaults to OTLP/gRPC, and Langfuse's
ingest is HTTP-only.** Operators *must* set `protocol: "http/protobuf"`.
Without it the exporter builds a gRPC client (`otel.go`
`case "", "grpc"`) and Langfuse silently drops the spans. This is a
documentation note, not a code change.

### What NOT to do

- **Do not add `--langfuse-host`** (or any `langfuse.*` config). The
  brief already rules this out; the codebase agrees — `docs/observability-cloud.md`
  states plainly: *"Stirrup does not vendor-detect — `protocol`,
  `endpoint`, and `headers` are sufficient for any OTLP/HTTP-protobuf
  gateway."* A Langfuse-named surface is a regression against that
  invariant.

- **Do not add `--otel-convention=langfuse` either.** The
  convention-selector *pattern* is legitimate and precedented — OTel
  itself gates convention variants behind `OTEL_SEMCONV_STABILITY_OPT_IN`
  — but a **vendor** value is the wrong use of it. Langfuse reads the
  standard `gen_ai.*` attributes Stirrup already emits (issue #108)
  natively, so a `langfuse` convention value would be a **no-op at best**.
  At worst it becomes the hook that justifies bespoke `langfuse.*`
  attribute-mapping code later — exactly the vendor-specific logic the
  brief says to avoid. **If a convention selector is ever built, its
  values must name a _convention flavour_, never a vendor.**

### The single change worth considering — and it is vendor-neutral

Stirrup's OTel trace emitter records turn-level counters (tokens,
duration, stop reason) and per-tool spans, but **`RecordTurnRecord` is a
deliberate no-op** (`otel.go:358`) — no prompt or completion *content*
ever reaches a span. Langfuse's ingest processor
(`packages/shared/src/server/otel/OtelIngestionProcessor.ts`) reads
input/output from exactly these keys:

```
gen_ai.input.messages
gen_ai.output.messages
gen_ai.system_instructions
gen_ai.{user,assistant,system,tool}.message   (span events)
```

Stirrup emits **none** of them, so a Langfuse trace shows correct
structure, token usage, model, and timing but an **empty
prompt/IO view**.

If richer Langfuse traces are wanted, the right change is a **default-off,
opt-in, vendor-neutral content-capture toggle** that makes
`RecordTurnRecord` emit those standard `gen_ai.*` content attributes. It
is correct because:

1. It is **pure OTel semconv** — no Langfuse string appears anywhere.
2. It **equally benefits SigNoz, Datadog, and Phoenix**, which read the
   same keys.
3. It matches the OTel spec, where message content is **Opt-In** and
   off by default precisely because of PII.

This change is **gated on a product decision** (see Open Questions): it
must respect `RunConfig.Redact()` and the scrubbing layer, default off,
and is probably mode- and policy-gated. Until that decision is made,
Langfuse integration is metadata-only and remains a docs-only task.

---

## Evidence

### 1. Stirrup already has every knob; Langfuse needs no new one

| Need | Where it already lives | Verified |
|---|---|---|
| OTLP/HTTP-protobuf transport | `validTraceEmitterProtocols = {"", "grpc", "http/protobuf"}` (`types/runconfig.go:1506`) | ✓ `http/protobuf` is a first-class accepted value (landed in #100) |
| Per-signal path append | `joinTracesPath(base) → base + "/v1/traces"` (`otel.go:216`) | ✓ matches Langfuse's `/api/public/otel/v1/traces` route exactly |
| Auth header, secret-managed | `TraceEmitter.Headers`, `secret://`-resolved, scrubbed, `Redact()`-stripped (`runconfig.go:1132`, `:486`) | ✓ identical to the Grafana `Authorization: secret://…` recipe |
| `gen_ai.*` span attributes | `gen_ai.provider.name/request.model/conversation.id/operation.name/usage.*/response.finish_reasons/tool.name` (`otel.go:33-42`) | ✓ Langfuse maps these natively |

Langfuse's own requirements (first-party docs, confirmed against the
local checkout): OTLP ingest at `/api/public/otel`, **HTTP only (no
gRPC)**, **Basic auth** `base64(publicKey:secretKey)`, self-hosted
**v3.22.0+**. The local checkout (HEAD 2026-06-05) is well past that
floor.

Net: the integration is a 4-line config block. No flag, no struct field,
no factory branch.

### 2. The pressure test confirms Langfuse deserves no special treatment

The brief asked: how hard is SigNoz or Datadog? **Identical** — they
differ only in endpoint, header name, and (for some) protocol. The
codebase already proves this:

- **Datadog** is *already* a documented row in
  `docs/observability-cloud.md` (`dd-api-key: secret://DD_API_KEY`).
- **SigNoz** is tracked in **open issue #156** ("research: SigNoz as an
  observability backend"), the sister of the **closed #94** ("Grafana
  research").
- **Grafana** is the worked example the whole doc is built around
  (#94 closed; local-stack #98 and cloud #99 still open).

Three landed pieces of groundwork make all of them pure-config:
**native OTLP/HTTP (#100)**, **`gen_ai.*` semconv alignment (#108)**, and
**run-scoped fields promoted to OTel Resource (#95)**. Langfuse slots
into the same slot with the same effort. A Langfuse-specific surface
would be an inconsistency the rest of the observability story has
deliberately avoided.

### 3. The convention-selector idea is precedented but unnecessary here

OTel's own mechanism for choosing between convention variants is the
`OTEL_SEMCONV_STABILITY_OPT_IN` environment variable — so a
"select a convention" knob is not alien to the ecosystem. Third-party
gateways (e.g. Bifrost) drive Langfuse, Datadog, Honeycomb, and Grafana
through a single OTLP exporter with **no per-vendor branches**. Because
Langfuse already understands `gen_ai.*`, there is nothing for a
`--otel-convention=langfuse` value to *do*. The pattern is sound; the
vendor-named value is not.

### 4. Two over-strong claims were refuted — the honest nuance

Adversarial verification (3-vote, need 2/3 to kill) **killed two
claims 0-3**:

- *"The GenAI conventions define a complete, vendor-neutral signal set …
  sufficient to reconstruct LLM traces without vendor-specific
  instrumentation."* — **Refuted.**
- *"GenAI telemetry … is backend-agnostic: a harness emitting standard
  `gen_ai.*` spans needs no vendor-specific code to target Langfuse,
  SigNoz, Datadog, or Grafana."* — **Refuted.**

The surviving, defensible truth is narrower and is the basis of this
recommendation: **ingestion is config-only, but integration *richness*
is not uniform.** Prompt/IO views need opt-in content capture (the
vendor-neutral toggle above). Backend-native concepts —
Langfuse **Sessions**, **Users**, **Tags** — are populated through
Langfuse's own `langfuse.*` attribute namespace, which standard
`gen_ai.*` does not cover. Stirrup should **not** chase those: doing so
is precisely the bespoke vendor logic the ethos forbids. The acceptable
loss is that Langfuse's session/user/tag panels stay sparse while traces,
timing, tokens, model, and (optionally) content all work.

---

## Learnings & observations

- **`gen_ai.conversation.id` is already wired from `SessionName`**
  (`otel.go:293`, via #50). Langfuse keys its **Session** view off a
  `langfuse.session.id` / `sessionId` field, **not** `gen_ai.conversation.id`.
  So Stirrup's session label will *not* automatically populate Langfuse
  Sessions. This is the most tempting place to add a bespoke mapping —
  resist it. If session grouping in Langfuse becomes a hard requirement,
  the least-bad option is a collector-side `transformprocessor` rule
  (copy `gen_ai.conversation.id` → `langfuse.session.id`) that lives in
  *operator* config, not in Stirrup.

- **Sub-agent traces will look flat in Langfuse.** Open issue **#89**
  ("OTel turn[N] nesting limitation under spawn_agent") means child-loop
  `turn[N]` spans are parented off `context.Background()`, not the
  parent's `tool.spawn_agent` span (`otel.go:323` TODO). Multi-agent runs
  will not render as a clean nested tree in any OTLP backend, Langfuse
  included. Worth fixing for trace quality generally, but it is not a
  Langfuse-specific problem and shouldn't be framed as one.

- **`gen_ai.agent.id` is intentionally not emitted** (#127) until Stirrup
  grows a named-agent concept. Langfuse will show provider/model but no
  persistent agent identity — correct and expected.

- **All OTel GenAI conventions are still "Development" status** (June
  2026) and the spec is relocating to a `semantic-conventions-genai`
  repo. Cited URLs may move and attribute names may change under us.
  Consider adopting `OTEL_SEMCONV_STABILITY_OPT_IN` version pinning
  proactively so Langfuse/SigNoz/Datadog dashboards don't break on a spec
  bump — but this is a general hardening item, decoupled from Langfuse.

- **The `http/json` protocol is deliberately excluded** (`runconfig.go`
  comment). Langfuse accepts protobuf via its `otlp-proto` route, so this
  is a non-issue — but it's why the recommendation pins `http/protobuf`
  specifically.

- **Doc-only change keeps the project's invariants intact.** No
  `RunConfig` field, no secret path, no `http.DefaultClient`, no new
  vendor SDK, no loop-purity violation. The cleanest possible
  integration is the one that adds nothing to the binary.

---

## Open questions (require a human/product call)

1. **Does Stirrup want prompt/completion content on OTel spans at all?**
   `RecordTurnRecord` is a *deliberate* no-op and the PII posture is
   conservative. This single decision gates whether the vendor-neutral
   content-capture toggle is built. If "no", Langfuse stays metadata-only
   and this whole effort is one doc PR.

2. **Is a thin, opt-in `gen_ai.conversation.id → langfuse.session.id`
   propagation acceptable, or too bespoke?** If too bespoke (likely,
   given the ethos), document the collector-side `transformprocessor`
   recipe as the operator's escape hatch instead.

3. **Adopt `OTEL_SEMCONV_STABILITY_OPT_IN` version pinning now, or defer
   until the GenAI conventions stabilise?** And is fixing **#89**
   (sub-agent span nesting) in scope, given it degrades multi-agent trace
   trees in every backend?

---

## Sources

Primary (first-party):
- Langfuse — Native OpenTelemetry integration: <https://langfuse.com/integrations/native/opentelemetry>
- Langfuse — Data model: <https://langfuse.com/docs/observability/data-model>
- Langfuse source: `web/src/pages/api/public/otel/v1/{traces,metrics}/index.ts`,
  `packages/shared/src/server/otel/OtelIngestionProcessor.ts` (local checkout `9eec10511`)
- OTel GenAI semconv: <https://opentelemetry.io/docs/specs/semconv/gen-ai/> (+ `/gen-ai-spans/`, `/gen-ai-agent-spans/`, `/gen-ai-events/`)
- Datadog OTel LLM instrumentation: <https://docs.datadoghq.com/llm_observability/instrumentation/otel_instrumentation/>
- OTel Collector transform processor: <https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/processor/transformprocessor/README.md>

Stirrup code & issues (direct reads):
- `harness/internal/trace/otel.go`, `types/runconfig.go`, `docs/observability-cloud.md`
- Issues: #100 (OTLP/HTTP, closed), #108 (gen_ai alignment, closed), #95 (Resource fields, closed), #50 (session name, closed), #94 (Grafana research, closed), #98/#99 (Grafana local/cloud, open), #156 (SigNoz, open), #89 (sub-agent span nesting, open), #127 (gen_ai.agent.id, closed)

Secondary:
- Bifrost OTel observability: <https://docs.getbifrost.ai/features/observability/otel>
- SigNoz LLM observability: <https://signoz.io/blog/llm-observability-opentelemetry/>
