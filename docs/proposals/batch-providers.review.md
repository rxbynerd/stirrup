# Review adjustments — `batch-providers.md`

**Reviewer:** Principal Technical Documentation Reviewer
**Review date:** 2026-05-03
**Source doc:** [`docs/proposals/batch-providers.md`](./batch-providers.md)
**Format:** ordered, severity-tagged adjustments. Each item names the location, the change to make, and (where useful) the replacement text. Apply in order; later items assume earlier ones are already in.

---

## Blockers (must apply before merge)

### B1. Invert the webhook story for the two providers

The doc asserts "Anthropic has webhooks; OpenAI is polling-only." Live verification on 2026-05-03 shows the reverse:

- **OpenAI ships native batch webhooks** (`batch.completed` and companions) per [`platform.openai.com/docs/api-reference/webhook-events`](https://platform.openai.com/docs/api-reference/webhook-events) and [`developers.openai.com/api/docs/guides/webhooks`](https://developers.openai.com/api/docs/guides/webhooks).
- **Anthropic does not currently ship batch webhooks**. The URL `platform.claude.com/docs/en/build-with-claude/webhooks` returns 404 and independent third-party guides explicitly state none exist for Message Batches.

Apply across **TL;DR (§1, item 4)**, **§6**, **§12 (responsibility table)**, **§14 (risks list)**, **§15 (sources)**:

- TL;DR item 4 — replace:
  > "The control plane terminates Anthropic webhooks and OpenAI polling."

  with:
  > "The control plane terminates provider webhooks where available (OpenAI `batch.completed` today; Anthropic when/if they ship them) and falls back to polling otherwise."

- §6 table — rewrite both rows:

  | Provider | Mechanism (verified 2026-05-03) | Where it terminates |
  |---|---|---|
  | Anthropic | Polling on `processing_status`. **No native webhooks for Message Batches at the time of writing.** | Polling at the control plane. |
  | OpenAI | Webhooks (`batch.completed` and lifecycle companions). Polling fallback also available. | Webhooks at the control plane; polling fallback also at the control plane. |

- §6 caveat box — replace the "Caveat on webhooks" passage with:
  > "Verified 2026-05-03: Anthropic Message Batches has no documented webhook product (the speculated `platform.claude.com/.../webhooks` page returns 404). OpenAI Batch is webhook-enabled (`batch.completed`). The harness design is unaffected — it sees only `batch_result` ControlEvents and is agnostic to the upstream mechanism — but the control plane should implement the OpenAI webhook subscriber first and the Anthropic poller as the only path for that provider."

- §15 sources — add:
  - `https://platform.openai.com/docs/api-reference/webhook-events`
  - `https://developers.openai.com/api/docs/guides/webhooks`

  And mark the Anthropic webhooks line "Could not verify (404)." as "Verified missing as of 2026-05-03."

---

## Major (resolve before implementation begins)

### M1. Specify cancellation semantics in bundled-batch mode

Add a new subsection to **§7** titled "Cancellation in bundled mode (gRPC transport)". Adopt the following:

> When the control plane bundles N concurrent harness submissions into one provider batch, neither Anthropic nor OpenAI exposes a per-entry cancel — the batch-cancel endpoint cancels the whole batch.
>
> Default policy: a single run's `cancel` ControlEvent unblocks that run at the harness boundary (the wrapper's `Correlator.Await` returns ctx error) and emits `batch_cancel_request` upstream, but the control plane MUST NOT propagate this to a provider batch-cancel call when other runs are still bundled with it. The cancelled run is billed for its share of the batch tokens; this is documented behaviour in `docs/sandbox.md`.
>
> Opt-in alternative `provider.batch.cancelBundleOnRunCancel` (default `false`): a run's cancel cancels the entire bundled batch. Other runs in the same bundle observe outcome `batch_cancelled`. Intended for tightly-coupled job groups where one run failing means the whole job is dead.

Then add a row to the §10 invariant table:

| `batch.cancelBundleOnRunCancel && transport.type == "stdio"` -> reject | The flag is meaningless without a control plane to bundle. |

### M2. Widen `BatchClient.Submit` to accept multiple entries

In **§2.1**, the sketched interface
```go
type BatchClient interface {
    Submit(ctx context.Context, entry BatchEntry) (id string, err error)
    Result(ctx context.Context, id string) (*BatchResult, error)
}
```
forces OpenAI's two-step flow (`POST /v1/files` then `POST /v1/batches`) into a degenerate per-entry shape and prevents `controlPlaneBatchClient` and `harnessPollingBatchClient` from sharing the type. Replace with:

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

