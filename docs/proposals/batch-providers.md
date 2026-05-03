# Batch message API support — implementation plan

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
2. Treat batch as **single-turn-per-batch**: the harness submits one
   batch entry per turn, blocks the `core.AgenticLoop` until the result
   arrives, then runs the rest of the turn (tool dispatch, message
   appending, next turn) exactly as today. This keeps multi-turn tool
   loops compatible with batch, at the cost of N×24 h worst-case for
   multi-turn runs — which is acceptable in `research`/`toil`.
3. **Submission unit = single run**, never "many turns from one run":
   batch entries are routed through the harness's existing
   `tool_result_request`/`tool_result_response` correlator pattern by
   adding two new event types — `batch_submission` (HarnessEvent) and
   `batch_result` (ControlEvent) — and the control plane is responsible
   for fanning many concurrent harness runs into the provider's true
   batch endpoint when it wants the cost discount.
4. The control plane terminates Anthropic webhooks and OpenAI polling.
   Stdio users get a degraded **harness-side polling** fallback, gated
   off by default and explicitly documented as such.
5. New RunConfig surface lives under `ProviderConfig.Batch`:
   `enabled`, `maxWaitSeconds`, `fallbackOnTimeout`. JSON shape and
   validation invariants are detailed in §10 below.

A phased rollout (events first, then provider wrapper, then CLI flag,
then control-plane integration, then stdio polling fallback) is at the
end.

---

## 1. Background

Stirrup's provider layer is hand-rolled HTTP/SSE against documented REST
APIs (see `harness/internal/provider/anthropic.go:103-171`,
`openai.go:300-450`, `bedrock.go`, `openai_responses.go:90-103`). The
`ProviderAdapter` interface is a single method
([`provider.go:12-14`](../../harness/internal/provider/provider.go)):

```go
type ProviderAdapter interface {
    Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error)
}
```

`StreamEvent` discriminates on `text_delta` / `tool_call` /
`message_complete` / `error` ([`types/events.go:21-31`](../../types/events.go)).
The agentic loop in
[`harness/internal/core/loop.go:500-602`](../../harness/internal/core/loop.go)
calls `Stream`, drains the channel via `streamEventsToResult`
([`core/types.go:443-508`](../../harness/internal/core/types.go)), and
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

[`harness/internal/provider/provider.go:12-14`](../../harness/internal/provider/provider.go)
defines the existing one-method interface. Three integration shapes are
possible:

