## Batch provider mode

### What it is

Async batch submission routes every provider turn through the
provider's batch endpoint (Anthropic
`/v1/messages/batches`, OpenAI `/v1/batches`) rather than the live
streaming endpoint. The provider returns the assistant message
asynchronously, in exchange for which the harness pays roughly half
the per-token price. Provider SLAs cap completion at 24 hours per
batch, but the harness never waits that long — see
[The wait budget](#the-wait-budget).

In practice this turns a streaming turn that completes in seconds
into an async turn that can take anywhere from a few seconds to the
run's whole wall-clock budget, in exchange for a ~50% discount on
input and output tokens.

### The wait budget

`ValidateRunConfig` caps every run's `timeout` at 3600 seconds, and
both entry points bind the run context to it: `stirrup harness`
directly, and `stirrup job` after the `task_assignment` arrives. The
run deadline therefore cancels a pending batch wait before any longer
harness-side cap could fire, so the batch wait budget is the run
`timeout` and nothing larger is reachable.

Validation enforces that directly:

- `provider.batch.maxWaitSeconds` must lie in `(0, timeout]`. A larger
  value is rejected with an error naming both the requested wait and
  the run timeout, on a disabled `batch` block as well as an enabled
  one, so the contradiction surfaces at authoring time.
- Omitting `maxWaitSeconds` on an enabled batch config defaults it to
  the run `timeout`.

A provider batch that has not resolved within the run's `timeout` is
therefore abandoned. Batch mode suits work the provider typically turns
around in minutes, not work that relies on the 24-hour SLA tail.

#### `fallbackOnTimeout` needs real headroom

`fallbackOnTimeout` retries a turn against the streaming endpoint when
the harness-side cap fires. It can only do that if the cap fires
*before* the run deadline — and a cap equal to the run `timeout` never
does. The run context is armed before the turn starts, while the cap
only starts once the wait blocks (after marshalling, `Submit`, and at
least one round trip), so the run deadline is always the earlier of the
two. `ValidateRunConfig` rejects the combination rather than accepting a
flag that provably cannot fire:

```
batch.fallbackOnTimeout requires batch.maxWaitSeconds strictly below
the run timeout, got maxWaitSeconds=3600 and timeout=3600: …
```

Sizing the headroom is the operator's call, because it depends on the
run's own retry configuration. A fallback turn needs
`provider.retry.wallClockBudgetMs` (default 90 000 ms, ceiling 300 000)
plus the 120 s streaming HTTP timeout.

The headroom must also cover *every preceding batch turn*, because each
turn's cap is measured from its own `Result` call rather than from run
start. With `timeout: 3600` and `maxWaitSeconds: 3000`, a first turn
that consumes 700 s puts the second turn's cap at 3700 s — past the run
deadline, so the fallback is silently unreachable again from turn 2
onward. For the fallback to stay reachable on the last turn of an
N-turn run, size it so that
`maxWaitSeconds × N + streaming-headroom <= timeout`. The harness cannot
pick that number generically, which is why it asks rather than reserving
a slice of the budget itself.

### When to use it

Batch mode targets non-interactive runs where wall-clock latency does
not matter and operators want the discount: large research crawls,
overnight backfills, low-priority `toil` work. `ValidateRunConfig`
enforces the safe-by-default posture:

- **`execution`** mode is rejected outright. An editing run that can
  also `run_command` must not block for hours between turns; the
  combination invites stale-workspace and resource-hold footguns.
- **`planning`** and **`review`** are rejected unless the operator
  sets `provider.batch.allowInteractiveModes=true`. These modes are
  interactive by design — they optimise for fast feedback on plans
  and code reviews rather than the whole-timeout wait that batch
  implies — and the opt-in exists so an operator who has deliberately
  chosen async planning has to acknowledge the footgun.
- **`research`** and **`toil`** are accepted unconditionally — they
  are the modes the feature was built for.

### How to enable

The CLI flag is the recommended path:

```sh
stirrup harness --batch --mode research --prompt "..."
```

The flag carries only the `enabled` bit. Operators who need to set
`maxWaitSeconds`, `harnessSidePolling`, `fallbackOnTimeout`,
`cancelBundleOnRunCancel`, or `allowInteractiveModes` must use
`--config` with a `provider.batch` block:

```json
{ "provider": { "type": "anthropic", "batch": { "enabled": true } }, "mode": "research" }
```

A `--config` file with a fuller `batch` block composes with the flag:
passing `--batch` on top of a file that sets
`harnessSidePolling=true` flips `enabled` on while preserving the
file's polling setting.

### Transport requirements

The recommended path is `transport=grpc`. The control plane bundles
concurrent runs into a single provider-side batch, amortising the
provider-side tail across many runs and giving the harness a single
round-trip to wait on. The phase-2 `controlPlaneBatchClient` is the
default for gRPC operators.

Stdio operators must set `provider.batch.harnessSidePolling=true` in
their `--config`. In this mode polling executes within the harness
process itself rather than via the control plane — there is no
control plane to amortise across, so every run holds its own
connection open for the duration of its batch. The harness-side polling client is
experimental in v1 and ships only for the Anthropic provider; OpenAI
support lands in phase 6 (issue #139), and Bedrock is deferred (see
below).

`ValidateRunConfig` rejects the mismatches:

- `harnessSidePolling=true` with `transport=grpc` is rejected — the
  control plane already owns polling on the gRPC path.
- `cancelBundleOnRunCancel=true` with `transport=stdio` is rejected
  — there is no bundle to cancel.

### The `batch_result` outcome contract

On the gRPC path the control plane completes each `batch_submission`
with a `batch_result` ControlEvent. Its `content` is the canonical
outcome: a JSON `BatchResult` setting exactly one of `response`
(success) and `err` (failure). The ControlEvent's `is_error` flag is
optional and redundant for this event type — the harness only
cross-checks it, and never resolves a disagreement in its favour,
because a control plane that mislabels a success as an error would
otherwise silently corrupt a turn.

Each of these becomes an `invalid_request_error` whose message names
what arrived:

- `content` missing, or larger than 4 MiB;
- `content` that is not valid JSON;
- a payload setting neither `response` nor `err`, or both;
- an `is_error` that disagrees with the payload — `true` alongside a
  success response, or `false` alongside an `err`.

`is_error` semantics are unchanged for `tool_result_response` and
`sandbox_token_response`, where the flag is the only discriminator.
Wire-level detail for control-plane implementers:
[`integration-guide.md`](integration-guide.md#batch-mode-amortised-token-pricing).

### Cost and budget caveats

Two operator-visible gaps follow from the wait window:

**Budget overrun gap.** `MaxTokenBudget` is checked at the top of
each loop turn and again after tool dispatch, never against the call
about to be issued. Accounting happens only once the provider
returns: `TokenTracker.RecordTurn` charges the turn's entire prepared
context — message history, system prompt, and tool definitions,
re-counted for that turn — plus its output tokens. The maximum
overrun is therefore one full turn's input context *plus* its output,
which for a run with a 150 k `contextStrategy.maxTokens` ceiling
dwarfs any single response. Operators sizing `MaxTokenBudget` for a
batch run should leave headroom for one whole turn at that ceiling,
not for the largest response or the run-average turn cost.

`MaxCostBudget` provides no cover here at all: it is accepted and
bounded but never enforced (see
[Limits and budgets](configuration.md#limits-and-budgets)), so a
batch run's spend is capped only by `Timeout` — the tightest cap,
validated at ≤ 3600 s and bound to the run context on both
`stirrup harness` and `stirrup job` — plus `MaxTokenBudget`,
`MaxTurns`, and whatever the control plane enforces.

**Long-lived credential exposure.** A batch wait keeps the provider's
API credentials live in memory for up to the run's `timeout` (an hour
at the cap), against ~120s for a streaming turn. Operators using
`WebIdentityAWSSource` (or any other `credential.Source` backed by a
short-lived federated token) should confirm their `CredentialsCache`
TTL covers the full `MaxWaitSeconds` window — a refresh that fires
mid-wait can leave the harness holding stale credentials when the
batch completes.

### `MaxTurns` × `MaxWaitSeconds` warning

The default `MaxTurns` cap is 20, so the nominal worst case is
`maxTurns × maxWaitSeconds`. The run's `Timeout` bounds that: it is
validated at ≤ 3600 s and applied to the run context on both entry
points, so no batch run reaches the wait window's own ceiling. At the
CLI defaults (`--max-turns 20`, `--timeout 600`, and therefore
`maxWaitSeconds` 600) the first turn alone can consume the entire run
budget and the remaining 19 never start.

`ValidateRunConfig` emits a `slog` WARN (not an error) when
`provider.batch.enabled` is set with `maxTurns > 5`, reporting
`maxWaitSeconds` and the `worstCaseSeconds` product, so operators
see the warning at run start without the validator hard-rejecting
an intentional choice. The threshold is advisory: a batch run that
intends to complete several turns should divide the budget
deliberately — for example `maxTurns: 5` and `maxWaitSeconds: 700`
against a `timeout` of 3600.

### Cancellation

Mid-batch run cancellation behaves differently per transport:

- **gRPC** — the harness unblocks immediately and, when
  `provider.batch.cancelBundleOnRunCancel=true`, emits a
  `batch_cancel_request` HarnessEvent so the control plane can cancel
  the matching provider-side batch entry. The flag defaults to
  `false` — the control plane is responsible for deciding whether to
  cancel an entire bundle when a single run drops out, since other
  runs in the same bundle may still want their results. Operators who
  know a run is the sole occupant of its bundle (or who explicitly
  prefer cancel-on-drop) opt in via the flag in `--config`.
- **stdio polling** — the harness best-efforts a cancel call against
  the provider before exiting. The call is fire-and-forget; a failed
  cancel does not block the run's exit. This path lands in phase 4
  (issue #137).

### Results-URL credential guard

The harness-side Anthropic polling client fetches the batch's
JSONL results from a `results_url` the API returns on the polled
batch object. That URL is provider-controlled data, and the fetch
attaches the run's `x-api-key` unconditionally, so before issuing
the GET the client validates that the URL's scheme is `https` and
its host is `anthropic.com` or a subdomain of it. A `results_url`
pointing anywhere else is treated as a misframed upstream response
or an active exfiltration attempt and rejected — either way the
credential must not be sent to an unverified host. The one
exception is when the client's `baseURL` has been overridden to a
loopback address (an `httptest` fixture in the test suite): in that
case a `results_url` sharing the same loopback host is accepted, so
the check does not require the test server to mint URLs on
`*.anthropic.com`.

### Bedrock

Bedrock batch via `CreateModelInvocationJob` is **out of scope in
v1**. The wire shape and capability surface diverge from the
Anthropic / OpenAI batch endpoints far enough that a separate
adapter design is needed; phase 6 ([issue #139](https://github.com/rxbynerd/stirrup/issues/139)) evaluates feasibility
and either lands the adapter or files a deferral issue. Until then,
`provider.type=bedrock` with `provider.batch.enabled=true` fails
validation with `batch is not supported for provider type "bedrock"
in v1`.

For the current status of the Bedrock follow-up, run `gh issue list
-L batch` and filter for the Bedrock label.