Update **§8** (stdio polling) to note that the size-1 case is a thin wrapper over the multi-entry interface, and update **§13 Phase 6** to explicitly call out that adding OpenAI is now a strict subset of the existing interface rather than a refactor.

### M3. Cut `firstTurnOnly` from v1

The doc itself describes this flag as a footgun. Move it to "Out of phases" in **§13** and remove it from:
- §4 ("Two configurable escapes" — keep `fallbackOnTimeout`, drop `firstTurnOnly`).
- §10 `BatchProviderConfig` Go sketch (remove the field).
- §10 invariants table (no longer applicable).

If a future user requests it, add it then; designs that ship configurable footguns acquire compatibility debt.

### M4. Document the bundled-batch budget enforcement split

Append to **§2.6**:
> In bundled mode, by the time the provider batch returns the harness has already been billed for every entry's tokens. Per-run `MaxCostBudget` enforcement therefore MUST be performed at the control plane *before* the entry is bundled into a provider batch. The harness's existing `TokenTracker.CheckBudget` still runs as a second-line check after the result arrives, but cannot prevent over-budget billing in bundled mode.

Add a row to the §12 responsibility table:

| Per-run pre-submission cost projection | **Control plane** in grpc mode; **harness** in stdio polling mode | Without it, bundled batches can blow per-run `MaxCostBudget` silently. |

### M5. Apply RFC 2119 keyword discipline in §10

Add a one-line "Conventions" note above the invariant table:
> "MUST", "MUST NOT", "SHOULD", "MAY" in this section are used per [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) and [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174)."

Then convert each row's prose-verb to a normative keyword:

| Current prose | Normative form |
|---|---|
| "-> reject" | "MUST be rejected by `ValidateRunConfig`" |
| "-> warn (not reject)" | "SHOULD emit a warning; MUST NOT reject" |

Apply the same edit to §6's "fails fast at boot" passage and §8's "fails at config validation".

### M6. Replace line-range references with symbol references

Every reference of the form `path/file.go:NNN-MMM` will rot. Sweep through the doc and:
- Replace with symbol references where the symbol exists (`harness/internal/core/loop.go::runInnerLoop`, `harness/internal/permission/askupstream.go::AskUpstreamPolicy.Check`).
- Where the line range is genuinely load-bearing (because you are quoting a block of code), use a commit-pinned permalink: `https://github.com/rxbynerd/stirrup/blob/<sha>/...#L282-L379`.

Locations to sweep: §1, §2.1, §2.2, §2.3, §2.4, §2.5, §2.6, §2.7, §3, §6, §10, §15.

Spot-checked drift on 2026-05-03:
- `core/types.go:282-379` — actually starts at line 298.
- `core/types.go:110-123` — actually starts at line 102.
- `core/loop.go:822-837` — actually 820-837.
- `core/loop.go:81-86` — actually 81-85.

### M7. Tighten TL;DR items 2 and 3

They read as contradictory on first pass. Replace item 2 with:
> "Treat batch as **single-turn-per-batch at the harness boundary**: each turn submits one batch entry; the control plane is free to bundle entries from many concurrent runs into a single provider-side batch (one `custom_id` per harness run/turn). Multi-turn tool loops are preserved unchanged at the cost of N x 24 h worst-case for multi-turn runs — acceptable in `research`/`toil`."

Leave item 3 as-is; it now reads as a clarifying expansion rather than an apparent contradiction.

### M8. Strengthen the Bedrock note

In **§2.5** (last paragraph) and **§13 Phase 6**, replace "the wire format is different" with the concrete reasons:
> Bedrock batch uses `CreateModelInvocationJob` with a JSONL input on S3 (`inputDataConfig.s3InputDataConfig`), not an HTTP body. Job timeout is 24-168 hours (per the [AWS API reference](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_CreateModelInvocationJob.html)), which exceeds the §10 cap of 86 400 s. Adding Bedrock therefore requires (a) an S3 abstraction in `BatchClient`, (b) a relaxation of the `maxWaitSeconds` invariant gated on provider type, and (c) a separate `BatchEntry` shape for the `recordId`/`modelInput` JSONL format. Recommend punting until concrete user demand.

### M9. Document the `maxWaitSeconds` interaction with `completion_window`