| Shape | Pros | Cons | Recommendation |
|---|---|---|---|
| **A. Add `Submit`/`Poll` methods to `ProviderAdapter`** | Single interface, simple `if cfg.Batch.Enabled` dispatch in the loop. | Forces every existing adapter (Bedrock, OpenAI Chat, OpenAI Responses, Replay) to grow stub no-op methods. Replay is especially awkward — there is no notion of "submission" in a fixture. The loop has to learn a new control-flow path. | Reject. |
| **B. Sibling `BatchProviderAdapter` interface, switched on per turn** | Clean separation of concerns; replay/sub-agent paths untouched. | The loop needs new control-flow that isn't `Stream`. | Reject as the *primary* shape because of loop churn. |
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
        //    builder (see §3 below) and emit batch_submission.
        // 2. Block on batch_result via the correlator.
        // 3. On success, fabricate text_delta/tool_call events from the
        //    completed Messages-API response, then a message_complete.
        //    On failure, emit a single error event with the result's
        //    error.type ("invalid_request_error", "expired", etc.).
    }()
    return ch, nil
}
```

`BatchClient` is a small internal interface owned by the wrapper:

```go
type BatchClient interface {
    Submit(ctx context.Context, entry BatchEntry) (id string, err error)
    Result(ctx context.Context, id string) (*BatchResult, error)
}
```

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

[`types/runconfig.go:506-514`](../../types/runconfig.go) defines five
modes; `IsReadOnlyMode` returns true for `planning`, `review`,
`research`, `toil`. The mode→batch eligibility matrix:

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
defaulting-but-permitting overrides (see `RuleOfTwo.Enforce`,
`CodeScanner` mode-aware default at `runconfig.go:892-901`).

Re. the issue's note that "the agentic loop is fundamentally turn-based":
correct, and confirmed against
[`core/loop.go:375-745`](../../harness/internal/core/loop.go). Each turn
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
   [`permission/askupstream.go:120-159`](../../harness/internal/permission/askupstream.go)
   emits a `permission_request` HarnessEvent, blocks on the correlator,
   unblocks on `permission_response` keyed by `RequestID`. The
   correlator itself is in
   [`transport/correlator.go`](../../harness/internal/transport/correlator.go)
   and supports any number of concurrent awaits, configurable timeout,
   and ctx cancellation.
2. **Async tool dispatch** —
   [`core/types.go:282-379`](../../harness/internal/core/types.go) emits
   `tool_result_request` and blocks on `tool_result_response` via a
   loop-owned correlator (`asyncCorrelator`,
   [`core/types.go:130-149`](../../harness/internal/core/types.go)).
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
`extractAsyncToolResult` ([`core/types.go:110-123`](../../harness/internal/core/types.go))
and `extractPermissionResponse`
([`permission/askupstream.go:85-94`](../../harness/internal/permission/askupstream.go)).

The `controlPlaneBatchClient.Submit/Result` from §2.1 collapses to a
single `Correlator.Await` call — submit is the emit, result arrives on
the matching ControlEvent. There's no separate "polling" step on the
harness side: the control plane is responsible for whatever polling /
webhook subscription it does upstream. *(The harness has zero need to
know which provider mechanism the control plane chose.)*

### 2.4 `AskUpstreamPolicy` as the closest analogue

[`permission/askupstream.go`](../../harness/internal/permission/askupstream.go)
is the cleanest in-repo template:

- It owns its own `Correlator` (line 56) attached to the transport on
  construction (line 80).
- The `Check` method (lines 124–159) is the canonical "emit + Await"
  pattern.
- It has a configurable `Timeout` defaulting to 60 s (line 16) — the
  batch timeout will default much higher (e.g. 24 h, which is the
  provider SLA), but the structural shape is the same.
- The error chain preserves `errors.Is`-distinguishable causes (line
  144) — important for the loop to map "batch timed out by harness" vs
  "batch timed out at provider (expired)" vs "ctx cancelled" to
  different outcomes.

The `BatchAdapter` mirrors this 1-to-1, with the timeout coming from
`provider.Batch.MaxWaitSeconds`.

### 2.5 Credential federation

[`credential/source.go:57-82`](../../harness/internal/credential/source.go)
builds `Source` instances purely from `ProviderConfig.Type` and
`ProviderConfig.Credential`. The batch endpoints share auth with their
streaming counterparts:

- **Anthropic batches** — `POST /v1/messages/batches` accepts
  `x-api-key` + `anthropic-version` headers, identical to
  [`anthropic.go:132-134`](../../harness/internal/provider/anthropic.go).
  Resolves via `StaticSource` from `APIKeyRef`.
- **OpenAI batches** — `POST /files` and `POST /batches` use the same
  Bearer token (or Azure `api-key` header) as Chat Completions. Resolves
  via the same auth path
  ([`openai.go:80-89`](../../harness/internal/provider/openai.go)).
- **Bedrock** — AWS does not expose a public Bedrock batch API in the
  same shape (it has `CreateModelInvocationJob` but the wire format is
  different). **Out of scope for v1.** The plan should be additive: if
  Bedrock batch is ever added, it would use the same `WebIdentityAWSSource`
  flow as streaming Bedrock.

**Conclusion:** no new credential type needed. The `BatchClient`
implementations reuse the same `credential.Resolved` per-request that the
streaming adapters already get. *No changes to `credential/`.*

The only new wrinkle is **credential lifetime**. Today
`WebIdentityAWSSource.Resolve` lazy-refreshes via `CredentialsCache`
([`credential/aws.go`](../../harness/internal/credential/aws.go)). A
batch wait can be hours; the polling client must call `Resolve` per
poll-tick, not cache the bearer token at submission time. This is
effectively free because `CredentialsCache` already handles refresh —
but it is an explicit invariant the implementer must respect.

### 2.6 Budget enforcement (`MaxCostBudget` / `MaxTokenBudget`)

[`types/runconfig.go:62-64`](../../types/runconfig.go) and
[`core/types.go:153-184`](../../harness/internal/core/types.go) define
the budgets. Today `TokenTracker.CheckBudget` runs before each turn and
again after tool results are appended ([`core/loop.go:378-382, 729-732`](../../harness/internal/core/loop.go)).

For batch, the cost is only known **after** the batch completes (the
result includes `usage.input_tokens` and `usage.output_tokens` per the
Anthropic doc, lines 1170–1175 of the saved doc — same shape as the
streaming Messages API). Two consequences:

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
  submission's input cost from `estimateCurrentTokens`
  ([`core/types.go:528-546`](../../harness/internal/core/types.go))
  and refuses submission if the per-batch projection would push the
  run over `MaxCostBudget`. Imperfect (output tokens are unknown) but
  non-zero protection.
- Future: aggregate "in-flight batch tokens" across concurrent runs at
  the control plane and apply a fleet-wide budget. Out of scope here.

### 2.7 Tracing & lakehouse

[`trace/trace.go`](../../harness/internal/trace/trace.go) and
[`types/runtrace.go:44-51`](../../types/runtrace.go) define the trace
schema. `TurnTrace` already has `DurationMs` — for a 1 h batch turn
this will be 3.6 M ms, which is correct but skews any percentile
aggregation that does not segment by turn-mode.

**Recommended additions** (for a follow-up issue, not v1):

- `TurnTrace.Mode string` — one of `"streaming"` / `"batch"` so the
  lakehouse can compute p50/p95 turn durations *separately* per mode.
  `eval/lakehouse/filestore.go` already has `Metrics` and aggregate
  computations
  ([`filestore.go:14-37`](../../eval/lakehouse/filestore.go)) — adding
  a per-mode bucket is a small change.
- `TurnTrace.BatchID string` (omitempty) — the provider's batch ID, so
  operators can cross-reference traces with the provider's console.

For v1, *do not* add fields. Instead, emit two new transport events
during the wait so the trace timeline at least shows the wait window:

- `batch_waiting` HarnessEvent (every 5 minutes, like `heartbeat`) so
  the control plane sees the harness is still alive. Reuses the
  existing 30 s heartbeat
  ([`core/loop.go:822-837`](../../harness/internal/core/loop.go)) — the
  heartbeat keeps firing at 30 s, and `batch_waiting` is the
  *batch-specific* analogue carrying the batch ID.
- `batch_submitted` and `batch_completed` lifecycle events (HarnessEvent
  type) for control-plane UIs that want to render a "waiting on batch"
  state distinct from "waiting on first SSE chunk".

OTel-side: a long-running `provider.batch.wait` span replaces the
existing `provider.stream` span
([`core/loop.go:492-498`](../../harness/internal/core/loop.go)) for a
batch turn. Tracer attribute `provider.batch=true` distinguishes them in
queries.

---

## 3. Reusing existing request marshalling (Required Q1)

Today's adapters mix request marshalling with HTTP+SSE:
[`anthropic.go:110-118`](../../harness/internal/provider/anthropic.go)
constructs an `anthropicRequest` value directly inside `Stream`;
[`openai.go:321`](../../harness/internal/provider/openai.go) does the
same with `openaiRequest`. The **batch wrapper must not duplicate this**.

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
([Anthropic doc](https://platform.claude.com/docs/en/build-with-claude/batch-processing),
lines 95–98 of the saved doc), and OpenAI batch wraps each in
`{ custom_id, method, url, body }`. Those two extra layers of envelope
live in the `BatchClient` HTTP code — **not** in the request-builder.
This keeps the request-builder unaware of "am I being streamed or
batched", which is the right separation.

Bedrock and Replay are intentionally left untouched. Bedrock has no
counterpart batch endpoint in scope; Replay is not affected because the
batch wrapper isn't used in replay runs (the recorded `RunRecording`
remembers whether it was a batch run, but the replayer plays it back as
fake events the same way today's `ReplayProvider` does — see §7).

---

## 4. Tool calls in a batch turn (Required Q2)

Batch responses carry the same `tool_use` content blocks as a streaming
response. Per the Anthropic batch doc:

> Any request that you can make to the Messages API can be included in
> a batch. This includes: Vision, **Tool use**, System messages, Multi-turn
> conversations, Any beta features.
> *(saved doc, line 54)*

**The harness handles tool_use in a batch turn identically to a
streaming turn.** The wrapper fabricates `tool_call` StreamEvents from
the completed batch result; the loop's existing `collectToolCalls` and
`dispatchToolCall` paths
([`core/loop.go:638, 656-722`](../../harness/internal/core/loop.go)) run
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

Two configurable escapes:

- `provider.batch.fallbackOnTimeout` (recommended default `false`): if a
  per-turn batch wait *times out at the harness* (not "expired" at the
  provider — that is a different failure), the wrapper switches to the
  streaming adapter for that turn. This trades cost for liveness.
- `provider.batch.firstTurnOnly` (default `false`): submit the first
  turn (the heavy context-laden one) as a batch, then do follow-ups via
  streaming. Useful for "research mode that wants a quick turnaround
  on tool follow-ups" — but defaults off because it's a footgun (you
  might think you're saving 50 % overall when you're saving 50 % only
  on the first turn).

UX note for `research` mode: tools are typically read-only (per
`DefaultReadOnlyBuiltInTools` at
[`runconfig.go:521-529`](../../types/runconfig.go)), so the tool-loop
case in research mode is realistic — `web_fetch`, `read_file`,
`search_files`. With single-turn-per-batch, a 5-turn research run
becomes a 5×24 h worst-case, but in practice batches typically complete
in <1 h (Anthropic doc, line 22), so the wall-clock is manageable.

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
| **(c) One turn per batch, size 1, sent directly to provider** | Acceptable as the stdio-mode degraded path. A size-1 batch still gets the 50 % discount per the Anthropic pricing table (line 71). It's just leaving throughput on the table. |

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

| Provider | Mechanism | Where it terminates |
|---|---|---|
| Anthropic | Polling on `processing_status` (saved doc lines 461–466). Anthropic also exposes a **Webhooks** product (`platform.claude.com/.../webhooks`) that fires `message_batch.succeeded` / `.errored` / `.expired` / `.canceled` events for batches you own. *[Verify exact event names against current docs.]* | **Webhooks at the control plane** (recommended); polling fallback also at the control plane. |
| OpenAI | Polling only (cookbook + reference). | **Polling at the control plane.** |

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
itself polls. Polling cadence: exponential backoff (60 s start,
doubling, capped at 5 min) — matching the SDK examples in the saved
doc (lines 540–541, `time.sleep(60)`) and OpenAI cookbook
(`time.sleep(60)`). Hard cap: `MaxWaitSeconds`, defaulting to 24 h
(matching the provider SLA).

The stdio-mode polling client is **opt-in** behind a separate flag
`provider.batch.harnessSidePolling=true`, defaulting to `false`. With
the flag off and `transport=stdio`, the harness fails fast at boot
with a clear error: *"batch requires either transport=grpc (control
plane bundles + polls) or provider.batch.harnessSidePolling=true (this
process polls directly)"*. This forces the operator to make the choice
explicit rather than silently incurring 24 h harness waits.

---

## 7. Failure & cancellation (Required Q5)

Cancellation paths:

| Trigger | Existing behaviour ([`core/loop.go:81-86`](../../harness/internal/core/loop.go)) | Batch adjustment |
|---|---|---|
| `cancel` ControlEvent during batch wait | `cancelRun(ErrCancelledByControlPlane)` cancels the run ctx | The wrapper's correlator unblocks via ctx.Done; the wrapper then **best-effort calls the batch cancel endpoint** (`POST /v1/messages/batches/{id}/cancel` or `POST /batches/{id}/cancel`) **only when in stdio polling mode**. In control-plane mode the harness emits a `batch_cancel_request` HarnessEvent and the control plane is responsible for the upstream cancel. |
| `ctx.DeadlineExceeded` (run timeout) | Same path as above (classified `timeout`) | Same as cancel. The provider batch is left to expire on its own (24 h) unless cancelled — which is fine because the operator is no longer being charged for tokens the model has not yet generated. |
| Harness restart (K8s pod replacement) | Run is lost; control plane treats as failed | **Batch is lost from the harness perspective** but **survives at the provider**. The control plane should record the batch ID at submission time so a replacement harness can be started with `--resume-batch <id>` (out of scope for v1; documented as a follow-up). |
| Batch partial failure (some entries succeed, others don't) | N/A — Stirrup submits size-1 entries, so partial failure collapses to total failure for the run. | The wrapper inspects the result type per the Anthropic doc table (saved doc lines 873–880): `succeeded` → fabricate stream; `errored` → emit StreamEvent{Type: "error"} with the provider error message; `canceled` → `outcome=cancelled`; `expired` → new outcome `"batch_expired"`. |

A new outcome value `"batch_expired"` is added to the canonical list at
[`runtrace.go:24`](../../types/runtrace.go) and the proto comment at
`harness.proto:113-115`. Distinct from `timeout` (which is the harness
wall-clock timeout) and `error` (which is "couldn't even submit").

In control-plane mode, the *control plane* observes the webhook event
and translates it into a `batch_result` ControlEvent with
`is_error=true` and `content` carrying the structured error — the
harness reuses its existing `extractAsyncToolResult`-style error path,
no new control-flow needed.

---

## 8. Stdio mode (Required Q6)

A stdio harness has no inbound channel for `batch_result` ControlEvents
because stdin in stdio mode is treated as a `user_response`/`cancel`
queue ([`transport/stdio.go`](../../harness/internal/transport/stdio.go)).
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
50 % discount. But the default must be safe: if the operator says
`provider.batch.enabled=true` and `transport=stdio` *without*
`provider.batch.harnessSidePolling=true`, the harness fails at config
validation with a directing error message.

The stdio polling client lives in
`harness/internal/provider/batchpoll.go` (new). It implements the
`BatchClient` interface from §2.1 and is selected by the factory when
both `transport.type=stdio` and `provider.batch.harnessSidePolling=true`.
HTTP code is hand-rolled per the project's minimal-dependency philosophy
(`CLAUDE.md` "External dependencies rationale"). Polling cadence: 60 s
start, exponential to 5 min, ctx-cancelled on run cancel.

---

## 9. Eval & replay (Required Q7)

[`provider/replay.go:23-97`](../../harness/internal/provider/replay.go)
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
      "firstTurnOnly": false,
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
    Enabled              bool `json:"enabled,omitempty"`
    MaxWaitSeconds       int  `json:"maxWaitSeconds,omitempty"`       // default 86400 (24h)
    HarnessSidePolling   bool `json:"harnessSidePolling,omitempty"`   // required when transport=stdio
    FallbackOnTimeout    bool `json:"fallbackOnTimeout,omitempty"`    // fall back to streaming on harness-side timeout
    FirstTurnOnly        bool `json:"firstTurnOnly,omitempty"`        // batch turn 0 only, stream rest
    AllowInteractiveModes bool `json:"allowInteractiveModes,omitempty"` // permit planning/review (execution still rejected)
}
```

