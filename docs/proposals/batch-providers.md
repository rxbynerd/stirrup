# Batch provider API support — implementation plan

**Status:** Draft (research; no code yet)
**Tracking issue:** [#67](https://github.com/rxbynerd/stirrup/issues/67)
**Author / date:** Researched 2026-05-03
**Audience:** future implementer (human or AI) picking up issue #67

---

## TL;DR

Both Anthropic and OpenAI offer asynchronous batch APIs that trade ~24 h
latency for a 50 % cost discount. They are a poor fit for `execution`,
`planning`, and `review` modes (interactive, multi-turn, tool-loop heavy)
but a strong fit for `research` and `toil` modes when the operator
deliberately opts in. This plan recommends:

1. Add a sibling **`BatchProviderAdapter`** interface (not a new method on
   `ProviderAdapter`) and a thin **adapter wrapper that fakes a stream**
   over a completed batch result, so the existing `core.runInnerLoop`
   stays unchanged.
2. Treat batch as **single-turn-per-batch at the harness boundary**: each
   turn submits one batch entry; the control plane is free to bundle
   entries from many concurrent runs into a single provider-side batch
   (one `custom_id` per harness run/turn). Multi-turn tool loops are
   preserved unchanged at the cost of N × 24 h worst-case for multi-turn
   runs — acceptable in `research`/`toil`.
3. **Submission unit = single run**, never "many turns from one run":
   batch entries are routed through the harness's existing
   `tool_result_request`/`tool_result_response` correlator pattern by
   adding two new event types — `batch_submission` (HarnessEvent) and
   `batch_result` (ControlEvent) — and the control plane is responsible
   for fanning many concurrent harness runs into the provider's true
   batch endpoint when it wants the cost discount.
4. The control plane terminates provider webhooks where available
   (OpenAI `batch.completed` today; Anthropic when/if they ship them) and
   falls back to polling otherwise. Stdio users get a degraded
   **harness-side polling** fallback, gated off by default and explicitly
   documented as such.
5. New RunConfig surface lives under `ProviderConfig.Batch`:
   `enabled`, `maxWaitSeconds`, `fallbackOnTimeout`. JSON shape and
   validation invariants are detailed in §10.

A phased rollout (events first, then provider wrapper, then CLI flag,
then control-plane integration, then stdio polling fallback) is at the
end.

---

## 1. Background

Stirrup's provider layer is hand-rolled HTTP/SSE against documented REST
APIs (see `harness/internal/provider/anthropic.go::AnthropicAdapter.Stream`,
`openai.go::OpenAIAdapter.Stream`, `bedrock.go::BedrockAdapter.Stream`,
`openai_responses.go::OpenAIResponsesAdapter.Stream`). The
`ProviderAdapter` interface is a single method
([`provider.go::ProviderAdapter`](../../harness/internal/provider/provider.go)):

```go
type ProviderAdapter interface {
    Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error)
}
```

`StreamEvent` discriminates on `text_delta` / `tool_call` /
`message_complete` / `error` ([`types/events.go::StreamEvent`](../../types/events.go)).
The agentic loop in
[`harness/internal/core/loop.go::runInnerLoop`](../../harness/internal/core/loop.go)
calls `Stream`, drains the channel via `streamEventsToResult`
([`core/types.go::streamEventsToResult`](../../harness/internal/core/types.go)), and
then dispatches any `tool_use` blocks. This is the *only* shape the loop
understands.

External docs read for this plan:

| Doc | Notes |
|---|---|
| Anthropic Message Batches API guide ([`platform.claude.com/docs/en/build-with-claude/batch-processing`](https://platform.claude.com/docs/en/build-with-claude/batch-processing)) | 50 % discount, 24 h SLA, 100 000 requests / 256 MB per batch, results retained 29 days, `POST /v1/messages/batches`, polling via `processing_status`, four result types: `succeeded` / `errored` / `canceled` / `expired`. Tool use, system prompts, multi-turn, and beta features are all batchable. **Streaming is not supported in batch.** |
| OpenAI Batch API reference + cookbook ([`developers.openai.com/api/reference/resources/batches`](https://developers.openai.com/api/reference/resources/batches), [`developers.openai.com/cookbook/examples/batch_processing`](https://developers.openai.com/cookbook/examples/batch_processing)) | 50 % discount, 24 h `completion_window`, polling-only (status: `validating` → `in_progress` → `finalizing` → `completed` / `failed` / `expired` / `cancelling` / `cancelled`), partial failure via `error_file_id`, file upload (`POST /files` purpose=batch) precedes batch creation (`POST /batches`). Endpoint must be one of `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/completions`. Function calling supported. |

> **Caveat on webhooks.** Issue #67 says "Anthropic has webhooks; OpenAI
> has polling." The Anthropic batch-processing guide I had access to
> (linked above) shows polling as the documented mechanism and does
> *not* mention webhooks; Anthropic added a separate Webhooks product
> ([platform.claude.com/.../webhooks](https://platform.claude.com/docs/en/build-with-claude/webhooks))
> that fires `message_batch.succeeded` / `.errored` / `.expired` /
> `.canceled` events for batches you own, but the page returned 404 on
> the URL I tried, so this plan treats webhook support as an opt-in
> control-plane feature rather than a hard requirement, and assumes
> polling is always a viable transport.
> *[Verify against current Anthropic docs before implementation.]*

---

## 2. Required grounding (per #67)

### 2.1 `ProviderAdapter` integration shape

[`harness/internal/provider/provider.go::ProviderAdapter`](../../harness/internal/provider/provider.go)
defines the existing one-method interface. Three integration shapes are
possible:

| Shape | Pros | Cons | Recommendation |
|---|---|---|---|
| **A. Add `Submit`/`Poll` methods to `ProviderAdapter`** | Single interface, simple `if cfg.Batch.Enabled` dispatch in the loop. | Forces every existing adapter (Bedrock, OpenAI Chat, OpenAI Responses, Replay) to grow stub no-op methods. Replay is especially awkward — there is no notion of "submission" in a fixture. The loop has to learn a new control-flow path. | Reject. |
| **B. Sibling `BatchProviderAdapter` interface, switched on per turn** | Clean separation of concerns; replay/sub-agent paths untouched. | The loop needs new control-flow that isn't `Stream`. | Reject in favour of Option C, which achieves the same separation without loop churn. |
| **C. Wrapper adapter that implements `ProviderAdapter.Stream` by submitting → blocking → faking a stream over the completed batch result** | Zero changes to `core.runInnerLoop`. The wrapper composes around an underlying batch client. Replay path keeps using `ReplayProvider` unchanged. Stall detection, token tracking, trace recording all work as-is. | Wrapper has to fabricate `text_delta` and `tool_call` events from a completed Messages-API response — straightforward because the batch result *is* a complete Messages response. The "stream" arrives in one chunk: a series of buffered events, not a true stream. | **Recommended primary shape.** |

The wrapper sits in `harness/internal/provider/batch.go` (new file) and
implements `ProviderAdapter`:

```go
// Sketch only — no production code in this issue.
type BatchAdapter struct {
    client     BatchClient        // see §2.1.1
    waitMax    time.Duration
    transport  transport.Transport // for emitting batch_submission events
    correlator *transport.Correlator
}

func (b *BatchAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
    ch := make(chan types.StreamEvent, 64)
    go func() {
        defer close(ch)
        // 1. Marshal params using the existing per-provider request
        //    builder (see §3) and emit batch_submission.
        // 2. Block on batch_result via the correlator.
        // 3. On success, fabricate text_delta/tool_call events from the
        //    completed Messages-API response, then a message_complete.
        //    On failure, emit a single error event with the result's
        //    error.type ("invalid_request_error", "expired", etc.).
    }()
    return ch, nil
}
```

The "fabricate" step is the only non-obvious bit. A worked sketch:

```go
// Sketch: translating a completed Messages-API response into the
// StreamEvent flow the loop expects.
func fabricateStream(ch chan<- types.StreamEvent, resp messagesResponse) {
    for _, block := range resp.Content {
        switch block.Type {
        case "text":
            ch <- types.StreamEvent{Type: "text_delta", Text: block.Text}
        case "tool_use":
            ch <- types.StreamEvent{
                Type:  "tool_call",
                ID:    block.ID,
                Name:  block.Name,
                Input: block.Input,
            }
        }
    }
    ch <- types.StreamEvent{
        Type:         "message_complete",
        StopReason:   resp.StopReason,    // direct passthrough; same enum
        OutputTokens: resp.Usage.OutputTokens,
        Content:      resp.Content,
    }
}
```

`stallDetector.recordToolCall` is unaffected by the fabrication: tool-call
detection happens after `streamEventsToResult` collects the buffered
events, so the loop sees the same `(name, input)` pairs whether they
arrived via SSE or via batch.

`BatchClient` is a small internal interface owned by the wrapper:

```go
type BatchClient interface {
    // Submit posts one or more entries as a single provider batch. For
    // Anthropic this maps to POST /v1/messages/batches with requests=entries.
    // For OpenAI this maps to POST /v1/files (purpose=batch) with a JSONL
    // body of the entries, then POST /v1/batches referencing input_file_id.
    Submit(ctx context.Context, entries []BatchEntry) (batchID string, err error)

    // Result returns one BatchResult per submitted entry, keyed by the
    // entry's custom_id. Callers select their entry by the custom_id they
    // assigned at submission time.
    Result(ctx context.Context, batchID string) (map[string]*BatchResult, error)
}
```

The multi-entry shape is required for OpenAI's two-step flow
(`POST /v1/files` then `POST /v1/batches`); Anthropic's single-step
endpoint accepts the same shape with a one-element slice. The size-1
case (used by `harnessPollingBatchClient`) is a thin wrapper over the
multi-entry interface.

with two implementations:

- `controlPlaneBatchClient` — emits `batch_submission` HarnessEvent and
  awaits `batch_result` on the transport via the correlator. This is
  the **only** implementation enabled when `transport=grpc`.
- `harnessPollingBatchClient` — calls the provider's HTTP batch endpoints
  directly (Anthropic `POST /v1/messages/batches` then GET-polls
  `processing_status`; OpenAI `POST /files` + `POST /batches` + poll).
  Used **only** for the `transport=stdio` degraded mode (§7).

This composition keeps Issue #67's "use existing request marshalling
rather than duplicating it" requirement: the wrapper calls into the
underlying provider adapter's request-builder helpers (refactored, see
§3) rather than re-implementing them.

### 2.2 Run modes

[`types/runconfig.go::IsReadOnlyMode`](../../types/runconfig.go) defines
the five modes and returns true for `planning`, `review`, `research`,
`toil`. The mode→batch eligibility matrix:

| Mode | Batch eligibility | Why |
|---|---|---|
| `execution` | **Reject** | Interactive coding; user is waiting. 24 h latency is unacceptable. Tool loops are deep; the cost-saving multiplier from a single batched turn is small. |
| `planning` | **Reject by default** (allow with `provider.batch.enabled` + explicit acknowledgement) | Often interactive (operator triages a plan). Read-only but typically short. The cost case is weak. |
| `review` | **Reject by default** | Same reasoning as planning. |
| `research` | **Allow opt-in (recommended)** | Read-only, web_fetch + spawn_agent, often run as overnight toil-style jobs. Cost-savings on long contexts (e.g. mining a transcript) are meaningful. |
| `toil` | **Allow opt-in (recommended)** | Read-only, automated, definitionally non-interactive — the bullseye for batch. |

Eligibility is not auto-selected. It is **operator opt-in via
`RunConfig.Provider.Batch.Enabled = true`**, and `ValidateRunConfig` rejects
the combination of `Batch.Enabled` with `mode == "execution"` (and warns,
but allows, with `planning`/`review` if the operator also sets
`Batch.AllowInteractiveModes`). This mirrors the existing pattern of
defaulting-but-permitting overrides (see `RuleOfTwo.Enforce` and the
`CodeScanner` mode-aware default in `types/runconfig.go::defaultCodeScannerType`).

Re. the issue's note that "the agentic loop is fundamentally turn-based":
correct, and confirmed against
[`core/loop.go::runInnerLoop`](../../harness/internal/core/loop.go). Each turn
needs the previous turn's tool outputs before the next provider call.
**Batch is therefore a single-turn-per-batch construct in this design**:
each turn submits one batch entry of size 1 and blocks until results
arrive. The batch endpoint is in this case a slow streaming endpoint, not
a fan-out tool. The full economics of batch (50 % off) are still captured
because the price per token does not depend on batch size at the
provider — only the asynchronous-processing flag is what triggers the
discount. Multi-turn runs simply pay the latency multiplier (N turns × up
to 24 h each), which is fine for `toil` and `research` and is exactly why
the eligibility matrix excludes interactive modes.

### 2.3 Async correlation pattern (transport)

The harness already has the exact shape this design needs. Two existing
correlator usages:

1. **Permission requests** —
   [`permission/askupstream.go::AskUpstreamPolicy.Check`](../../harness/internal/permission/askupstream.go)
   emits a `permission_request` HarnessEvent, blocks on the correlator,
   unblocks on `permission_response` keyed by `RequestID`. The
   correlator itself is in
   [`transport/correlator.go::Correlator`](../../harness/internal/transport/correlator.go)
   and supports any number of concurrent awaits, configurable timeout,
   and ctx cancellation.
2. **Async tool dispatch** —
   [`core/types.go::dispatchAsyncToolCall`](../../harness/internal/core/types.go)
   emits `tool_result_request` and blocks on `tool_result_response` via
   a loop-owned correlator (`asyncCorrelator`,
   [`core/types.go::asyncCorrelator`](../../harness/internal/core/types.go)).
   Lazy-constructed on first async tool dispatch, attached to the
   transport's `OnControl` fan-out.

**Batch will mirror these exactly.** Two new event types:

```protobuf
// HarnessEvent.type = "batch_submission"
//   - request_id:   correlation ID
//   - tool_name:    unused (omitempty)
//   - input:        JSON-encoded BatchSubmission payload (model, messages,
//                   tools, system, max_tokens, provider_type)
//   - content:      unused
//
// ControlEvent.type = "batch_result"
//   - request_id:   echoes the originating batch_submission
//   - content:      JSON-encoded BatchResult payload (success: complete
//                   Messages-API response; error: structured error)
//   - is_error:     true on failure paths (errored / expired / canceled)
```

A new `Correlator` (e.g. `batchCorrelator`) is constructed on first batch
turn, attached to `Transport.OnControl` with an extractor that matches
`event.Type == "batch_result"`. The pattern is identical to
`extractAsyncToolResult` ([`core/types.go::extractAsyncToolResult`](../../harness/internal/core/types.go))
and `extractPermissionResponse`
([`permission/askupstream.go::extractPermissionResponse`](../../harness/internal/permission/askupstream.go)).

The `controlPlaneBatchClient.Submit/Result` from §2.1 collapses to a
single `Correlator.Await` call — submit is the emit, result arrives on
the matching ControlEvent. There's no separate "polling" step on the
harness side: the control plane is responsible for whatever polling /
webhook subscription it does upstream. *(The harness has zero need to
know which provider mechanism the control plane chose.)*

### 2.4 `AskUpstreamPolicy` as the closest analogue

[`permission/askupstream.go`](../../harness/internal/permission/askupstream.go)
is the cleanest in-repo template:

- It owns its own `Correlator` field, attached to the transport on
  construction in `NewAskUpstreamPolicy`.
- The `Check` method is the canonical "emit + Await" pattern.
- It has a configurable `Timeout` defaulting to 60 s — the batch
  timeout will default much higher (e.g. 24 h, which is the provider
  SLA), but the structural shape is the same.
- The error chain preserves `errors.Is`-distinguishable causes —
  important for the loop to map "batch timed out by harness" vs
  "batch timed out at provider (expired)" vs "ctx cancelled" to
  different outcomes.

The `BatchAdapter` mirrors this 1-to-1, with the timeout coming from
`provider.Batch.MaxWaitSeconds`.

### 2.5 Credential federation

[`credential/source.go::NewSource`](../../harness/internal/credential/source.go)
builds `Source` instances purely from `ProviderConfig.Type` and
`ProviderConfig.Credential`. The batch endpoints share auth with their
streaming counterparts:

- **Anthropic batches** — `POST /v1/messages/batches` accepts
  `x-api-key` + `anthropic-version` headers, identical to the streaming
  request shape in
  [`anthropic.go::AnthropicAdapter.Stream`](../../harness/internal/provider/anthropic.go).
  Resolves via `StaticSource` from `APIKeyRef`.
- **OpenAI batches** — `POST /files` and `POST /batches` use the same
  Bearer token (or Azure `api-key` header) as Chat Completions. Resolves
  via the same auth path
  ([`openai.go::OpenAIAdapter.Stream`](../../harness/internal/provider/openai.go)).
- **Bedrock** — AWS Bedrock batch uses `CreateModelInvocationJob` with
  a JSONL input on S3 (`inputDataConfig.s3InputDataConfig`), not an HTTP
  body. Job timeout is 24–168 hours (per the
  [AWS API reference](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_CreateModelInvocationJob.html)),
  which exceeds the §10 cap of 86 400 s. Adding Bedrock therefore
  requires (a) an S3 abstraction in `BatchClient`, (b) a relaxation of
  the `maxWaitSeconds` invariant gated on provider type, and (c) a
  separate `BatchEntry` shape for the `recordId`/`modelInput` JSONL
  format. **Out of scope for v1.** Recommend punting until concrete user
  demand. Auth would still resolve via `WebIdentityAWSSource`.

**Conclusion:** no new credential type needed. The `BatchClient`
implementations reuse the same `credential.Resolved` per-request that the
streaming adapters already get. *No changes to `credential/`.*

The only new wrinkle is **credential lifetime**. Today
`WebIdentityAWSSource.Resolve` lazy-refreshes via `CredentialsCache`
([`credential/aws.go::WebIdentityAWSSource`](../../harness/internal/credential/aws.go)).
A batch wait can be hours; the polling client must call `Resolve` per
poll-tick, not cache the bearer token at submission time. This is
effectively free because `CredentialsCache` already handles refresh —
but it is an explicit invariant the implementer must respect.

### 2.6 Budget enforcement (`MaxCostBudget` / `MaxTokenBudget`)

[`types/runconfig.go::RunConfig`](../../types/runconfig.go) and
[`core/types.go::TokenTracker`](../../harness/internal/core/types.go)
define the budgets. Today `TokenTracker.CheckBudget` runs before each
turn and again after tool results are appended (see
[`core/loop.go::runInnerLoop`](../../harness/internal/core/loop.go)).

For batch, the cost is only known **after** the batch completes (the
result includes `usage.input_tokens` and `usage.output_tokens` per the
[Anthropic batch processing docs](https://platform.claude.com/docs/en/build-with-claude/batch-processing)
— same shape as the streaming Messages API). Two consequences:

1. **Pre-turn budget check still runs**, using the cumulative tokens
   from previous turns. This catches the case where the harness has
   already exceeded the budget before submitting another batch.
2. **Post-turn check happens only after the wait completes.** A batch
   that submits 10M tokens but the budget allows 5M will cost the
   operator (50 % off) for the over-budget tokens before the loop sees
   the overage and bails. *This is a documented degradation — flag it
   in `docs/sandbox.md` alongside the safety-rings sections.*

Mitigation hooks in this plan but **not implemented in v1**:

- Add a `MaxCostPerSubmission` advisory field that pre-estimates the
  submission's input cost from
  [`core/types.go::estimateCurrentTokens`](../../harness/internal/core/types.go)
  and refuses submission if the per-batch projection would push the
  run over `MaxCostBudget`. Imperfect (output tokens are unknown) but
  non-zero protection.
- Future: aggregate "in-flight batch tokens" across concurrent runs at
  the control plane and apply a fleet-wide budget. Out of scope here.

In bundled mode, by the time the provider batch returns the harness has
already been billed for every entry's tokens. Per-run `MaxCostBudget`
enforcement therefore MUST be performed at the control plane *before*
the entry is bundled into a provider batch. The harness's existing
`TokenTracker.CheckBudget` still runs as a second-line check after the
result arrives, but cannot prevent over-budget billing in bundled mode.

### 2.7 Tracing & lakehouse

[`trace/trace.go`](../../harness/internal/trace/trace.go) and
[`types/runtrace.go::TurnTrace`](../../types/runtrace.go) define the
trace schema. `TurnTrace` already has `DurationMs` — for a 1 h batch
turn this will be 3.6 M ms, which is correct but skews any percentile
aggregation that does not segment by turn-mode.

**Recommended additions** (for a follow-up issue, not v1):

- `TurnTrace.Mode string` — one of `"streaming"` / `"batch"` so the
  lakehouse can compute p50/p95 turn durations *separately* per mode.
  `eval/lakehouse/filestore.go` already has `Metrics` and aggregate
  computations
  ([`filestore.go::FileStore.Metrics`](../../eval/lakehouse/filestore.go))
  — adding a per-mode bucket is a small change.
- `TurnTrace.BatchID string` (omitempty) — the provider's batch ID, so
  operators can cross-reference traces with the provider's console.

For v1, *do not* add fields. Instead, emit two new transport events
during the wait so the trace timeline at least shows the wait window:

- `batch_waiting` HarnessEvent (every 5 minutes, like `heartbeat`) so
  the control plane sees the harness is still alive. Reuses the
  existing 30 s heartbeat in
  [`core/loop.go::runHeartbeat`](../../harness/internal/core/loop.go)
  — the heartbeat keeps firing at 30 s, and `batch_waiting` is the
  *batch-specific* analogue carrying the batch ID.
- `batch_submitted` and `batch_completed` lifecycle events (HarnessEvent
  type) for control-plane UIs that want to render a "waiting on batch"
  state distinct from "waiting on first SSE chunk".

OTel-side: emit one short OTel span per poll tick
(`provider.batch.poll`, attributes `batch.id`, `batch.attempt`,
`batch.status`) plus a final `provider.batch.complete` span carrying
`batch.duration_ms`. This avoids multi-hour spans, which OTel collectors
and storage backends (Tempo, Jaeger) often drop or truncate. The
control plane can reconstruct the timeline from the span chain. Tracer
attribute `provider.batch=true` distinguishes them from
`provider.stream` spans in queries.

---

## 3. Reusing existing request marshalling (Required Q1)

Today's adapters mix request marshalling with HTTP+SSE:
[`anthropic.go::AnthropicAdapter.Stream`](../../harness/internal/provider/anthropic.go)
constructs an `anthropicRequest` value directly inside `Stream`;
[`openai.go::OpenAIAdapter.Stream`](../../harness/internal/provider/openai.go)
does the same with `openaiRequest`. The **batch wrapper must not
duplicate this**.

**Recommended refactor (small, mechanical):** extract a per-adapter
request-builder function:

```go
// in provider/anthropic.go
func buildAnthropicRequest(params types.StreamParams, stream bool) anthropicRequest {
    return anthropicRequest{
        Model:       params.Model,
        System:      params.System,
        Messages:    params.Messages,
        Tools:       params.Tools,
        MaxTokens:   params.MaxTokens,
        Temperature: params.Temperature,
        Stream:      stream,
    }
}
```

Symmetric helpers for `openai.go` (`buildOpenAIRequest`) and
`openai_responses.go` (`buildResponsesRequest`). All three are pure
functions of `StreamParams` and a boolean.

These helpers produce *different* types — `anthropicRequest`,
`openaiRequest`, and `responsesRequest` reflect the underlying API
differences (`system` vs `instructions`, `messages` vs typed `input[]`,
`max_tokens` vs `max_output_tokens`). The `BatchEntry.Body` field is
`json.RawMessage` precisely so the wrapper can hold any of the three.
Pure-function purity per provider is preserved; only the call-site
picks which builder to invoke.

The batch wrapper then consumes them per-provider:

```go
// Sketch.
type BatchEntry struct {
    CustomID string          // "stirrup-<runID>-turn-<n>"
    Provider string          // "anthropic" | "openai-compatible" | "openai-responses"
    Body     json.RawMessage // marshalled anthropicRequest / openaiRequest / responsesRequest
}
```

The batch endpoints accept the **exact same body** as the streaming
endpoints, with `stream: false` (Anthropic does not expose `stream` in
batch params; OpenAI requires `stream: false` for batched chat
completions per the cookbook ref). The shared builder is the canonical
source of truth for "what does a Stirrup turn look like on the wire".

Caveat: Anthropic batch wraps each request in `{ custom_id, params }`
(see the [Anthropic batch processing docs](https://platform.claude.com/docs/en/build-with-claude/batch-processing)),
and OpenAI batch wraps each in `{ custom_id, method, url, body }`.
Those two extra layers of envelope live in the `BatchClient` HTTP code —
**not** in the request-builder. This keeps the request-builder unaware
of "am I being streamed or batched", which is the right separation.

Bedrock and Replay are intentionally left untouched. Bedrock has no
counterpart batch endpoint in scope; Replay is not affected because the
batch wrapper isn't used in replay runs (the recorded `RunRecording`
remembers whether it was a batch run, but the replayer plays it back as
fake events the same way today's `ReplayProvider` does — see §7).

---

## 4. Tool calls in a batch turn (Required Q2)

Batch responses carry the same `tool_use` content blocks as a streaming
response. Per the
[Anthropic batch processing docs](https://platform.claude.com/docs/en/build-with-claude/batch-processing):

> Any request that you can make to the Messages API can be included in
> a batch. This includes: Vision, **Tool use**, System messages,
> Multi-turn conversations, Any beta features.

**The harness handles tool_use in a batch turn identically to a
streaming turn.** The wrapper fabricates `tool_call` StreamEvents from
the completed batch result; the loop's existing `collectToolCalls` and
`dispatchToolCall` paths in
[`core/loop.go::runInnerLoop`](../../harness/internal/core/loop.go) run
unchanged.

What happens *next* is the question Q2 is really asking: does the
follow-up turn (with tool results appended) submit *another* batch, or
does it fall back to streaming?

**Recommendation: another batch, by default.** A `research`/`toil` user
who chose batch is implicitly accepting the latency budget for the
*whole run*. The pricing case for streaming follow-ups inside a batched
run is weak (the cheap turn was the long-context one; follow-ups are
small). Implementation: the `BatchAdapter.Stream` is just called again
on turn N+1; nothing in the loop knows the difference.

One configurable escape:

- `provider.batch.fallbackOnTimeout` (recommended default `false`): if a
  per-turn batch wait *times out at the harness* (not "expired" at the
  provider — that is a different failure), the wrapper switches to the
  streaming adapter for that turn. This trades cost for liveness.

UX note for `research` mode: tools are typically read-only (per
[`runconfig.go::DefaultReadOnlyBuiltInTools`](../../types/runconfig.go)),
so the tool-loop case in research mode is realistic — `web_fetch`,
`read_file`, `search_files`. With single-turn-per-batch, a 5-turn
research run becomes a 5 × 24 h worst-case, but in practice batches
typically complete in <1 h (per the
[Anthropic batch processing docs](https://platform.claude.com/docs/en/build-with-claude/batch-processing)),
so the wall-clock is manageable.

---

## 5. Submission unit (Required Q3)

> Is the unit a single run (one batch entry per run, batched across
> many concurrent stirrup runs at the control plane), or many turns from
> one run (always size 1, just routed to the batch endpoint)?

**Recommendation: a single turn per batch submission, with size 1.**
The control plane is responsible for fanning many concurrent harness
runs into a *real* batch (one provider batch ID containing N entries,
one per run/turn). Reasoning:

| Option | Why we reject / accept |
|---|---|
| **(a) Many turns from one run, batched together** | Reject. Multi-turn turns are a *causal chain* — turn N+1's input depends on turn N's tool results. You cannot meaningfully pre-batch them. |
| **(b) One turn per batch, size 1, fanned at the control plane** | **Accept.** The harness emits `batch_submission` events; the control plane bundles N events from N concurrent runs into one provider batch and demultiplexes the results back to the right runs by `request_id`. The harness is unaware of the bundling. |
| **(c) One turn per batch, size 1, sent directly to provider** | Acceptable as the stdio-mode degraded path. A size-1 batch still gets the 50 % discount per the [Anthropic batch processing pricing table](https://platform.claude.com/docs/en/build-with-claude/batch-processing). It's just paying the per-batch HTTP-call overhead and missing whatever throughput allocation the provider applies to large batches; the per-token discount is unaffected by batch size. |

This is the *only* sensible architecture given that:

- Stirrup runs are independent processes (K8s Jobs) — they cannot
  introspect each other to bundle.
- The control plane already brokers cross-run state.
- The `request_id` correlator pattern (§2.3) makes demultiplexing
  trivial.

The cost-savings story is therefore *only* realised when a control plane
is present and bundling. For lone stdio users the stdio polling client
still gets the discount per-token (no bundling needed); they just don't
realise the throughput uplift.

---

## 6. Polling vs webhooks (Required Q4)

| Provider | Mechanism (verified 2026-05-03) | Where it terminates |
|---|---|---|
| Anthropic | Polling on `processing_status`. **No native webhooks for Message Batches at the time of writing.** | Polling at the control plane. |
| OpenAI | Webhooks (`batch.completed` and lifecycle companions). Polling fallback also available. | Webhooks at the control plane; polling fallback also at the control plane. |

> **Verified 2026-05-03:** Anthropic Message Batches has no documented
> webhook product (the speculated
> `platform.claude.com/.../webhooks` page returns 404). OpenAI Batch is
> webhook-enabled (`batch.completed`). The harness design is unaffected
> — it sees only `batch_result` ControlEvents and is agnostic to the
> upstream mechanism — but the control plane should implement the
> OpenAI webhook subscriber first and the Anthropic poller as the only
> path for that provider.

**Webhooks are mandatory at the control plane**, not the harness, for
three reasons:

1. The harness is a short-lived K8s Job (`VERSION1.md` §Architecture).
   It cannot expose a public ingress; pods are ephemeral.
2. Webhook signature verification needs a long-lived secret. Stirrup
   never wants long-lived inbound secrets in the harness — the entire
   security posture (`security/inputvalidator.go`,
   `RunConfig.Redact()`) is built around outbound-only credentials.
3. A control plane already has a public ingress and a stable domain.

**Harness-only stdio fallback (Required Q6 also):** when
`transport=stdio` *and* `provider.batch.enabled=true`, the harness
itself polls. Polling cadence: exponential backoff with jitter — 10 s
initial, doubling to a 5 min cap, with ±20 % jitter on each interval to
avoid herding multiple harnesses against the provider's batch-status
endpoint. Hard cap: `MaxWaitSeconds`, defaulting to 24 h (matching the
provider SLA).

Setting `MaxWaitSeconds` below the provider's minimum completion
window does NOT cause the provider to finish faster. It is a
harness-side hard timeout; the upstream batch continues until the
provider's own deadline (24 h for both Anthropic and OpenAI today, see
the [OpenAI Batch FAQ](https://help.openai.com/en/articles/9197833-batch-api-faq))
and is billed regardless. In `harnessSidePolling=true` mode, the
harness MUST best-effort call the upstream batch-cancel endpoint when
its `MaxWaitSeconds` fires.

The stdio-mode polling client is **opt-in** behind a separate flag
`provider.batch.harnessSidePolling=true`, defaulting to `false`. With
the flag off and `transport=stdio`, `ValidateRunConfig` MUST reject the
combination with a clear error: *"batch requires either transport=grpc
(control plane bundles + polls) or provider.batch.harnessSidePolling=true
(this process polls directly)"*. This forces the operator to make the
choice explicit rather than silently incurring 24 h harness waits.

---

## 7. Failure & cancellation (Required Q5)

Cancellation paths:

| Trigger | Existing behaviour ([`core/loop.go::cancelRun`](../../harness/internal/core/loop.go)) | Batch adjustment |
|---|---|---|
| `cancel` ControlEvent during batch wait | `cancelRun(ErrCancelledByControlPlane)` cancels the run ctx | The wrapper's correlator unblocks via ctx.Done; the wrapper then **best-effort calls the batch cancel endpoint** (`POST /v1/messages/batches/{id}/cancel` or `POST /batches/{id}/cancel`) **only when in stdio polling mode**. In control-plane mode the harness emits a `batch_cancel_request` HarnessEvent and the control plane is responsible for the upstream cancel. |
| `ctx.DeadlineExceeded` (run timeout) | Same path as above (classified `timeout`) | Same as cancel. The provider batch is left to expire on its own (24 h) unless cancelled — which is fine because the operator is no longer being charged for tokens the model has not yet generated. |
| Harness restart (K8s pod replacement) | Run is lost; control plane treats as failed | **Batch is lost from the harness perspective** but **survives at the provider**. The control plane should record the batch ID at submission time so a replacement harness can be started with `--resume-batch <id>` (out of scope for v1; documented as a follow-up). |
| Batch partial failure (some entries succeed, others don't) | N/A — Stirrup submits size-1 entries, so partial failure collapses to total failure for the run. | The wrapper inspects the result type per the [Anthropic batch processing docs](https://platform.claude.com/docs/en/build-with-claude/batch-processing): `succeeded` → fabricate stream; `errored` → emit StreamEvent{Type: "error"} with the provider error message; `canceled` → `outcome=cancelled`; `expired` → new outcome `"batch_expired"`. |

A new outcome value `"batch_expired"` is added to the canonical list in
[`types/runtrace.go::Outcome`](../../types/runtrace.go) and the proto
comment in `proto/harness/v1/harness.proto`. Distinct from `timeout`
(which is the harness wall-clock timeout) and `error` (which is
"couldn't even submit").

### Cancellation in bundled mode (gRPC transport)

When the control plane bundles N concurrent harness submissions into
one provider batch, neither Anthropic nor OpenAI exposes a per-entry
cancel — the batch-cancel endpoint cancels the whole batch.

Default policy: a single run's `cancel` ControlEvent unblocks that run
at the harness boundary (the wrapper's `Correlator.Await` returns ctx
error) and emits `batch_cancel_request` upstream, but the control
plane MUST NOT propagate this to a provider batch-cancel call when
other runs are still bundled with it. The cancelled run is billed for
its share of the batch tokens; this is documented behaviour in
`docs/sandbox.md`.

Opt-in alternative `provider.batch.cancelBundleOnRunCancel` (default
`false`): a run's cancel cancels the entire bundled batch. Other runs
in the same bundle observe outcome `batch_cancelled`. Intended for
tightly-coupled job groups where one run failing means the whole job
is dead.

In control-plane mode, the *control plane* observes the webhook event
and translates it into a `batch_result` ControlEvent with
`is_error=true` and `content` carrying the structured error — the
harness reuses its existing `extractAsyncToolResult`-style error path,
no new control-flow needed.

---

## 8. Stdio mode (Required Q6)

A stdio harness has no inbound channel for `batch_result` ControlEvents
because stdin in stdio mode is treated as a `user_response`/`cancel`
queue ([`transport/stdio.go::StdioTransport`](../../harness/internal/transport/stdio.go)).
Two options:

1. **Hard requirement: batch needs `transport=grpc`.** Cleanest, lowest
   surface area, but kills the local-CLI use case for `stirrup harness
   --provider anthropic --batch true ...`.
2. **Single-process polling mode.** The harness itself runs the batch
   client in-process: submits, polls, fabricates the stream, emits
   `batch_waiting` heartbeats to stdout in the meantime so the operator
   sees something is happening.

**Recommendation: support both, with stdio-polling opt-in.** A local CLI
user running an overnight `toil` job has a legitimate reason to want
this; we don't want to force them through a control plane to get the
50 % discount. But the default must be safe: if the operator sets
`provider.batch.enabled=true` and `transport=stdio` *without*
`provider.batch.harnessSidePolling=true`, `ValidateRunConfig` MUST
reject the configuration with a directing error message.

The stdio polling client lives in
`harness/internal/provider/batchpoll.go` (new). It implements the
`BatchClient` interface from §2.1 (the size-1 case is a thin wrapper
over the multi-entry `Submit`/`Result` shape) and is selected by the
factory when both `transport.type=stdio` and
`provider.batch.harnessSidePolling=true`. HTTP code is hand-rolled per
the project's minimal-dependency philosophy (`CLAUDE.md` "External
dependencies rationale"). Polling cadence: 10 s start, exponential to
5 min with ±20 % jitter, ctx-cancelled on run cancel.

---

## 9. Eval & replay (Required Q7)

[`provider/replay.go::ReplayProvider`](../../harness/internal/provider/replay.go)
replays `TurnRecord.ModelOutput` as fake stream events. **Batch turns
should be replayed identically to streaming turns**, because the
replayer is testing the *loop's* behaviour given a model response, not
the wire mechanism.

Two adjustments to the eval framework:

1. **Distinguish in the trace, not the replay.** Add
   `TurnTrace.Mode string` (`"streaming"` | `"batch"` —
   pre-populated from the BatchAdapter wrapper at turn-record time) so
   `eval mine-failures`
   ([`eval/cmd/eval/`](../../eval/cmd/eval)) does not surface a 24 h
   batch turn as a "misleading wall-clock" failure. The `mine-failures`
   command uses `RunRecording.FinalOutcome.Outcome` and turn metrics;
   adding mode-aware filtering keeps batch turns out of the
   "stalled/slow" buckets.
2. **Replay is wire-mechanism-agnostic.** `ReplayProvider` does not
   implement `BatchProviderAdapter`; it only implements
   `ProviderAdapter.Stream`. The wrapper-shape choice (§2.1) means
   replay paths never see the batch wrapper at all — the recorded
   `ModelOutput` is replayed as if it had streamed. This is correct: an
   eval suite that mines a real production batch turn is testing
   "given this model output, does the loop do the right thing?", not
   "does HTTP polling work?".

For `eval drift` and `compare-to-production`
(`eval/cmd/eval/main.go`), bucket metrics by
`config.provider.batch.enabled` so a fleet whose batch share grows over
time doesn't trip the drift detector on duration deltas.

---

## 10. RunConfig surface (Required Q8)

New nested struct `BatchProviderConfig` on `ProviderConfig`:

```jsonc
// Excerpt from a RunConfig — full example at examples/runconfig/batch.json (to be created).
{
  "provider": {
    "type": "anthropic",
    "apiKeyRef": "secret://ANTHROPIC_API_KEY",
    "batch": {
      "enabled": true,
      "maxWaitSeconds": 86400,
      "harnessSidePolling": false,
      "fallbackOnTimeout": false,
      "cancelBundleOnRunCancel": false,
      "allowInteractiveModes": false
    }
  },
  "mode": "research",
  "transport": { "type": "grpc", "address": "control-plane.local:8443" },
  "tools": { "builtIn": ["read_file", "list_directory", "search_files", "web_fetch"] },
  "maxTurns": 8,
  "timeout": 3600
}
```

Go shape (sketch):

```go
type ProviderConfig struct {
    // ... existing fields ...
    Batch *BatchProviderConfig `json:"batch,omitempty"`
}

type BatchProviderConfig struct {
    Enabled                 bool `json:"enabled,omitempty"`
    // MaxWaitSeconds is a *int (matching MaxTokenBudget / Timeout in
    // RunConfig) so a missing value is wire-distinguishable from the
    // explicit 0 the invariant table rejects. ValidateRunConfig applies
    // the 86400 default when the pointer is nil and Enabled=true.
    MaxWaitSeconds          *int `json:"maxWaitSeconds,omitempty"`
    HarnessSidePolling      bool `json:"harnessSidePolling,omitempty"`      // required when transport=stdio
    FallbackOnTimeout       bool `json:"fallbackOnTimeout,omitempty"`       // fall back to streaming on harness-side timeout
    CancelBundleOnRunCancel bool `json:"cancelBundleOnRunCancel,omitempty"` // grpc-only: a single run's cancel cancels the bundled batch
    // AllowInteractiveModes permits batch.enabled with mode == "planning"
    // or mode == "review". Has no effect on mode == "execution"
    // (always rejected).
    AllowInteractiveModes   bool `json:"allowInteractiveModes,omitempty"`
}
```

Validation (added to
[`runconfig.go::ValidateRunConfig`](../../types/runconfig.go)):

> **Conventions.** "MUST", "MUST NOT", "SHOULD", "MAY" in this section
> are used per [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) and
> [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174).

| Invariant | Rationale |
|---|---|
| `batch.enabled && mode == "execution"` MUST be rejected by `ValidateRunConfig` | Interactive mode; 24 h latency unacceptable. |
| `batch.enabled && (mode == "planning" \|\| mode == "review") && !batch.allowInteractiveModes` MUST be rejected | Default-deny for these modes, but operator can override. |
| `batch.enabled && transport.type == "stdio" && !batch.harnessSidePolling` MUST be rejected | No inbound channel; force the operator to be explicit. |
| `batch.maxWaitSeconds != nil && (*batch.maxWaitSeconds <= 0 \|\| *batch.maxWaitSeconds > 86400)` MUST be rejected | Bound the wait to the provider SLA. Note: setting it below the provider's `completion_window` (24 h for both providers) does NOT make the upstream batch finish faster — it is a harness-side hard timeout. |
| `batch.harnessSidePolling && transport.type == "grpc"` MUST be rejected | Don't run two concurrent batch clients. |
| `batch.cancelBundleOnRunCancel && transport.type == "stdio"` MUST be rejected | The flag is meaningless without a control plane to bundle. |

Cross-cutting interaction with **#42 safety rings**:

- `RuleOfTwo` (see [`runconfig.go::validateRuleOfTwo`](../../types/runconfig.go)):
  a long batch wait (hours) is a longer-lived credential exposure than
  a 30 s stream. Adding
  `provider.Batch.Enabled=true` *should not* itself trigger the
  Rule-of-Two (it's not a new sensitivity axis), but the implementer
  must double-check `ruleOfTwoSensitiveData` heuristics still hold.
  Recommendation: leave the rule unchanged for v1, document the longer
  exposure window in `docs/sandbox.md`.
- `CodeScanner`: only relevant in `execution`, which is not batch-eligible.
  No interaction.
- Cedar policies: a Cedar action `Action::"batch:submit"` could gate
  batch submission per-run. Sketch only — not v1.

---

## 11. Sketched event types (Required by §2.3)

In `proto/harness/v1/harness.proto`, add to the `HarnessEvent`
discriminator comment block:

```
//   "batch_submission"
//     - request_id: unique ID for this batch entry; the control plane
//                   must echo it back in the corresponding batch_result
//                   ControlEvent.
//     - input:      JSON-encoded BatchSubmission payload describing the
//                   provider type, model, messages, tools, system, and
//                   max_tokens. The control plane bundles many
//                   batch_submission events from concurrent runs into a
//                   single provider-side batch.
//
//   "batch_waiting"
//     - request_id: the originating batch_submission's request_id.
//     (Heartbeat-style event during the wait; emitted every 5 minutes.)
//
//   "batch_cancel_request"
//     - request_id: the originating batch_submission's request_id.
//     (Best-effort upstream cancel signal; the control plane should
//     forward to the provider's batch-cancel endpoint.)
```

And on `ControlEvent`:

```
//   "batch_result"
//     - request_id: must match a previously received
//                   batch_submission HarnessEvent.request_id.
//     - content:    JSON-encoded BatchResult payload (success: full
//                   Messages-API response; error: structured error type
//                   from { invalid_request_error, server_error,
//                   batch_expired, batch_cancelled }).
//     - is_error:   true for non-success result types.
```

No proto field changes are required: `batch_submission`,
`batch_waiting`, `batch_cancel_request`, and `batch_result` reuse
`request_id`, `input`, and `content` on the existing `HarnessEvent` /
`ControlEvent` shapes; only the type discriminator strings are new. The
proto comment block in `proto/harness/v1/harness.proto` SHOULD be
updated alongside the implementation to document the new discriminator
values. This is by design: the existing union-on-`type` shape already
absorbs batch as a 4th correlator pattern (after `permission_*`,
`tool_result_*`, `user_response`).

---

## 12. Control plane responsibilities vs harness responsibilities

| Responsibility | Owner | Notes |
|---|---|---|
| Marshalling per-turn request body | **Harness** | Reuses the per-adapter `build*Request` helpers from §3. |
| Emitting `batch_submission` events | **Harness** | Via the `BatchAdapter.Stream` wrapper. |
| Bundling N concurrent submissions into a real provider batch | **Control plane** | The discount only matters with bundling; harness is single-tenant. |
| Calling `POST /v1/messages/batches` (Anthropic) or `POST /files`+`POST /batches` (OpenAI) | **Control plane** | Owns the provider creds for the batch endpoints (the harness's `secret://` ref is also valid here, but bundling means the control plane is the single submitter). |
| Polling / webhook subscription | **Control plane** | See §6. Webhooks at the control plane only. |
| Demultiplexing batch results to the right run | **Control plane** | Maps provider `custom_id` (which the harness sets to `request_id`) back to the harness gRPC stream. |
| Emitting `batch_result` ControlEvents | **Control plane** | One per harness run, even if 100 runs were bundled. |
| Cancellation upstream | **Control plane** in grpc mode; **harness** in stdio polling mode | See §7. |
| Recording `batch_id` in the trace | **Harness** | Trace stays self-contained; the lakehouse can join on `batch_id` for cross-run analysis. |
| Cost aggregation | **Control plane** | `MaxCostBudget` is enforced per-run inside the harness; fleet-wide cost is upstream. |
| Per-run pre-submission cost projection | **Control plane** in grpc mode; **harness** in stdio polling mode | Without it, bundled batches can blow per-run `MaxCostBudget` silently (see §2.6). |

### Harness-only degraded mode (`transport=stdio`)

When the operator sets:

```json
{
  "provider": {
    "type": "anthropic",
    "batch": { "enabled": true, "harnessSidePolling": true }
  },
  "transport": { "type": "stdio" }
}
```

the harness:

1. Builds the same request body as a streaming turn (§3) but submits
   directly to `POST /v1/messages/batches` (size-1 batch).
2. Polls `GET /v1/messages/batches/{id}` every 10 s → 5 min (exponential
   backoff with ±20 % jitter) using the same HTTP client family as
   [`anthropic.go::AnthropicAdapter`](../../harness/internal/provider/anthropic.go),
   with adjusted timeouts (poll requests are ~30 s, not 120 s).
3. On `processing_status=ended`, fetches `results_url`, parses the JSONL
   line for the run's `custom_id`, and fabricates the `StreamEvent` flow.
4. Emits a `batch_waiting` HarnessEvent (carrying `batchId` and
   elapsed-seconds) every 5 minutes so a control plane or interactive
   operator sees progress; `heartbeat` events continue at 30 s. (Never
   use `text_delta` for operator-only feedback — it would pollute the
   recorded `ModelOutput` and replay path.)
5. On harness exit (SIGINT / cancel), best-effort calls
   `POST /v1/messages/batches/{id}/cancel`. The provider has its own 24 h
   garbage collection so a missed cancel just leaks the batch (no
   compute spent past cancellation).

The stdio polling path **cannot** realise the bundling throughput
benefit, but the per-token discount still applies. The path is
documented in `docs/sandbox.md` as "experimental — recommended path is
the control plane".

---

## 13. Phased implementation outline

This is the implementation order an implementer should follow. Each
phase ends in a green build and is independently testable.

### Phase 0 — refactor (no behaviour change)
- Extract `buildAnthropicRequest` / `buildOpenAIRequest` /
  `buildResponsesRequest` helpers from the existing adapters. Pure
  refactor; existing `Stream` calls them. Adds one property test per
  helper: for any `StreamParams`, `Stream(params)` and
  `buildRequest(params, true)` MUST produce byte-identical request
  bodies. Property tests survive minor request-shape additions (e.g.
  new optional fields) where golden fixtures would force a churn diff
  per change.
- *Touches:* `harness/internal/provider/{anthropic,openai,openai_responses}.go`
- *No proto changes; no RunConfig changes.*

### Phase 1 — RunConfig surface + validation
- Add `BatchProviderConfig` to `types/runconfig.go`. Wire up
  `ValidateRunConfig` to enforce the §10 invariants.
- Add JSON schema test cases.
- *Touches:* `types/runconfig.go`, `types/runconfig_test.go`,
  `examples/runconfig/full.json` (add a commented-out batch block).
- *Outcome:* `--config` files can carry `batch.*` fields; nothing yet
  consumes them.

### Phase 2 — `BatchAdapter` + correlator + `controlPlaneBatchClient`
- New file `harness/internal/provider/batch.go` implementing the
  wrapper. Uses a freshly attached `transport.Correlator` (mirroring
  `permission/askupstream.go::NewAskUpstreamPolicy`).
- New events `batch_submission`, `batch_waiting`, `batch_result`,
  `batch_cancel_request` recognised in
  `transport/grpc_translate.go` and `transport/stdio.go`. **Proto
  comment block updated** (Required Q from #67: proto can be
  recommended, but the plan does not produce them — defer the actual
  generated-code change to a follow-up issue.)
- Factory wiring in `harness/internal/core/factory.go` to wrap the
  selected `ProviderAdapter` in `BatchAdapter` when
  `provider.batch.enabled && transport.type=="grpc"`.
- *Outcome:* end-to-end gRPC run with the control plane round-tripping
  `batch_submission` ↔ `batch_result`; can be exercised with a faked
  control plane in tests.

### Phase 3 — CLI flag + docs
- `--batch` boolean flag on `stirrup harness` mapping to
  `provider.batch.enabled`. (Other batch knobs only via `--config`.)
- New section in `docs/sandbox.md` covering the cost/latency tradeoff,
  the credential-lifetime caveat (§2.5), and the budget-degradation
  caveat (§2.6).
- *Outcome:* operator can opt in via flag.

### Phase 4 — `harnessPollingBatchClient` (stdio fallback)
- New file `harness/internal/provider/batchpoll.go` implementing
  `BatchClient` against `POST /v1/messages/batches` (Anthropic only for
  v1; OpenAI added in a follow-up).
- Factory selects this client when `transport.type=="stdio" &&
  provider.batch.harnessSidePolling`.
- *Outcome:* local CLI `stirrup harness --batch true --transport stdio
  --provider anthropic ...` works.

### Phase 5 — eval mode tagging
- Add `TurnTrace.Mode` field, populate from `BatchAdapter`. Update
  `eval/lakehouse/filestore.go` aggregations to bucket by mode.
  Update `eval mine-failures` filter to skip `mode=batch` turns by
  default (with a `--include-batch` flag).
- *Outcome:* batch turns no longer pollute streaming-mode percentile
  metrics.

### Phase 6 — OpenAI batch + Bedrock evaluation
- Add OpenAI batch path to `harnessPollingBatchClient` (file upload then
  batch creation, per the cookbook). With the multi-entry `BatchClient`
  shape from §2.1 this is a strict subset of the existing interface, not
  a refactor.
- Evaluate Bedrock `CreateModelInvocationJob` separately. Per §2.5,
  Bedrock batch uses a JSONL input on S3 (`inputDataConfig.s3InputDataConfig`),
  not an HTTP body, with a job timeout of 24–168 hours that exceeds the
  §10 cap of 86 400 s. A Bedrock batch path therefore requires (a) an
  S3 abstraction in `BatchClient`, (b) a relaxation of the
  `maxWaitSeconds` invariant gated on provider type, and (c) a separate
  `BatchEntry` shape for the `recordId`/`modelInput` JSONL format.
  Recommend punting until a concrete user request.

### Out of phases (not in v1, kept here for completeness)
- Cedar action `Action::"batch:submit"` and policy templates.
- `--resume-batch <id>` (harness restart recovery).
- Per-batch cost projection / pre-flight guard.
- Fleet-wide cost aggregation.
- `provider.batch.firstTurnOnly` (batch turn 0 only, stream the rest).
  Considered for v1 but cut: the doc itself characterised it as a
  footgun (operators may believe they are saving 50 % overall when in
  fact the saving applies to the first turn alone). Add only when a
  concrete user requests it.

---

## 14. Risks and unanswered questions

- **Webhook coverage by provider.** Verified 2026-05-03: OpenAI Batch
  ships native webhooks (`batch.completed` and lifecycle companions);
  Anthropic Message Batches has no documented webhook product (the
  speculated `platform.claude.com/.../webhooks` page returns 404). The
  plan's design works whether or not webhooks exist, because polling is
  always available and the harness never sees webhooks directly.
  *Implementer should re-verify both providers' webhook offerings
  before writing the control-plane subscriber.*
- **Cost overrun on the last turn.** §2.6's gap is real and unavoidable
  in v1: the operator can blow the budget by exactly one turn's worth
  of tokens. Document loudly.
- **Long-lived credential exposure.** §2.5's note: a 24 h batch wait is
  a 24 h SSM parameter exposure. Today's cap is 30 s of streaming.
  This is a security-rings question more than a batch question; flag in
  `docs/sandbox.md` so it gets reviewed.
- **`MaxTurns` semantics.** The default 20-turn run × 24 h is 480 hours
  worst-case. Recommendation: add a soft warning in `ValidateRunConfig`
  when `batch.enabled && maxTurns > 5` (analogous to the rule-of-two
  warning emitter). Hard rejection remains a control-plane policy
  decision — Cedar action `Action::"batch:submit"` with a
  `principal.maxTurns < 5` check is the natural point.
- **Cancellation race in stdio polling mode.** If the harness is killed
  with SIGKILL (not SIGINT), the upstream cancel call is missed. Cost
  exposure: zero (model has not yet generated tokens), but the operator
  may see a stale "processing" batch in their console for 24 h.
  Acceptable.

---

## 15. Sources & references

External:

- Anthropic, *Batch processing*,
  [`platform.claude.com/docs/en/build-with-claude/batch-processing`](https://platform.claude.com/docs/en/build-with-claude/batch-processing)
  — fetched 2026-05-03. Source for SLA, status enumeration, request
  envelope, result-type table, usage-token shape, and pricing.
- Anthropic, *Webhooks*,
  [`platform.claude.com/docs/en/build-with-claude/webhooks`](https://platform.claude.com/docs/en/build-with-claude/webhooks)
  — **verified missing as of 2026-05-03** (page returns 404). Anthropic
  Message Batches has no native webhook product at the time of writing.
- OpenAI, *Batch reference*,
  [`platform.openai.com/docs/api-reference/batch`](https://platform.openai.com/docs/api-reference/batch)
  — fetched 2026-05-03. Used for endpoint/status enumeration and
  `error_file_id` partial-failure model.
- OpenAI, *Cookbook: batch_processing*,
  [`developers.openai.com/cookbook/examples/batch_processing`](https://developers.openai.com/cookbook/examples/batch_processing)
  — fetched 2026-05-03. Used for the JSONL input shape (`custom_id`,
  `method`, `url`, `body`) and the `client.files.create(purpose="batch")`
  + `client.batches.create(...)` flow.
- OpenAI, *Webhooks events*,
  [`platform.openai.com/docs/api-reference/webhook-events`](https://platform.openai.com/docs/api-reference/webhook-events)
  — fetched 2026-05-03. Source for `batch.completed` and lifecycle
  companion events.
- OpenAI, *Webhooks guide*,
  [`developers.openai.com/api/docs/guides/webhooks`](https://developers.openai.com/api/docs/guides/webhooks)
  — fetched 2026-05-03. Used for signature-verification posture.
- OpenAI, *Batch FAQ*,
  [`help.openai.com/en/articles/9197833-batch-api-faq`](https://help.openai.com/en/articles/9197833-batch-api-faq)
  — fetched 2026-05-03. Source for the fixed `completion_window=24h`.
- AWS, *CreateModelInvocationJob*,
  [`docs.aws.amazon.com/bedrock/latest/APIReference/API_CreateModelInvocationJob.html`](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_CreateModelInvocationJob.html)
  — fetched 2026-05-03. Used for the Bedrock-batch deferral rationale
  in §2.5 and §13 Phase 6.

Codebase:

- [`harness/internal/provider/provider.go::ProviderAdapter`](../../harness/internal/provider/provider.go) — adapter interface
- [`harness/internal/provider/anthropic.go::AnthropicAdapter.Stream`](../../harness/internal/provider/anthropic.go) — Anthropic streaming impl
- [`harness/internal/provider/openai.go::OpenAIAdapter`](../../harness/internal/provider/openai.go) — OpenAI Chat Completions wire types and Stream impl
- [`harness/internal/provider/openai_responses.go::OpenAIResponsesAdapter`](../../harness/internal/provider/openai_responses.go) — OpenAI Responses wire types and Stream impl
- [`harness/internal/provider/replay.go::ReplayProvider`](../../harness/internal/provider/replay.go) — replay provider
- [`harness/internal/transport/transport.go`](../../harness/internal/transport/transport.go) — Transport interface
- [`harness/internal/transport/correlator.go::Correlator`](../../harness/internal/transport/correlator.go) — request/response correlator (the canonical async-await pattern)
- [`harness/internal/permission/askupstream.go::AskUpstreamPolicy`](../../harness/internal/permission/askupstream.go) — closest analogue to "block on a control-plane decision"
- [`harness/internal/core/loop.go::runInnerLoop`](../../harness/internal/core/loop.go) — turn-based agentic loop the wrapper must not perturb
- [`harness/internal/core/types.go::dispatchAsyncToolCall`](../../harness/internal/core/types.go) — existing precedent for "emit + Await + classify error"
- [`types/events.go::HarnessEvent`](../../types/events.go) — `HarnessEvent` / `ControlEvent`
- [`types/runconfig.go::ValidateRunConfig`](../../types/runconfig.go) — RunConfig validator
- [`types/runtrace.go::RunTrace`](../../types/runtrace.go) — `RunTrace` / `TurnTrace`
- [`proto/harness/v1/harness.proto`](../../proto/harness/v1/harness.proto) — current event shape
- [`eval/lakehouse/filestore.go::FileStore`](../../eval/lakehouse/filestore.go) — file-store lakehouse aggregations
- `VERSION1.md` — for the harness's "short-lived job, not a server" posture
- `CLAUDE.md` — for the minimal-dependency philosophy that constrains the wire client choices.