OpenAI's `completion_window` is currently fixed at `24h` (see [OpenAI Batch FAQ](https://help.openai.com/en/articles/9197833-batch-api-faq)). A user setting `maxWaitSeconds=3600` will hard-fail at 1 hour while the upstream batch keeps running and being billed. Add to **§6** (after the polling cadence paragraph) and **§10** (note under `MaxWaitSeconds`):

> Setting `MaxWaitSeconds` below the provider's minimum completion window does NOT cause the provider to finish faster. It is a harness-side hard timeout; the upstream batch continues until the provider's own deadline (24h for both Anthropic and OpenAI today) and is billed regardless. In `harnessSidePolling=true` mode, the harness MUST best-effort call the upstream batch-cancel endpoint when its `MaxWaitSeconds` fires.

### M10. Add a worked example for the "fabricate a stream" wrapper

In **§2.1**, after the `BatchAdapter.Stream` sketch, add a 15-line worked example showing:

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

Add a sentence noting that `stallDetector.recordToolCall` is unaffected because tool-call detection happens after `streamEventsToResult` collects the buffered events.

---

## Minor

### m1. §11 — drop the misleading "no fields needed" proto blocks

Delete the two `message HarnessEvent { ... }` and `message ControlEvent { ... }` code blocks. Replace with a single sentence:
> "No proto field changes are required: `batch_submission`, `batch_waiting`, `batch_cancel_request`, and `batch_result` reuse `request_id`, `input`, and `content` on the existing `HarnessEvent` / `ControlEvent` shapes; only the type discriminator strings are new. The proto comment block in `proto/harness/v1/harness.proto` SHOULD be updated alongside the implementation to document the new discriminator values."

### m2. §3 — note that the request-builders are not symmetric

After the symmetric-helpers paragraph, add:
> "These helpers produce *different* types — `anthropicRequest`, `openaiRequest`, and `responsesRequest` reflect the underlying API differences (`system` vs `instructions`, `messages` vs typed `input[]`, `max_tokens` vs `max_output_tokens`). The `BatchEntry.Body` field is `json.RawMessage` precisely so the wrapper can hold any of the three. Pure-function purity per provider is preserved; only the call-site picks which builder to invoke."

### m3. §6 — adjust the polling cadence

Replace "exponential backoff (60 s start, doubling, capped at 5 min)" with:
> "exponential backoff with jitter: 10 s initial, doubling to a 5 min cap, with +/- 20% jitter on each interval to avoid herding multiple harnesses against the provider's batch-status endpoint."

### m4. §8 step 4 — do not use `text_delta` for operator-feedback

Synthetic chunks emitted via `text_delta` would enter the model's history on replay (it's the assistant-output channel). Replace step 4:
> "4. Emits a `batch_waiting` HarnessEvent (carrying `batchId` and elapsed-seconds) every 5 minutes so a control plane or interactive operator sees progress; `heartbeat` events continue at 30 s. (Never use `text_delta` for operator-only feedback — it would pollute the recorded `ModelOutput` and replay path.)"

### m5. §2.7 — replace the long-running OTel span with per-poll spans

Replace the "long-running `provider.batch.wait` span" recommendation with:
> "Emit one short OTel span per poll tick (`provider.batch.poll`, attributes `batch.id`, `batch.attempt`, `batch.status`) plus a final `provider.batch.complete` span carrying `batch.duration_ms`. This avoids multi-hour spans, which OTel collectors and storage backends (Tempo, Jaeger) often drop or truncate. The control plane can reconstruct the timeline from the span chain."

### m6. §5 — clarify "leaving throughput on the table"

Replace with:
> "It's just paying the per-batch HTTP-call overhead and missing whatever throughput allocation the provider applies to large batches; the per-token discount is unaffected by batch size."

### m7. §14 `MaxTurns` recommendation needs a mechanism

Replace:
> "`ValidateRunConfig` should warn (not reject) when `batch.enabled` and `maxTurns > 5` — or this can be a control-plane policy decision."

with:
> "Recommendation: add a soft warning in `ValidateRunConfig` when `batch.enabled && maxTurns > 5` (analogous to the rule-of-two warning emitter). Hard rejection remains a control-plane policy decision — Cedar action `Action::"batch:submit"` with a `principal.maxTurns < 5` check is the natural point."

### m8. §13 Phase 0 — replace golden fixtures with property check