Validation (added to `ValidateRunConfig` at
[`runconfig.go:562-652`](../../types/runconfig.go)):

| Invariant | Rationale |
|---|---|
| `batch.enabled && mode == "execution"` → reject | Interactive mode; 24 h latency unacceptable. |
| `batch.enabled && (mode == "planning" \|\| mode == "review") && !batch.allowInteractiveModes` → reject | Default-deny for these modes, but operator can override. |
| `batch.enabled && transport.type == "stdio" && !batch.harnessSidePolling` → reject | No inbound channel; force the operator to be explicit. |
| `batch.maxWaitSeconds <= 0 \|\| > 86400` → reject | Bound the wait to the provider SLA. |
| `batch.harnessSidePolling && transport.type == "grpc"` → reject | Don't run two concurrent batch clients. |
| `batch.firstTurnOnly && batch.fallbackOnTimeout` → warn (not reject) | Both are fallback mechanisms; setting both is unusual but not unsafe. |

Cross-cutting interaction with **#42 safety rings**:

- `RuleOfTwo` (`runconfig.go:755-771`): a long batch wait (hours) is a
  longer-lived credential exposure than a 30 s stream. Adding
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

Concrete proto field changes (deferred to a follow-up issue per
"Out of scope: Proto changes"):

```protobuf
message HarnessEvent {
  // ... existing 13 fields ...
  // No new fields needed — all batch events reuse request_id (11),
  // input (5), and content (7). Only the type discriminator changes.
}

message ControlEvent {
  // ... existing 8 fields ...
  // No new fields needed — batch_result reuses request_id (4),
  // content (7), and is_error (8).
}
```

