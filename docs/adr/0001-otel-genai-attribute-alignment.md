# ADR-0001: OTel GenAI Attribute Alignment

- **Status:** Accepted
- **Date:** 2026-05-08
- **Issue:** [#108](https://github.com/rxbynerd/stirrup/issues/108)
- **Related:** [Issue #104](https://github.com/rxbynerd/stirrup/issues/104) — observability conventions research

## Context

The OTel trace emitter (`harness/internal/trace/otel.go`) emits stirrup-specific span attribute names — `run.id`, `run.mode`, `run.provider`, `run.model`, `turn.tokens.input`, `turn.tokens.output`, `tool.name`, etc. These names predate the OpenTelemetry GenAI semantic conventions and reflect stirrup's internal vocabulary.

The OpenTelemetry [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) define a stable namespace for GenAI workload telemetry: `gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, plus tool, agent, and operation attributes. As of mid-2026 most GenAI-specific attribute keys remain in the spec's *Development* stability tier (cross-cutting attributes like `error.type` have stabilised); the names are unlikely to churn meaningfully but the spec authors reserve the right to refine them.

The synthesis at `docs/research/issue-104/SYNTHESIS.md` found four-of-six convergence on a recommendation to align with the GenAI semconv where feasible. The argument is operational, not aesthetic:

- **Vendor APM dashboards key off the GenAI namespace.** Honeycomb, Datadog, Grafana, and New Relic all ship pre-built GenAI dashboards (token-cost panels, latency-per-model histograms, tool-call success rates). These dashboards filter on `gen_ai.system`, `gen_ai.request.model`, and `gen_ai.usage.*`. Stirrup spans currently render as "unknown GenAI workload" in those dashboards even though the OTLP wire format is correct.
- **Operators currently have to write custom dashboards.** Adopting the GenAI namespace is the difference between drop-in observability and per-operator dashboard authoring.

The change itself is small (~30 lines of added `attribute.String(...)` calls). The substantive question is the transition policy.

## Decision

Adopt **dual-emit**: add the GenAI semconv attributes alongside the existing `run.*` / `turn.*` / `tool.*` attributes on every span emitted by the OTel emitter. Do not remove the existing names in this change. Schedule removal of the legacy names for a future minor release once the GenAI semconv attributes graduate from Development to Stable.

### Mapping

The dual-emit pairs follow the table below. Where the GenAI semconv has no counterpart, the stirrup-prefixed attribute is emitted alone (no synthetic GenAI alias is invented).

| Existing attribute (kept) | GenAI attribute (added) | Span |
|---|---|---|
| `run.provider` | `gen_ai.provider.name` (translated, see below) | run |
| `run.model` | `gen_ai.request.model` | run |
| `run.session_name` | `gen_ai.conversation.id` | run |
| `turn.tokens.input` | `gen_ai.usage.input_tokens` | turn |
| `turn.tokens.output` | `gen_ai.usage.output_tokens` | turn |
| `turn.stop_reason` | `gen_ai.response.finish_reasons` (array) | turn |
| `tool.name` | `gen_ai.tool.name` | tool_call |
| `run.id` | (none — execution instance, not agent identity) | run |
| `run.mode` | (none — stirrup-specific) | run |
| `run.outcome` | (none — stirrup-specific) | run |
| `run.turns` | (none — stirrup-specific) | run |
| `turn.number` | (none — stirrup-specific) | turn |
| `turn.tool_calls` | (none — stirrup-specific) | turn |
| `turn.duration_ms` | (none — stirrup-specific) | turn |
| `tool.success` | (none — stirrup-specific) | tool_call |
| `tool.duration_ms` | (none — stirrup-specific) | tool_call |
| `harness.version` | (none — stirrup-specific) | run |

Three additional notes:

- `gen_ai.response.finish_reasons` is an **array** in the semconv; the harness wraps its single scalar `StopReason` in a one-element string slice rather than emitting a scalar that downstream consumers would have to special-case.
- The turn span gets a `gen_ai.operation.name = "chat"` attribute — per semconv, an LLM completion is a `chat` operation.
- The OTel GenAI registry deprecated `gen_ai.system` in favour of `gen_ai.provider.name`; vendor APM dashboards key off the current non-deprecated attribute, so this ADR uses the new name from day one.

#### Provider enum translation

The OTel GenAI semconv defines a closed enum for `gen_ai.provider.name`. Stirrup's internal `config.Provider.Type` strings only match the spec for `anthropic`; the other four would fail dashboard filters. The trace emitter routes through a private helper, `genAIProviderName` in `otel.go`, which maps:

| Stirrup `config.Provider.Type` | `gen_ai.provider.name` (emitted) |
|---|---|
| `anthropic` | `anthropic` |
| `bedrock` | `aws.bedrock` |
| `openai-compatible` | `openai` |
| `openai-responses` | `openai` |
| `gemini` | `gcp.vertex_ai` |

Unknown stirrup types fall through unchanged so future provider additions remain observable, even before the table is updated. The legacy `run.provider` attribute keeps the original (untranslated) stirrup value for operators who need stirrup's internal vocabulary.

## Consequences

- **Span size grows.** Each turn span carries roughly four extra attributes (~80 bytes), each tool span carries one (~20 bytes), the root span carries up to four (~80 bytes). For a 20-turn run with 30 tool calls the marginal overhead is on the order of 2 kB per run. Acceptable.
- **Operators can migrate dashboards on their own schedule.** Existing Grafana boards keyed on `run.*` keep working; new boards can adopt `gen_ai.*` immediately.
- **Vendor-shipped GenAI dashboards begin recognising stirrup spans without custom configuration.** This is the operational win.
- **`gen_ai.agent.id` is intentionally not emitted.** The spec defines this as a persistent agent identity (e.g. an OpenAI Assistant ID). Stirrup has no first-class named-agent concept yet, so the run ID is the wrong value — every span would report a unique agent identity, and dashboards that group or aggregate by agent would see N agents for N runs rather than one agent with N runs. Emit when stirrup grows a named-agent configuration field; correcting the mapping after operators built dashboards on it would be a breaking change. (Follow-up issue: TBD.)
- **`gen_ai.operation.name` is currently emitted on the `turn[N]` span, but `provider.stream` is the spec-correct host.** The OTel GenAI spec scopes inference attributes (including `gen_ai.operation.name`) to the *provider call* span, not the agentic *turn* span. Stirrup's existing `provider.stream` span (in `harness/internal/core/loop.go`) is the more correct attachment point — it represents a single API call to the model with `SpanKind = CLIENT`, while `turn[N]` is a harness loop iteration that may contain a provider call plus tool dispatch and bookkeeping. Current placement on `turn[N]` is acceptable for the dual-emit window because the data is not corrupted and per-turn aggregates remain accurate; the move to `provider.stream` is tracked as a follow-up (TBD: file follow-up). New observability work should not extend the `turn[N]` placement further.
- **Removal calendar.** Legacy `run.*` / `turn.*` / `tool.*` attribute removal is targeted for the **second minor version after this ADR ships** (e.g. if this lands in v0.x, removal targets v0.(x+2)), regardless of whether the GenAI semconv has graduated from Development to Stable by then. Operators are expected to migrate dashboards within that window. GenAI semconv graduation may accelerate removal but will not block it; the open-ended "until the spec stabilises" trigger was rejected because the spec has been in Development for 2+ years with no committed graduation date and the same failure mode would defeat the deprecation plan.
- **Metric attributes are out of scope for this ADR.** The metric attribute namespace in `harness/internal/observability/metrics.go` (`run.mode`, `provider.type`, `tool.name`, etc.) is intentionally untouched. Aligning metric attributes is a separate, larger surface — there are 12 counters, 5 histograms, and 1 UpDownCounter, and metrics carry stricter cardinality concerns than spans. If/when needed, track in a follow-up issue.
- **JSONL trace emitter is also out of scope.** `harness/internal/trace/jsonl.go` writes the `RunTrace` Go struct's JSON-tagged field names, not OTel attribute names. Different surface, different consumers (eval framework, replay).

## Alternatives considered

### Outright rename (rejected)

Replace `run.*` / `turn.*` / `tool.*` with `gen_ai.*` in a single change. Smallest diff and zero parallel-namespace cost.

Rejected because operators who already key Grafana / Honeycomb / Datadog dashboards on `run.id`, `run.model`, `turn.tokens.*` would experience a silent regression — queries return empty rows after upgrade. The harness has no way to detect this, so the regression would surface as "the dashboard stopped working after the bump" days later. The operational cost falls on operators, not on us, which is the wrong direction.

### Alias-only without deprecation plan (rejected)

Emit both names indefinitely. Cheapest implementation; same dual-emit cost forever.

Rejected because it creates a parallel naming universe with no end state. Future readers of the codebase have to learn both vocabularies, and any new observability work has to decide which name to extend. A finite dual-emit window with an explicit removal trigger keeps the eventual end-state clean.

### Keep stirrup-only namespace (rejected)

Do nothing. Lowest implementation cost, but operators continue to need custom dashboards, and the project misses the operational win that motivated issue #108 in the first place. The synthesis from issue #104 explicitly endorses alignment.

## References

- Issue [#108](https://github.com/rxbynerd/stirrup/issues/108) — OTel GenAI attribute alignment
- Issue [#104](https://github.com/rxbynerd/stirrup/issues/104) — observability conventions research; SYNTHESIS endorses alignment
- [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/)
- `harness/internal/trace/otel.go` — implementation
- `harness/internal/trace/otel_test.go` — dual-emit assertions