Replace "Adds a single test per helper asserting JSON equality with a golden fixture" with:
> "Adds one property test per helper: for any `StreamParams`, `Stream(params)` and `buildRequest(params, true)` MUST produce byte-identical request bodies. Property tests survive minor request-shape additions (e.g. new optional fields) where golden fixtures would force a churn diff per change."

### m9. Spelling consistency

Sweep for American spellings inside an otherwise British document:
- "footgun" appears in §4 (footgun is fine — it is jargon, not a register choice). Leave as-is.
- "behavior" in §13 Phase 0 — change to "behaviour".
- All other instances of "behaviour", "marshalling", "summarise" are already correct.

### m10. Title of the document

Rename "Batch message API support" to "Batch provider API support" to match the proposed `BatchProviderAdapter` interface name and the project's existing `ProviderAdapter` terminology. Update the H1 in `batch-providers.md` and any cross-references.

---

## Nits

### n1. §1 item 5

Drop "below" — "detailed in §10 below" -> "detailed in §10".

### n2. §15 saved-doc citations

Lines like "(saved doc, line 54)" cite a document not present in the repo. Either:
- Commit the saved Anthropic/OpenAI docs to `docs/proposals/_external/` for future verification, or
- Drop the line numbers and cite only the URL.

Prefer the second; the URLs are the source of truth.

### n3. §2.1 table — Option B verdict

Replace "Reject as the *primary* shape because of loop churn" with "Reject in favour of Option C, which achieves the same separation without loop churn." The current wording reads as if Option B is partially accepted.

### n4. §10 — `MaxWaitSeconds` zero-value semantics

The Go shape gives `MaxWaitSeconds int` with `omitempty`; an unset value will zero-value to 0, which is currently rejected by the invariant `batch.maxWaitSeconds <= 0 -> reject`. That means `Batch.Enabled=true` without an explicit `MaxWaitSeconds` is a hard error. Either:
- Document the default in `ValidateRunConfig` (apply 86 400 if zero, then validate), OR
- Switch to `*int` (matches the project's existing `MaxTokenBudget *int` / `Timeout *int` pattern in `RunConfig`).

Recommend the second for consistency with the rest of `RunConfig`.

### n5. §10 — `AllowInteractiveModes` field placement

The field gates `planning`/`review` only; `execution` is rejected unconditionally. The field name suggests broader scope. Rename to `AllowReadOnlyInteractiveModes` or document the exact set in the field comment:

```go
// AllowInteractiveModes permits batch.enabled with mode == "planning" or
// mode == "review". Has no effect on mode == "execution" (always rejected).
AllowInteractiveModes bool `json:"allowInteractiveModes,omitempty"`
```

---

## Open questions to resolve before applying

1. **M1 cancellation policy** — confirm "default: do not propagate cancel upstream when bundled; opt-in to bundle-cancel" is the right call. If you want bundle-cancel as the default, swap the polarity of the new flag.
2. **M3** — confirm `firstTurnOnly` may be cut from v1.
3. **M5** — confirm RFC 2119 wording is welcome (some teams prefer prose).
4. **n4** — confirm `*int` is the right shape for `MaxWaitSeconds`.

---

## Sources consulted at review time (2026-05-03)

- [Anthropic Batch processing](https://platform.claude.com/docs/en/build-with-claude/batch-processing)
- [Anthropic Message Batches API production guide (third-party)](https://jangwook.net/en/blog/en/anthropic-message-batches-api-production-guide/) — confirms no native webhooks
- [OpenAI Batch reference](https://platform.openai.com/docs/api-reference/batch)
- [OpenAI Batch FAQ](https://help.openai.com/en/articles/9197833-batch-api-faq)
- [OpenAI Webhooks events](https://platform.openai.com/docs/api-reference/webhook-events)
- [OpenAI Webhooks guide](https://developers.openai.com/api/docs/guides/webhooks)
- [OpenAI Webhooks Batch & Deep Research guide 2026](https://www.hooklistener.com/learn/openai-webhooks-guide)
- [AWS Bedrock CreateModelInvocationJob](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_CreateModelInvocationJob.html)
- In-repo files spot-checked: `harness/internal/provider/provider.go`, `harness/internal/permission/askupstream.go`, `harness/internal/transport/correlator.go`, `harness/internal/core/loop.go`, `harness/internal/core/types.go`, `types/runtrace.go`, `types/runconfig.go`, `types/events.go`