This is by design: the existing union-on-`type` shape already absorbs
batch as a 4th correlator pattern (after `permission_*`,
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
2. Polls `GET /v1/messages/batches/{id}` every 60 s → 5 min (exponential
   backoff) using the same HTTP client family
   ([`anthropic.go:42-50`](../../harness/internal/provider/anthropic.go))
   with adjusted timeouts (poll requests are ~30 s, not 120 s).
3. On `processing_status=ended`, fetches `results_url`, parses the JSONL
   line for the run's `custom_id`, and fabricates the `StreamEvent` flow.
4. Emits a `text_delta` of "[batch waiting...]" every 5 minutes to stdout
   so an interactive operator gets visible progress; `heartbeat` events
   continue at 30 s.
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
  refactor; existing `Stream` calls them. Adds a single test per helper
  asserting JSON equality with a golden fixture.
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
  `permission/askupstream.go:73-81`).
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
  batch creation, per the cookbook).
- Evaluate Bedrock `CreateModelInvocationJob` separately — note in this
  plan that it is NOT wire-compatible with `ConverseStream`, so a
  Bedrock batch path is essentially a new adapter rather than a wrapper.
  Recommend punting until a concrete user request.

### Out of phases (not in v1, kept here for completeness)
- Cedar action `Action::"batch:submit"` and policy templates.
- `--resume-batch <id>` (harness restart recovery).
- Per-batch cost projection / pre-flight guard.
- Fleet-wide cost aggregation.

