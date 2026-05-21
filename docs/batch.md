## Batch provider mode

### What it is

Async batch submission routes every provider turn through the
provider's batch endpoint (Anthropic
`/v1/messages/batches`, OpenAI `/v1/batches`) rather than the live
streaming endpoint. The provider returns the assistant message
asynchronously, in exchange for which the harness pays roughly half
the per-token price. Provider SLAs cap completion at 24 hours per
batch.

In practice this turns a streaming turn that completes in seconds
into an async turn that can take anywhere from a few seconds to a
full day, in exchange for a ~50% discount on input and output tokens.

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
  and code reviews rather than the 24-hour wait that batch implies —
  and the opt-in exists so an operator who has deliberately chosen
  async planning has to acknowledge the footgun.
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
24h tail across many runs and giving the harness a single round-trip
to wait on. The phase-2 `controlPlaneBatchClient` is the default for
gRPC operators.

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

### Cost and budget caveats

Two operator-visible gaps follow from the 24h wait window:

**Budget overrun gap.** `MaxCostBudget` is enforced in the agentic
loop *after* a batch turn completes. A single batch turn can return
tokens that push the run's running cost over budget by one turn's
worth before the loop catches it. The shortfall is bounded by one
turn's tokens, but it is not zero — operators sizing
`MaxCostBudget` for a batch run should leave headroom for the most
expensive single response the model can produce, not the run-average
turn cost.

**Long-lived credential exposure.** A 24h batch wait keeps the
provider's API credentials live in memory for 24h, against ~120s for
a streaming turn. Operators using `WebIdentityAWSSource` (or any
other `credential.Source` backed by a short-lived federated token)
should confirm their `CredentialsCache` TTL covers the full
`MaxWaitSeconds` window — a refresh that fires mid-wait can leave
the harness holding stale credentials when the batch completes.

### `MaxTurns` × 24h warning

The default `MaxTurns` cap is 20. With the 20-turn default, a batch
run can take up to 20 × 24 = 480 hours (20 days) in the worst case.
`ValidateRunConfig` emits a `slog` WARN (not an error) when
`provider.batch.enabled` is set with `maxTurns > 5`, so operators
see the warning at run start without the validator hard-rejecting
an intentional choice. The threshold is advisory: production batch
runs should set `maxTurns <= 5` unless the operator has a specific
reason to accept the extended worst-case.

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
