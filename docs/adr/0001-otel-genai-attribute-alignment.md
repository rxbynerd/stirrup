# ADR-0001: OTel GenAI Attribute Alignment

- **Status:** Accepted
- **Date:** 2026-05-08
- **Revised:** 2026-05-08 — dual-emit was implemented and then withdrawn after review; see "Alternatives considered" below.
- **Issue:** [#108](https://github.com/rxbynerd/stirrup/issues/108)
- **Related:** [Issue #104](https://github.com/rxbynerd/stirrup/issues/104) — observability conventions research

## Context

The OTel trace emitter (`harness/internal/trace/otel.go`) emitted stirrup-specific span attribute names — `run.id`, `run.mode`, `run.provider`, `run.model`, `turn.tokens.input`, `turn.tokens.output`, `tool.name`, etc. These names predate the OpenTelemetry GenAI semantic conventions and reflect stirrup's internal vocabulary.

The OpenTelemetry [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) define a stable namespace for GenAI workload telemetry: `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, plus tool, agent, and operation attributes. As of mid-2026 most GenAI-specific attribute keys remain in the spec's *Development* stability tier (cross-cutting attributes like `error.type` have stabilised); the names are unlikely to churn meaningfully but the spec authors reserve the right to refine them.

The synthesis at `docs/research/issue-104/SYNTHESIS.md` found four-of-six convergence on a recommendation to align with the GenAI semconv where feasible. The argument is operational, not aesthetic:

- **Vendor APM dashboards key off the GenAI namespace.** Honeycomb, Datadog, Grafana, and New Relic all ship pre-built GenAI dashboards (token-cost panels, latency-per-model histograms, tool-call success rates). These dashboards filter on `gen_ai.provider.name`, `gen_ai.request.model`, and `gen_ai.usage.*`. Stirrup spans previously rendered as "unknown GenAI workload" in those dashboards even though the OTLP wire format was correct.
- **Operators currently have to write custom dashboards.** Adopting the GenAI namespace is the difference between drop-in observability and per-operator dashboard authoring.

stirrup is pre-release; no operators are running production deployments keyed on the legacy attribute names, so a transition window is unnecessary.

## Decision

Outright **rename**: replace each stirrup-prefixed span attribute that has a GenAI semconv equivalent with the GenAI name. Keep stirrup-prefixed names only for attributes that have no GenAI counterpart.

### Mapping

| Legacy attribute (removed) | GenAI attribute (emitted) | Span |
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

Unknown stirrup types fall through unchanged so future provider additions remain observable, even before the table is updated. Stirrup's internal vocabulary (e.g. distinguishing `openai-compatible` from `openai-responses`) is still surfaced through metrics attributes and the `RunTrace` Go struct; only the OTel span attribute carries the translated enum.

## Consequences

- **Spans are smaller.** Each turn span carries roughly four fewer attributes (~80 bytes), each tool span carries one fewer (~20 bytes), the root span carries up to three fewer (~60 bytes) versus the dual-emit alternative. For a 20-turn run with 30 tool calls the marginal saving is on the order of 2 kB per run versus dual-emit.
- **No operator-side migration burden.** stirrup is pre-release; there are no Grafana boards keyed on the legacy `run.*`/`turn.*`/`tool.*` names that would silently regress. The risk that motivated a transition window in mature systems does not apply here.
- **No parallel naming universe.** Future readers of the codebase have one vocabulary to learn, and any new observability work has a single name to extend. There is no follow-up cleanup PR to schedule and no removal-calendar complexity in the ADR.
- **Vendor-shipped GenAI dashboards recognise stirrup spans without custom configuration.** This is the operational win that motivated the alignment.
- **`gen_ai.agent.id` is intentionally not emitted.** The spec defines this as a persistent agent identity (e.g. an OpenAI Assistant ID). Stirrup has no first-class named-agent concept yet, so the run ID is the wrong value — every span would report a unique agent identity, and dashboards that group or aggregate by agent would see N agents for N runs rather than one agent with N runs. Emit when stirrup grows a named-agent configuration field; correcting the mapping after operators built dashboards on it would be a breaking change. (Follow-up issue: TBD.)
- **`gen_ai.operation.name` is currently emitted on the `turn[N]` span, but `provider.stream` is the spec-correct host.** The OTel GenAI spec scopes inference attributes (including `gen_ai.operation.name`) to the *provider call* span, not the agentic *turn* span. Stirrup's existing `provider.stream` span (in `harness/internal/core/loop.go`) is the more correct attachment point — it represents a single API call to the model with `SpanKind = CLIENT`, while `turn[N]` is a harness loop iteration that may contain a provider call plus tool dispatch and bookkeeping. Current placement on `turn[N]` is acceptable because the data is not corrupted and per-turn aggregates remain accurate; the move to `provider.stream` is tracked as a follow-up (TBD: file follow-up). New observability work should not extend the `turn[N]` placement further.
- **Metric attributes are out of scope for this ADR.** The metric attribute namespace in `harness/internal/observability/metrics.go` (`run.mode`, `provider.type`, `tool.name`, etc.) is intentionally untouched. Aligning metric attributes is a separate, larger surface — there are 12 counters, 5 histograms, and 1 UpDownCounter, and metrics carry stricter cardinality concerns than spans. If/when needed, track in a follow-up issue.
- **JSONL trace emitter is also out of scope.** `harness/internal/trace/jsonl.go` writes the `RunTrace` Go struct's JSON-tagged field names, not OTel attribute names. Different surface, different consumers (eval framework, replay).

## Alternatives considered

### Dual-emit (rejected)

Emit both the legacy `run.*`/`turn.*`/`tool.*` and the new `gen_ai.*` names on every span, with a removal calendar for the legacy names a couple of minor releases out.

This was the original landing of this work on `gh-108` (commits `722e214` and `b2f8113`) and would be the right call for a deployed system: operators with dashboards keyed on the legacy names get a transition window, and the harness can sunset the legacy names on a published schedule once the GenAI semconv graduates from Development to Stable.

Rejected after review for stirrup specifically because stirrup is pre-release. There are no operator-built dashboards to migrate, so the transition-window benefit buys nothing, while the cost is real: parallel naming universe in the codebase, every test asserts both, a future cleanup PR has to land and the ADR has to track a removal calendar with all of the "graduation tied to spec stabilisation" failure modes the original draft already flagged. Trading a second-of-engineering of re-aliasing later (if and when stirrup grows real operator users) for permanently simpler spans now is the right call.

### Alias-only without deprecation plan (rejected)

Emit both names indefinitely. Cheapest to implement; same parallel-naming-universe cost forever.

Rejected because it creates a parallel naming universe with no end state. Future readers of the codebase have to learn both vocabularies, and any new observability work has to decide which name to extend.

### Keep stirrup-only namespace (rejected)

Do nothing. Lowest implementation cost, but operators continue to need custom dashboards, and the project misses the operational win that motivated issue #108 in the first place. The synthesis from issue #104 explicitly endorses alignment.

## References

- Issue [#108](https://github.com/rxbynerd/stirrup/issues/108) — OTel GenAI attribute alignment
- Issue [#104](https://github.com/rxbynerd/stirrup/issues/104) — observability conventions research; SYNTHESIS endorses alignment
- [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/)
- `harness/internal/trace/otel.go` — implementation
- `harness/internal/trace/otel_test.go` — GenAI attribute assertions