---

## 14. Risks and unanswered questions

- **Webhook coverage on Anthropic.** This plan was written without
  successfully fetching the Anthropic webhook docs (the URL returned
  404 during research). The plan's design works whether or not webhooks
  exist, because polling is always available and the harness never sees
  webhooks directly. *Implementer should verify webhook event names
  before writing the control-plane subscriber.*
- **Cost overrun on the last turn.** §2.6's gap is real and unavoidable
  in v1: the operator can blow the budget by exactly one turn's worth
  of tokens. Document loudly.
- **Long-lived credential exposure.** §2.5's note: a 24 h batch wait is
  a 24 h SSM parameter exposure. Today's cap is 30 s of streaming.
  This is a security-rings question more than a batch question; flag in
  `docs/sandbox.md` so it gets reviewed.
- **`MaxTurns` semantics.** The default 20-turn run × 24 h is 480 hours
  worst-case. `ValidateRunConfig` should warn (not reject) when
  `batch.enabled` and `maxTurns > 5` — or this can be a control-plane
  policy decision. Recommend leaving the warning to the control plane.
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
  — fetched 2026-05-03. Quoted lines 22, 41–47, 54, 71, 95–98, 873–880,
  1170–1175, 2183, 2215, 2221.
- Anthropic, *Webhooks*,
  [`platform.claude.com/docs/en/build-with-claude/webhooks`](https://platform.claude.com/docs/en/build-with-claude/webhooks)
  — **could not verify**, page returned 404 during research. Used only
  for the second-hand framing in issue #67.
- OpenAI, *Batch reference*,
  [`developers.openai.com/api/reference/resources/batches`](https://developers.openai.com/api/reference/resources/batches)
  — fetched 2026-05-03. Used for endpoint/status enumeration and
  `error_file_id` partial-failure model.
- OpenAI, *Cookbook: batch_processing*,
  [`developers.openai.com/cookbook/examples/batch_processing`](https://developers.openai.com/cookbook/examples/batch_processing)
  — fetched 2026-05-03. Used for the JSONL input shape (`custom_id`,
  `method`, `url`, `body`) and the `client.files.create(purpose="batch")`
  + `client.batches.create(...)` flow.

Codebase:

- [`harness/internal/provider/provider.go:12-14`](../../harness/internal/provider/provider.go) — `ProviderAdapter` interface
- [`harness/internal/provider/anthropic.go:103-171`](../../harness/internal/provider/anthropic.go) — Anthropic streaming Stream impl
- [`harness/internal/provider/openai.go:91-178`](../../harness/internal/provider/openai.go) — OpenAI Chat Completions wire types
- [`harness/internal/provider/openai_responses.go:90-176`](../../harness/internal/provider/openai_responses.go) — OpenAI Responses wire types
- [`harness/internal/provider/replay.go:23-97`](../../harness/internal/provider/replay.go) — Replay provider
- [`harness/internal/transport/transport.go`](../../harness/internal/transport/transport.go) — Transport interface
- [`harness/internal/transport/correlator.go`](../../harness/internal/transport/correlator.go) — request/response correlator (the canonical async-await pattern)
- [`harness/internal/permission/askupstream.go`](../../harness/internal/permission/askupstream.go) — `AskUpstreamPolicy`, the closest analogue to "block on a control-plane decision"
- [`harness/internal/core/loop.go:375-745`](../../harness/internal/core/loop.go) — `runInnerLoop`, the turn-based agentic loop the wrapper must not perturb
- [`harness/internal/core/types.go:282-379`](../../harness/internal/core/types.go) — `dispatchAsyncToolCall`, the existing precedent for "emit + Await + classify error"
- [`types/events.go:43-93`](../../types/events.go) — `HarnessEvent` / `ControlEvent`
- [`types/runconfig.go:562-652`](../../types/runconfig.go) — `ValidateRunConfig`
- [`types/runtrace.go:14-50`](../../types/runtrace.go) — `RunTrace` / `TurnTrace`
- [`proto/harness/v1/harness.proto`](../../proto/harness/v1/harness.proto) — current event shape
- [`eval/lakehouse/filestore.go:14-37`](../../eval/lakehouse/filestore.go) — file-store lakehouse aggregations
- `VERSION1.md` — for the harness's "short-lived job, not a server" posture
- `CLAUDE.md` — for the minimal-dependency philosophy that constrains the wire client choices.
