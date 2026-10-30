# Configuration

The harness reads its configuration from a single `RunConfig` — a
declarative document that names a concrete implementation for each of
the [thirteen components](architecture.md#the-thirteen-components).
Two equivalent surfaces are supported:

- A JSON file passed via `--config <path>`. The schema mirrors
  [`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto)
  exactly; the Go types in
  [`types/runconfig.go`](../types/runconfig.go) are what the loader
  unmarshals into.
- Individual CLI flags. Each flag corresponds to one field in the
  `RunConfig`; together the flags cover the most common compositions
  without writing a config file.

The proto definition is the source of truth; the JSON encoding uses
the proto field names verbatim. The CLI loader uses
`encoding/json.DisallowUnknownFields`, so a typo in a field name
fails fast with a clear error rather than being silently dropped.

## Precedence

When `--config`, piped stdin, `STIRRUP_CONFIG`, or explicit flags
are passed, the order of precedence is:

1. **Base config** — one of the following, in descending priority:

   | Rank | Source | Notes |
   |---|---|---|
   | 1 | `--config <path>` flag | Explicit. Wins outright. |
   | 2 | `--config -` flag | Reads the base from piped stdin. |
   | 3 | Auto-detected piped stdin | Triggered when `--config` is absent and stdin is a pipe or a redirected regular file (not a TTY). |
   | 4 | `STIRRUP_CONFIG` env var | Filesystem path, or the literal `-` to opt into stdin. Consulted only when `--config` is absent. |

   The four sources are mutually exclusive. Combining a path-shaped
   source (`--config <path>` or `STIRRUP_CONFIG=<path>`) with piped
   stdin is a hard error so operators never have to guess which
   source won. `STIRRUP_CONFIG=-` with piped stdin is allowed because
   the env var is opting into the stdin path, not naming a separate
   base.
2. **Explicit flags** — flags whose `cmd.Flags().Changed(...)` bit is
   set replace the corresponding base field.
3. **Defaults** — flags left at their default value do **not**
   override the base. This is what makes `--config` ergonomic:
   defaults can stay defaults while the file's intent is preserved.

When the env var is the chosen source, the harness emits a single
`slog.Debug` line naming the path (or `-`) so operators can audit
precedence at debug log level without grepping the source.

The positional `prompt` argument is a fallback only. It fills the
prompt slot when the base omits it and `--prompt` is not set, but
neither the base's `prompt` nor an explicit `--prompt` is overridden
by a positional.

### `STIRRUP_CONFIG` ergonomics

`STIRRUP_CONFIG` mirrors `KUBECONFIG` and `AWS_CONFIG_FILE`: setting
it once per shell session removes the need to retype `--config` on
every invocation, while leaving the flag itself available for ad-hoc
overrides.

```sh
export STIRRUP_CONFIG="$HOME/.config/stirrup/default.json"

# Uses the env-var config.
stirrup harness --prompt "explain this repo"

# Explicit --config wins over the env var for this one invocation.
stirrup harness --config ./experimental.json --prompt "try the new model"

# Pipe a config in and ignore the env var path; STIRRUP_CONFIG=- opts
# into stdin without naming a separate source.
STIRRUP_CONFIG=- some-tool emit-config | stirrup harness --prompt "x"
```

## Building RunConfigs interactively

For development workflows that compose a `RunConfig` incrementally —
or just to capture what a flag-only invocation *would* have run — the
harness ships two surfaces that complement `--config <path>`:

- `stirrup run-config` emits a fully-resolved `RunConfig` JSON
  document to stdout without invoking the agentic loop. Every
  RunConfig-producing flag from `stirrup harness` is honoured; a
  base config can arrive via stdin (the pipeline pattern) or
  `--config <path>`.
- `stirrup harness --output-runconfig <path>` writes the resolved
  RunConfig as JSON to `<path>` (use `-` for stdout) and exits
  without running. The captured document is exactly what would have
  been handed to the loop, so it can be checked into source control
  for replay or compared with `diff` between runs.

The two surfaces together support a UNIX-style pipeline where each
stage layers one more adjustment before the final stage runs the
agent:

```sh
stirrup run-config --model claude-opus-4-7 \
  | stirrup run-config --max-turns 100 \
  | stirrup run-config --mode execution --executor container \
  | stirrup harness --prompt "refactor X"
```

`stirrup run-config` exposes three subcommand-specific flags:

| Flag | Default | Notes |
|---|---|---|
| `--validate` | `false` | Run `types.ValidateRunConfig` on the resolved document and exit non-zero on failure. Without this flag, partial / chained configs are emitted as-is so a downstream stage can complete them. |
| `--compact` | `false` | Emit single-line JSON instead of indented (2-space). |
| `--redact` | `false` | Apply `RunConfig.Redact()` before emit. Rewrites `secret://` references to `secret://[REDACTED]`. The result is share-safe but no longer runnable as-is — operators who need a runnable replay should omit the flag. |

`stirrup harness --output-runconfig` always emits the unredacted form
because its purpose is exact replay; pipe through
`stirrup run-config --redact` when a share-safe artifact is wanted.

### Reading from stdin

`stirrup harness` accepts a base `RunConfig` from stdin in two shapes:

- **`--config -`** is the explicit, scripted opt-in. It always
  reads stdin and fails loudly if stdin is a terminal or carries no
  data.
- **Auto-detection on a non-TTY pipe** triggers when `--config` is
  unset and stdin is a named pipe (`|`) or a regular-file
  redirection (`< config.json`). Other non-TTY shapes — including
  `< /dev/null` and the stdin handed to tests by `go test` — fall
  through to flag-only construction so the harness does not trap
  noninteractive automation. The auto-stdin trigger fires only for
  named-pipe and regular-file redirects; a character-device or empty
  stdin such as `stirrup harness < /dev/null` deliberately falls
  through to flag-only construction rather than erroring, so an
  operator who needs stdin treated as a config source must opt in
  explicitly with `--config -`.

Piping a config into `stirrup harness` consumes stdin at startup,
before transport initialisation, so the stdio transport sees EOF for
any subsequent control-event input. This matches the batch shape the
pipeline pattern targets — operators who need interactive control
should use `--transport grpc`.

## Dry-run preflight

`--output-runconfig` answers "what *would* run?"; `--dry-run` answers
"*could* it run?". A dry-run takes the resolved `RunConfig` through every
initialisation step short of the first agentic turn — validate the
config, construct every component, resolve every credential, and probe
the reachability of the provider, MCP servers, trace backend, and egress
allowlist — then prints a per-step report and exits. It is the
preflight check for the side-channel concerns `ValidateRunConfig` cannot
see: a missing API key, an unreachable MCP server, a Cedar policy that
will not parse, a container image that is not pulled, a trace collector
that is down.

```sh
stirrup harness --config run.json --dry-run
```

A dry-run **never spends provider tokens**. Provider probes hit only a
metadata endpoint — Anthropic and OpenAI `GET /v1/models`, the
publisher-model list for Vertex AI — and never a completion endpoint.
The Bedrock probe validates the AWS credential chain rather than calling
a billable runtime operation (so it confirms credentials resolve, not
that the Bedrock endpoint is reachable). A container-executor dry-run is
read-only: it pings the engine socket and checks the image is present
locally without creating a container, starting the egress proxy, or
pulling the image. Pass `--no-probe-executor` to suppress even that
contact, recording the executor step as a `skip`.

The trace probe is the one step that reaches a live backend: for
`traceEmitter.type=otel` it exports a single throwaway span (tagged
`stirrup.preflight=true` so dashboards and alert rules can filter it) to
confirm the collector is reachable. Pass `--no-probe-trace` to suppress
all collector contact. The workspace-export probe (`gs://` destinations)
and the `gcs` trace probe authenticate via the same
`gcp-workload-identity` default the real run uses, so both fail outside a
GCP runtime that provides a metadata server unless an explicit credential
is configured.

The report lists each step with one of four statuses:

- `ok` — the component constructed and (where applicable) its probe
  succeeded.
- `skip` — the step was intentionally not run: the component has no
  network probe (a local executor, a `jsonl` trace file), the feature is
  not configured (no MCP servers, no workspace export), or a
  `--no-probe-*` gate suppressed it.
- `warn` — the step found a condition the run can proceed with: an
  optional MCP server that did not answer or does not advertise every
  allowlisted tool, or a `tools.lazyLoading.eager` name that matches no
  tool. A warning does not fail the report.
- `fail` — construction or a probe failed. The step names the component,
  carries the underlying error, and — where a concrete next step is
  known — a remediation hint.

When `tools.lazyLoading.enabled` is set, the `lazy-tools` step reports the
tool set a run would send. Its detail gives the search strategy, the
eager and deferred counts in total and per group (built-in, control
plane, each MCP server), and, for every provider and model the router can
select, whether the turn uses the provider's native tool search or the
harness `tool_search`, with the reason when it is the harness one:
`strategy harness`; an `openai-responses` provider whose endpoint profile
is not `openai`, reported as `endpoint profile <name>`; the model missing
from the native search allow-list; or no eager tool besides
`tool_search`. The endpoint profile is named only when the model would
have a hosted search on the `openai` profile; otherwise the allow-list
reason is given. Each eager
name that matches no tool adds a `warn` step named
`lazy-tools:eager:<name>`. The step builds the tool set with the same
code a real run uses, so the counts match the run's. It counts MCP tools
from the `tools/list` probe: under `--no-probe-mcp`, or when a server does
not answer, those tools are left out of the counts, the detail says so
and defers the deferral decision to run start, and `eager` names that
start with `mcp_<server>_` for such a server are not reported as unknown.
The step never fails the dry-run. See
[Lazy tool loading](#lazy-tool-loading).

The human-readable report goes to **stderr**; `--output=json` emits the
structured report (a `PreflightReport`: a `steps` array plus an `ok`
boolean) to **stdout** instead, so it can be parsed or stored. Routing
the report to stderr keeps stdout free for a captured config when
`--dry-run` is combined with `--output-runconfig`.

### Probe gates

Each network-touching probe can be suppressed for cost-controlled or
air-gapped environments. A suppressed probe records `skip` and does not
fail the run:

| Flag | Skips |
|---|---|
| `--no-probe-provider` | The provider metadata probe (all configured providers). |
| `--no-probe-mcp` | The MCP `initialize` / `tools/list` handshake for every configured server. |
| `--no-probe-trace` | The trace-emitter reachability probe (`otel` flush, `gcs` bucket check). |
| `--no-probe-egress` | The egress-allowlist DNS resolution (container executor in `allowlist` network mode). |
| `--no-probe-executor` | The container-engine probe (socket ping + image-present, container executor only). The executor step then records `skip`; no engine is contacted. No effect on `local`/`api` executors, which construct without an engine. |
| `--dry-run-timeout` | Not a gate — bounds the total preflight wall-clock. Defaults to `30s`. |

The egress-allowlist probe (`--no-probe-egress`) is DNS-only: it
resolves each allowlist entry's hostname (a wildcard entry resolves
its base domain) but never opens a TCP connection to the
destination. A TCP dial would be a stronger reachability signal, but
turns a cheap dry-run into a fan-out of outbound connections to
every allowlisted host, which is both slow and surprising for a
config-check command. DNS resolution catches the common
misconfiguration — a typo'd or non-existent hostname — without that
cost.

A `--no-probe-*` gate or `--dry-run-timeout` supplied **without**
`--dry-run` is an invalid flag combination and exits `4` (see
[Exit codes](#exit-codes)). Silently ignoring them would hide an
operator typo — for example, `--no-probe-provider` on a real run that
then contacts the provider anyway.

MCP probe failures are treated like every other probe: a configured
server that does not answer the handshake fails the dry-run. An operator
who expects a server to be unavailable suppresses its probe with
`--no-probe-mcp`.

### Exit codes and composition

| Outcome | Exit code |
|---|---|
| Every step `ok`, `skip` or `warn` | `0` |
| One or more steps `fail` | `1` |
| Invalid flag combination | `4` |

`--dry-run` composes with `--output-runconfig`: both run. The order is
validate → preflight → write the captured config → exit, so the resolved
config is captured alongside the report even when a probe fails (a
failed `--output-runconfig` write still takes precedence as an I/O error,
exit `3`).

## CLI flags

`stirrup harness --help` is authoritative. The table below documents
the same flags grouped by concern.

Without a prompt on an interactive terminal, `stirrup harness` prints a
curated usage hint — a grouped, example-led subset of the flags below —
rather than the bare prompt-required error, and a bare `stirrup` prints
a two-subcommand entry-point hint. Both are first-contact orientation
only; `stirrup harness --help` remains authoritative for the full flag
reference.

### Required

| Flag | Default | Notes |
|---|---|---|
| `--prompt`, positional arg | (required) | User prompt. |

### Run identity and shape

| Flag | Default | Notes |
|---|---|---|
| `--config <path>` | (none) | JSON `RunConfig`. |
| `--mode`, `-m` | `planning` | One of `execution`, `planning`, `review`, `research`, `toil`. `planning`, `review`, `research`, and `toil` are read-only (no writes, no shell); `execution` is the editable mode. The default is `planning` so a bare invocation has no write or shell capability — pass `--mode execution` to enable edits and shell. See [Read-only modes](#read-only-modes). |
| `--name` | (none) | Human-readable session label, attached to logs/traces. Metadata only — not injected into the prompt. |
| `--workspace`, `-w` | cwd | Workspace directory. |
| `--max-turns` | `20` | Hard-capped at 100. |
| `--timeout` | `600` | Wall-clock seconds for one run — the primary run and, afresh, each follow-up run; capped at 3600. Not a session cap. |
| `--temperature` | (unset → `0.1`) | Sampling temperature forwarded to the provider on every turn. Range `0.0`–`2.0` (the union of provider-side ranges; see [Limits and budgets](#limits-and-budgets)). Omit the flag to inherit the harness default; pass an explicit `0` for greedy decoding. The runtime distinguishes "flag absent" from `--temperature=0` via cobra's `Changed()` bit. |
| `--reasoning-effort` | (none) | Provider-neutral reasoning depth: `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`. Empty says nothing on the wire and leaves the model on its provider default. Adapters project it onto the model's native control — Gemini `generationConfig.thinkingConfig.thinkingLevel`, Anthropic `output_config.effort`, OpenAI `reasoning_effort` (Chat Completions) or `reasoning.effort` (Responses) — and models with no probed control ignore it, so a single config stays portable across providers. Per-model acceptance is narrower than the enum (Gemini 3.7 Flash rejects `minimal`, no Claude or GPT-6 model accepts it, GPT-5.4-onward models document no `minimal` level, GPT-5.4 and GPT-5.5 stop at `xhigh`, and Gemini has no `xhigh` or `max`); the [provider quirks registry](provider-quirks.md#32-reasoning-effort) rejects an unaccepted level before the request is sent. JSON path: `reasoningEffort`. |
| `--reasoning-summary` | (none) | Whether the model's reasoning text is requested and streamed to the transport as `thinking_delta` events: `auto` or `off`. Empty is treated as `auto`, except that `openai-responses` requests no summary unless the value is explicitly `auto`. Provider-neutral like `--reasoning-effort`: see [Reasoning summaries](#reasoning-summaries) for how each adapter projects it, including the unset value. `off` stops the harness streaming reasoning text and, where the provider has a request control for it (Gemini, OpenAI Responses), does not request it. Conversation history is unaffected either way. JSON path: `reasoningSummary`. |
| `--log-level` | `info` | One of `debug`, `info`, `warn`, `error`. |

### Loop behaviour

| Flag | Default | Notes |
|---|---|---|
| `--max-tool-parallel` | `0` | Maximum async tool calls dispatched concurrently in a single turn. Range `1`–`16` (hard ceiling enforced by `ValidateRunConfig`); `0` resolves to the library default of `4`. JSON path: `toolDispatch.maxParallel`. |
| `--escalate-tool-choice` | `false` | Recover from a first-turn no-tool answer on a workspace-dependent task by retrying with provider-native required tool choice (a stronger prompt where the provider does not support forcing). On Anthropic and Gemini the native retry adds a short synthetic user message before forcing a tool, because some models on those APIs reject a request that ends on a model turn. A turn that stops on `refusal` or `model_context_window_exceeded` is not retried. A run continuing a previous run's history (`followUpHistory: "continue"`) counts the tool calls in that history as tool use. Off by default (issue #230). JSON path: `toolChoiceEscalation.enabled`. |
| `--escalate-tool-choice-max-retries` | `0` | Maximum forced retries per inner-loop run. Range `1`–`3`; `0` resolves to the default of `1`. No effect unless `--escalate-tool-choice` is set. JSON path: `toolChoiceEscalation.maxRetries`. |

### Provider

| Flag | Default | Notes |
|---|---|---|
| `--provider` | `anthropic` | One of `anthropic`, `bedrock`, `openai-compatible`, `openai-responses`, `gemini`, `replay`. `replay` serves recorded model output from `--replay-file` and needs no credential; see [Replay](#replay). |
| `--model` | `claude-sonnet-4-6` for `anthropic`; none otherwise | Model id for the static / per-mode router. Required for every provider other than `anthropic`; omitting it exits 1 with an error naming the provider. |
| `--prompt-model` | (none) | Model identity the system prompt templates render against, without changing the wire model. Combine with `--model` to compare a prompt tuned for one model against another. Empty derives the prompt model from the effective model. JSON path: `promptBuilder.promptModel`. See [System prompt templating](#system-prompt-templating). |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` for `anthropic`; `secret://OPENAI_API_KEY` for `openai-compatible` / `openai-responses`; none for `bedrock` / `gemini` / `replay` | A `secret://` reference. API keys never live in `RunConfig`. The default applies only when neither the flag nor `--config` supplies a value and no `credential` block is set; `--api-key-ref=""` sends no key, which is valid only for `openai-compatible` / `openai-responses` at a custom `--base-url` (a keyless local server); provider construction rejects an empty reference for any other provider or for the default OpenAI endpoint. Rejected for `gemini` (Vertex uses GCP IAM) and for `replay` (no model is called). |
| `--base-url` | (none) | Provider base URL. Required for Azure / gateway scenarios. |
| `--api-key-header` | (none) | Header name. Empty = `Authorization: Bearer`; set to `api-key` for Azure key auth. |
| `--query-param key=value` | (none) | Repeatable. Adds query parameters to every provider request URL — e.g. `--query-param api-version=preview` for Azure. Keys here override duplicates already encoded in `--base-url`. |
| `--provider-compat-profile` | (none) | Closed enum that loads a provider-quirks compatibility profile. Only legal value in v1: `"zai-glm"` (Z.ai GLM legacy `max_tokens` key + `tool_stream: true` extension). Unknown values fail at startup. JSON path: `provider.compatProfile`. |
| `--provider-endpoint-profile` | (derived) | Closed enum declaring which implementation of the OpenAI wire serves `--base-url`: `openai`, `azure-openai`, `bedrock-mantle`, or `generic`. Empty derives `openai` for OpenAI's own API (`https://api.openai.com/v1`, or no `--base-url`) and `generic` for any other base URL. Legal only for `openai-compatible` and `openai-responses`; `azure-openai` and `bedrock-mantle` require `--base-url`. Set `openai` to declare a relay in front of OpenAI's API. See [Endpoint profiles](providers.md#endpoint-profiles). JSON path: `provider.endpointProfile` (and `providers[name].endpointProfile`). |

Wire-shape divergences between provider/model pairs (e.g. OpenAI
reasoning-class sampling-param omissions, Z.ai GLM legacy field
names, Gemini 3.x `thoughtSignature` preservation) are not exposed
on `RunConfig`. They are first-party rules in the
`harness/internal/provider/quirks` registry, applied per-stream by
the adapter. The `--provider-compat-profile` and
`--provider-endpoint-profile` flags are the only operator-facing
surfaces that influence quirk resolution; all other rules apply
automatically based on `provider.type` and `model`.
Introspect with `stirrup providers quirks --provider X --model Y`;
full reference at [`provider-quirks.md`](provider-quirks.md).

For the full per-adapter wire-format reference, including Azure
Foundry notes and intentional exclusions, see
[`providers.md`](providers.md).

### Retry policy

The harness retries transient provider failures (HTTP 408, 409, 429,
500, 502, 503, 504 and transport-level timeouts) with exponential
backoff and full jitter. `Retry-After` and `Retry-After-Ms` headers
are honoured when present and bounded by the configured max delay.
A 429 whose error body reports an exhausted billing, spend, or quota
limit is not retried, since retrying cannot restore access (see the
classification table below).

| Flag | Config field | Default | Hard ceiling |
|---|---|---|---|
| `--provider-retry-max-attempts` | `provider.retry.maxAttempts` | `3` | `5` |
| `--provider-retry-initial-delay` | `provider.retry.initialDelayMs` | `500ms` | — |
| `--provider-retry-max-delay` | `provider.retry.maxDelayMs` | `16s` | `60s` |
| `--provider-retry-wall-clock` | `provider.retry.wallClockBudgetMs` | `90s` | `300s` |

`maxAttempts` is the total number of HTTP attempts including the
first, so the default value of `3` permits two retries. A value of
`1` disables retries. `initialDelayMs: 0` is treated as unset and
the defaulter substitutes 500ms. To request a 1ms initial delay
(the minimum resolvable value), set `1`. Negative values are
rejected.

`ValidateRunConfig` fills the documented defaults when a field is
left at its zero value, so leaving every flag unset behaves
identically to passing no retry block at all. CLI flags apply only
to the default provider — per-named-provider retry policy (under
`providers.<name>.retry`) requires `--config`.

The wall-clock budget is bounded by the run's `--timeout`; setting
`--provider-retry-wall-clock` higher than `--timeout` is valid but
the effective ceiling becomes the remaining run timeout.

Honoured by every adapter. `anthropic`, `gemini`, and
`openai-responses` share the same `DoWithRetry` integration as
`openai-compatible`: the resolved `RetryPolicy` governs only the
pre-stream request/response exchange (connection errors, or a
429/5xx/529 on the initial response). A retryable failure after the
stream has started is retried by the agentic loop as a whole model
turn, reusing these settings and capped at two retries per turn.
`bedrock` has no raw `*http.Client` seam — `ConverseStream` goes through the AWS SDK's own transport —
so `maxAttempts` and `maxDelayMs` are mapped onto the SDK's Standard
retryer instead; `initialDelayMs` and `wallClockBudgetMs` have no
SDK-native equivalent and are not applied to `bedrock`.

The pre-stream response is classified the same way for every adapter
that uses `DoWithRetry` (`anthropic`, `gemini`, `openai-compatible`,
and `openai-responses`), whichever provider the error codes come from:

| Response | Retried |
|---|---|
| 429 with `error.code` `insufficient_quota`, `credit_balance_exhausted`, `organization_spend_limit_exceeded`, `project_spend_limit_exceeded`, or `organization_usage_limit_exceeded`, or with `error.type` `insufficient_quota` | No: retrying cannot restore access until the limit is raised |
| 429 with any other code (for example `slow_down` or `rate_limit_exceeded`) or none | Yes, honouring `Retry-After` |
| 503 `server_is_overloaded` | Yes, honouring `Retry-After` |
| 403, including `misalignment_policy_violation` | No |

A quota 429 logs `provider_quota_exhausted` at Warn with the matched
code and records `quota_exhausted` as the `provider.retry.outcome`
attribute of `stirrup.harness.provider_retry_outcomes`. The quota check
reads at most 4 KB of the 429 body, so a larger error body is
classified by status alone. The codes are OpenAI's documented billing
codes (see [`providers.md`](providers.md#openai-responses-api)); they
have not been probed against the live API.

Defaults are tuned for the cost of one extra coding-loop turn rather
than the OpenAI Python SDK's 8 s cap: a coding agent typically has
many minutes per turn and benefits more from clearing a transient
upstream blip than from failing fast.

### Vertex AI / Gemini

| Flag | Default | Notes |
|---|---|---|
| `--gcp-project` | (none) | GCP project ID. Required when `--provider=gemini`. |
| `--gcp-location` | `global` | Vertex AI location: `global` or a region like `us-central1`. |
| `--gcp-credentials-file` | (none) | Path to a Google service account JSON key file. When set, implies `credential.type=gcp-service-account`; otherwise the credential layer falls back to Application Default Credentials. |

Gemini thinking depth is not a provider-scoped flag: the generic
`--reasoning-effort` flag (see
[Run identity and shape](#run-identity-and-shape)) projects onto
`generationConfig.thinkingConfig.thinkingLevel` for Gemini models.

### Replay

| Flag | Default | Notes |
|---|---|---|
| `--replay-file` | (none) | Path to a JSONL trace recording whose `turn_record` events supply each turn's model output, in order. Required when `--provider=replay`; rejected on every other provider, and rejected when it contains a `..` segment, like the other host paths a control plane can set. The harness opens the path from its own filesystem, relative to its working directory when not absolute; the eval runner resolves a suite's relative paths against the suite file instead. Must be a regular file of at most 64 MiB. JSON path: `provider.replayFile`. |

The `replay` provider calls no model: each turn's output is the next
`turn_record` of the recording, so the harness drives its real tool
registry, executor, permission policy and trace emitters over a
scripted conversation with no credential and no network. The model
defaults to `replay` and is a label only. A recording is either the
trace an earlier run wrote through `traceEmitter.type=jsonl` or a
hand-written file of `turn_record` lines; the format and limits are in
[`providers.md`](providers.md#replay). `apiKeyRef`, `credential` and
`baseUrl` are rejected alongside the replay provider, and `--dry-run`
records a skip for its provider probe because there is nothing to
reach.

A `--provider` switch on a `--config` document drops the fields the
previous type owned when their own flag was not given, and rewrites a
static router that named the previous type: `--provider anthropic` on
a replay document drops `replayFile` and the `replay` model label (so
the anthropic default model applies, or `--model` is demanded for a
provider without one) and runs it live; `--provider replay` drops the
document's `apiKeyRef`, mirroring how a switch to `gemini` clears a
stale key. Only the top-level `provider` is switched; entries in the
`providers` map are untouched. A leftover `credential` block or
`baseUrl` on a switch to `replay` is not dropped and fails validation.
The eval suites rely on the switch to live: see
[`eval.md`](eval.md#replay-doubles).

### Anthropic Workload Identity Federation

| Flag | Default | Notes |
|---|---|---|
| `--anthropic-federation-rule-id` | (none) | Federation rule ID (`fdrl_...`). Implies `credential.type=anthropic-wif`. Env fallback: `ANTHROPIC_FEDERATION_RULE_ID`. |
| `--anthropic-organization-id` | (none) | Anthropic organization UUID. Required with WIF. Env fallback: `ANTHROPIC_ORGANIZATION_ID`. |
| `--anthropic-service-account-id` | (none) | Service account ID (`svac_...`). Required with WIF. Env fallback: `ANTHROPIC_SERVICE_ACCOUNT_ID`. |
| `--anthropic-workspace-id` | (none) | Workspace ID (`wrkspc_...`) or `default`. Conditional. Env fallback: `ANTHROPIC_WORKSPACE_ID`. |
| `--anthropic-from-github-actions` | `false` | Enable GitHub Actions OIDC token source. Reads `ACTIONS_ID_TOKEN_REQUEST_URL` / `ACTIONS_ID_TOKEN_REQUEST_TOKEN`. Implicit selection from env presence is rejected — explicit opt-in is required. The `ANTHROPIC_IDENTITY_TOKEN_FILE` and `ANTHROPIC_IDENTITY_TOKEN` env vars also infer the file / env token sources respectively when unset by `--config`. |

Walkthrough: [`anthropic-wif.md`](anthropic-wif.md).

### Azure Workload Identity Federation

| Flag | Default | Notes |
|---|---|---|
| `--azure-tenant-id` | (none) | Azure AD tenant UUID. Implies `credential.type=azure-workload-identity`. Use with `--provider=openai-compatible` or `openai-responses` against Azure OpenAI / Foundry. The `TokenSource` (file / github-actions-oidc / aws-irsa / azure-imds) must come from `--config`. |
| `--azure-client-id` | (none) | App Registration / federated identity credential client ID (UUID). Required with `--azure-tenant-id`. |
| `--azure-scope` | `https://cognitiveservices.azure.com/.default` | OAuth2 scope for the Entra access token. Override only for non-default Azure audiences. |

Walkthrough: [`azure-workload-identity.md`](azure-workload-identity.md).

### OpenAI Workload Identity Federation

| Flag | Default | Notes |
|---|---|---|
| `--openai-identity-provider-id` | (none) | OpenAI identity provider ID. Implies `credential.type=openai-wif`. Use with `--provider=openai-compatible` or `openai-responses` against the OpenAI API. Env fallback: `OPENAI_IDENTITY_PROVIDER_ID`. |
| `--openai-service-account-id` | (none) | OpenAI service account ID. Required with `--openai-identity-provider-id`. Env fallback: `OPENAI_SERVICE_ACCOUNT_ID`. |
| `--openai-subject-token-type` | (none) | RFC 8693 subject token type URN. Optional; defaults to `urn:ietf:params:oauth:token-type:jwt`. Env fallback: `OPENAI_SUBJECT_TOKEN_TYPE`. |
| `--openai-from-github-actions` | `false` | Enable GitHub Actions OIDC token source, setting the audience to `https://api.openai.com/v1`. Implicit selection from env presence is rejected — explicit opt-in is required. The `OPENAI_IDENTITY_TOKEN_FILE` and `OPENAI_IDENTITY_TOKEN` env vars also infer the file / env token sources respectively when unset by `--config`. |

The exchange audience is set on the `tokenSource` (canonically
`https://api.openai.com/v1`), not in the exchange body. Walkthrough:
[`openai-wif.md`](openai-wif.md).

### Components

| Flag | Default | Notes |
|---|---|---|
| `--executor` | `local` | One of `local`, `container`, `k8s`, `k8s-sandbox`, `api`, `none`. `k8s-sandbox` is the [Agent Sandbox CRD variant](executors/k8s-agent-sandbox.md) of `k8s`. `none` has no execution surface at all — no local filesystem or shell access — for MCP-only / server-side-tool runs. See the naming-collision note below. |
| `--container-runtime` | (none) | Per-`executor` closed set. For `container` (host OCI runtime): `runc`, `runsc` (gVisor), `kata`, `kata-qemu`, `kata-fc`, `kata-clh` — must be registered with the host Docker/Podman daemon. For `k8s` (Pod `RuntimeClassName`): `runc`, `gvisor`, `kata-qemu`, `kata-fc`, `kata-clh` — note the name is `gvisor`, not `runsc`. `k8s-sandbox` is gVisor-only: empty or `gvisor`, any other value is rejected. Empty = engine default for `container`, cluster-default RuntimeClass for `k8s`. |
| `--k8s-namespace` | (none) | Namespace for the `k8s` / `k8s-sandbox` sandbox Pod. Required when `--executor=k8s` or `--executor=k8s-sandbox`. JSON path: `executor.k8sNamespace`. |
| `--k8s-kubeconfig` | (none) | Path to a kubeconfig for the `k8s` / `k8s-sandbox` executors. An explicit value wins even in-cluster; empty prefers in-cluster config, then `$KUBECONFIG`. JSON path: `executor.k8sKubeconfig`. |
| `--k8s-node-selector` | (none) | Repeatable `key=value` `nodeSelector` constraining where the `k8s` / `k8s-sandbox` Pod schedules (e.g. `--k8s-node-selector disktype=ssd`). JSON path: `executor.k8sNodeSelector`. |
| `--k8s-service-account` | (none) | ServiceAccount name for the `k8s` / `k8s-sandbox` Pod. Empty uses the namespace `default`. The token is never automounted regardless. JSON path: `executor.k8sServiceAccount`. |
| `--k8s-egress-proxy-url` | (none) | URL the `k8s` / `k8s-sandbox` Pod routes `HTTP_PROXY`/`HTTPS_PROXY` (and the lower-case `http_proxy`/`https_proxy`, which is what `git` honours for a plain-http proxy URL) through. Required when the executor is `k8s` or `k8s-sandbox` and the network mode is `allowlist`; rejected otherwise. JSON path: `executor.k8sEgressProxyUrl`. |
| `--edit-strategy` | `multi` | One of `whole-file`, `search-replace`, `udiff`, `multi`. `composite` is reachable only via `--config`. |
| `--verifier` | `none` | One of `none`, `test-runner`, `llm-judge`. `composite` is reachable only via `--config`. |
| `--git-strategy` | `none` | One of `none`, `deterministic`. |
| `--permission-policy-file` | (none) | Path to a Cedar policy file. When set and the policy type is unset elsewhere, implies `permissionPolicy.type=policy-engine`. Starters live under [`examples/policies/`](../examples/policies/). |
| `--code-scanner` | (none) | One of `none`, `patterns`, `semgrep`. `composite` is accepted only via `--config` (it requires `codeScanner.scanners`). Empty defers to the mode-aware default (`patterns` for execution, `none` for read-only modes). |
| `--guardrail` | (none) | GuardRail classifier type: `none`, `granite-guardian`, `shieldstral`, `cloud-judge`, `composite`. `composite` requires `--config` (`guardRail.stages`). JSON path: `guardRail.type`. See [`guardrails.md`](guardrails.md). |
| `--guardrail-endpoint` | (none) | Classifier endpoint URL for the `granite-guardian`, `shieldstral`, or `cloud-judge` adapter (http/https; a path such as `/v1/chat/completions` is allowed). JSON path: `guardRail.endpoint`. |
| `--guardrail-api-key-ref` | (none) | Secret reference for GuardRail API key (e.g. `secret://MISTRAL_API_KEY`). Used by `shieldstral`. JSON path: `guardRail.apiKeyRef`. |
| `--guardrail-model` | (none) | Model identifier for the GuardRail classifier. Empty applies the adapter-defined default: `ibm-granite/granite-guardian-4.1-8b` for `granite-guardian` and `claude-haiku-5-5` for `cloud-judge`. `shieldstral` has no default and requires the id the server exposes (for example `mistralai/Shieldstral-1.0-3B` on vLLM; LM Studio lists its own model ids). The `cloud-judge` default is in Anthropic API format — when the primary provider is Bedrock, set the Bedrock-format ID (e.g. `anthropic.claude-haiku-5-5`). JSON path: `guardRail.model`. |
| `--guardrail-fail-open` | `false` | When set, classifier transport errors / timeouts produce an allow verdict plus a `guard_error` security event instead of blocking the run. Default is fail-closed. Top-level only — governs the whole guardrail tree. JSON path: `guardRail.failOpen`. See [`guardrails.md`](guardrails.md#fail-open-posture). |
| `--tools-profile` | (none) | Model-facing toolset profile. Closed enum: `""`/`default` (no aliasing, internal tool names) or `coding-classic` (terse coding-CLI aliases). Changes only the names the model sees; dispatch identities and gating are unchanged. JSON path: `tools.profile`. See [Toolset profiles](#toolset-profiles). |
| `--lazy-tools` | `false` | Turns on lazy tool loading: tools outside the eager set reach the model only after it searches for them. JSON path: `tools.lazyLoading.enabled`. See [Lazy tool loading](#lazy-tool-loading). |
| `--lazy-tools-strategy` | (none; behaves as `auto`) | How deferred tools are searched: `auto` uses the provider's native tool search where the quirks registry gives the routed provider, endpoint profile and model one (see [Strategies and providers](#strategies-and-providers)), and the harness `tool_search` otherwise; `harness` always uses `tool_search`. JSON path: `tools.lazyLoading.strategy`. |
| `--lazy-tools-eager` | (none) | Repeatable internal tool ID sent on every turn without a search. Unset defers only MCP-imported and control-plane tools; a list defers all remaining tools, built-ins included. Replaces any `tools.lazyLoading.eager` from `--config`. JSON path: `tools.lazyLoading.eager`. |
| `--lazy-tools-max-results` | `0` | How many tools one harness `tool_search` loads. `0` selects 5; the maximum is 20. JSON path: `tools.lazyLoading.maxResults`. |

See also: [`docs/executors/k8s.md`](executors/k8s.md) for the `k8s`
executor's architecture, deployment recipes, egress model, and the full
`executor.k8s*` field reference, and
[`docs/executors/k8s-agent-sandbox.md`](executors/k8s-agent-sandbox.md)
for the `k8s-sandbox` deltas (Sandbox CRD provisioning, gVisor-only
runtime, RBAC).

**`executor.type: "none"` vs. `executor.network.mode: "none"`.** These
are two unrelated settings that happen to share a value:

- `executor.type: "none"` selects the executor tier with no execution
  surface at all — no `ReadFile`, `WriteFile`, `ListDirectory`, or
  `Exec`. It is set via `--executor none`.
- `executor.network.mode: "none"` is the deny-all egress setting on a
  filesystem/shell-capable executor (`container`, `k8s`,
  `k8s-sandbox`) — the executor still reads and writes a workspace
  and runs commands, it just has no network route out. There is no
  top-level `--network` flag; it is set only via `executor.network`
  in a `--config` file (see `full.json` and `k8s-gvisor.json` under
  [`examples/runconfig/`](../examples/runconfig/)).

`executor.type: "none"` and `executor.network` are mutually
irrelevant: the none executor rejects an explicit `executor.network`
block outright (it has no network surface for a mode to constrain),
and `executor.network.mode: "none"` is meaningless without a
filesystem/shell executor to confine.

Under `executor.type: "none"`, a read-only mode (`--mode`
`planning`/`review`/`research`/`toil`) with no explicit
`tools.builtIn` defaults to the non-filesystem built-in tools
(`web_fetch`, `spawn_agent`) rather than the full read-only default —
the none executor has no capability to back the rest.

### Transport

| Flag | Default | Notes |
|---|---|---|
| `--transport` | `stdio` | One of `stdio`, `grpc`. |
| `--transport-addr` | (none) | gRPC target address; required when `--transport=grpc`. |
| `--followup-grace` | `0` | Idle seconds to keep the transport open after each run completes, waiting for a follow-up `user_response`; the window restarts after every run. With a window configured, the CLI bounds the whole session at 10 × (`--timeout` + `--followup-grace`) after the primary run — room for ten follow-ups each waited for through a full window — since it has no orchestrator deadline behind it. Env fallback: `STIRRUP_FOLLOWUP_GRACE`. |
| `--heartbeat-seconds` | `0` | Seconds between `heartbeat` events while a run is in progress. `0` selects the default of 30; any other value must be between 5 and 600, and an out-of-range value fails validation. The interval applies to the primary run and to every follow-up run. A control plane that derives orphan detection from heartbeat arrival should set a value well under its liveness deadline. No env fallback. JSON path: `heartbeatSeconds`. gRPC: `RunConfig.heartbeat_seconds`, honoured by harnesses advertising the `heartbeat_interval` capability. See [the integration guide](integration-guide.md#detecting-a-stalled-or-orphaned-run). |
| `--followup-history` | (unset → `reset`) | The history a follow-up run starts from. `reset` (also selected by an empty value) starts it from its prompt alone. `continue` starts it from the previous run's committed history — every turn whose tool calls all have results — with the prompt appended; tool results that run left unscreened are screened first, and a run that ended `rule_of_two_violation` or `guardrail_blocked` passes no history on. The primary run is unaffected, and the value is accepted but inert without a follow-up window; it takes effect only through the follow-up loop, so an embedder calling `harnessapi.Loop.Run` repeatedly gets no carried history and no warning. No env fallback. JSON path: `followUpHistory`. See [the integration guide](integration-guide.md#follow-up-turns). |

### Tracing

| Flag | Default | Notes |
|---|---|---|
| `--trace <path>` | (none) | JSONL trace path. Implies `--trace-emitter=jsonl` unless overridden. |
| `--trace-emitter` | `jsonl` | One of `jsonl`, `otel`, `gcs`. |
| `--otel-endpoint` | (none) | OTLP endpoint. Falls back to the platform profile's trace endpoint, then to `localhost:4317`. For `http/protobuf` use the gateway base path (e.g. `https://otlp-gateway-prod-us-east-0.grafana.net/otlp`): the exporter appends `/v1/traces`, `/v1/metrics`, and `/v1/logs`, and uses a path that already ends in the signal's own segment as given. Metrics and logs inherit this endpoint, so a per-signal URL belongs in `--otel-metrics-endpoint` or `observability.logsExport.endpoint` instead. The `OTEL_EXPORTER_OTLP_ENDPOINT` variable is not consulted for traces or metrics; see [`observability-cloud.md`](observability-cloud.md#precedence). |
| `--otel-protocol` | (none) | OTLP wire protocol: `""` (defaults to grpc, or to the protocol the platform profile requires), `grpc`, `http/protobuf`. Applies to traces, metrics, and logs. HTTP/JSON is intentionally not supported, and `OTEL_EXPORTER_OTLP_PROTOCOL` is not read. See [`observability-cloud.md`](observability-cloud.md). |
| `--otel-header` | (none) | Repeatable `key=value` HTTP header attached to every OTLP export request. Values may be `secret://` references resolved at exporter init — never pass raw secrets. Requires `--otel-protocol=http/protobuf`, or an `--otel-platform` that requires it (validation rejects headers on the plaintext gRPC path). Explicit flags replace any `headers` map from `--config`. The `OTEL_EXPORTER_OTLP_HEADERS` env var is the SDK-native fallback when no headers are configured. |
| `--otel-metrics-endpoint` | (none) | OTLP endpoint for the metrics exporter when metrics target a different collector than traces. Defaults to the platform profile's metrics endpoint, then to `--otel-endpoint`; with neither, metrics are not exported. |
| `--otel-metrics-temporality` | (none) | Aggregation temporality for exported metrics: `cumulative` or `delta` (`traceEmitter.metricsTemporality`). Empty defers to env `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`, then to `cumulative`. Backends that ingest deltas natively need `delta`; Prometheus-style backends expect `cumulative`. Rejected unless `--trace-emitter=otel`. |
| `--otel-platform` | (none) | Observability platform profile (`traceEmitter.platform`): `cloudwatch`, `langfuse`, or `phoenix`. A profile fills OTLP settings the config leaves unset (per-signal endpoints, headers, request signing, resource and span attributes) and never overrides an explicit field. Rejected unless `--trace-emitter=otel`. See [`observability-cloud.md`](observability-cloud.md#platform-profiles). |
| `--otel-platform-option` | (none) | Repeatable `key=value` setting for the selected platform (`traceEmitter.platformOptions`, e.g. `--otel-platform-option region=us-east-1`). At most 8 entries; keys match `^[a-z][A-Za-z0-9]{0,31}$`; values are at most 256 bytes of printable UTF-8 and are never secrets (`secret://` values are rejected). Which keys a platform accepts is checked when the exporters are built, so `stirrup harness --dry-run` reports a misspelled key and `stirrup run-config` does not. Explicit flags replace any `platformOptions` map from `--config`. |
| `--otel-capture-content` | `false` | Opt the otel emitter into recording prompt/completion content on chat, agent, and tool spans via the GenAI semconv attributes (`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`). Off by default: message content is likely to contain PII. Content is scrubbed for secret-shaped substrings before export. See [`observability-cloud.md`](observability-cloud.md#span-content-capture-opt-in). |
| `--otel-capture-content-max-bytes` | `0` | Cap each captured content attribute value at this many bytes (`traceEmitter.captureContentMaxBytes`). A longer value is shortened at a message or part boundary so JSON stays valid, and the span gains `stirrup.<attribute>.truncated=true`. `0` means no cap; otherwise at least `256`. Requires `--otel-capture-content`. See [`observability-cloud.md`](observability-cloud.md#span-content-capture-opt-in). |
| `--deployment-environment` | (none) | OTel `deployment.environment.name` resource attribute (e.g. `production`, `staging`). Empty falls through to env `OTEL_DEPLOYMENT_ENVIRONMENT`, then to `local`. |
| `--service-name` | (none) | OTel `service.name` resource attribute (`observability.serviceName`, e.g. `triage-agent`). Backends list and separate agents by this value, so deployments that should appear separately set distinct names. Empty falls through to env `OTEL_SERVICE_NAME`, then to `stirrup`. |
| `--service-namespace` | (none) | OTel `service.namespace` resource attribute (e.g. `stirrup-eval`, `team-a`). Empty falls through to env `OTEL_SERVICE_NAMESPACE`, then to `stirrup`. |
| `--log-export` | `none` | Structured-log export: `none` (stderr only) or `otlp` (stderr plus an OTLP log exporter, `observability.logsExport.type`). The exporter uses the traces protocol and headers, over gRPC or `http/protobuf`. The endpoint defaults to `--otel-endpoint` (or the platform profile's logs endpoint); `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` overrides it. With the otel trace emitter, every record emitted during a run carries `trace_id` and `span_id`; see [`observability-cloud.md`](observability-cloud.md#log-trace-correlation). |
| `--correlation-session-id` | (none) | Session identifier stamped on every span as `session.id` and `gen_ai.conversation.id` (`correlation.sessionId`), taking precedence over `--name`. See [Correlation](#correlation) for limits. |
| `--correlation-task-id` | (none) | Task identifier stamped on every span as `stirrup.task.id` (`correlation.taskId`). |
| `--correlation-attempt-id` | (none) | Attempt identifier stamped on every span as `stirrup.attempt.id` (`correlation.attemptId`). |
| `--correlation-user-id` | (none) | Pseudonymous end-user identifier stamped on every span as `user.id` (`correlation.userId`). |
| `--correlation-url` | (none) | Control-plane page for the run, stamped on every span as `stirrup.control_plane.url` (`correlation.url`). |
| `--correlation-label` | (none) | Repeatable `key=value` label stamped on every span as `stirrup.label.<key>` (`correlation.labels`). A pair without `=` or with an empty key is rejected when the config is built. Labels merge key by key over any `labels` from `--config`, and a repeated key keeps its last value. An empty value is rejected. Key shape and limits are checked by `ValidateRunConfig` in both commands; `stirrup run-config` checks them with `--validate`. |
| `--traceparent` | (none) | `stirrup harness` only. W3C `traceparent` of the upstream span that parents the run's root span. Sets the run's context, not its `RunConfig`; see [Upstream parent](#upstream-parent). |
| `--tracestate` | (none) | `stirrup harness` only. W3C `tracestate` carried with `--traceparent`. Ignored without a valid `--traceparent`. |

#### Correlation

The `correlation` block carries the identifiers a control plane uses
for the run in its own UI. The harness stamps each set value on every
span of the run and treats every value as opaque. The values never
enter the model's context, and they are not resource attributes or
metric labels. The block is set in a `--config` file, on stdin, in a
gRPC `task_assignment`, or with the `--correlation-*` flags in the table
above. Each flag replaces only its own field, and an unset flag never
clears a value from the file. An explicitly empty scalar value, such as
`--correlation-user-id ""`, clears that field. Labels from a file
cannot be removed from the command line; a flag can only add or
overwrite a key. The block is omitted when nothing sets it. `stirrup run-config` emits the flags as part of the resolved
config.

```json
"correlation": {
  "sessionId": "conversation-8841",
  "taskId": "task-2207",
  "attemptId": "attempt-2",
  "userId": "u-5c1f0e",
  "url": "https://cp.example.com/tasks/2207/attempts/2",
  "labels": { "agent_profile": "reviewer" }
}
```

| Field | Span attribute | Validation |
|---|---|---|
| `sessionId` | `session.id` and `gen_ai.conversation.id`; takes precedence over `sessionName` (`--name`), which remains the fallback | 1–199 bytes of printable US-ASCII. Langfuse drops session IDs of 200 characters or more. |
| `taskId` | `stirrup.task.id` | At most 255 bytes of printable UTF-8. |
| `attemptId` | `stirrup.attempt.id` | At most 255 bytes of printable UTF-8. |
| `userId` | `user.id` | At most 255 bytes of printable UTF-8. The control plane chooses a pseudonymous value; the harness never interprets it. |
| `url` | `stirrup.control_plane.url` | Absolute `http` or `https` URL with a host, no userinfo and no fragment, at most 1500 bytes. A query string is allowed. |
| `labels` | `stirrup.label.<key>`, one per entry | At most 16 entries. Keys match `^[a-z][a-z0-9_]{0,63}$`; values are at most 255 bytes of printable UTF-8. |

`labels` carries slicing dimensions the harness cannot name in
advance, such as an agent profile or an upstream execution mode. The
run's own mode is already stamped as `run.mode` and needs no label.
The key pattern keeps every key a valid metadata path segment, so a key
can never spell `__proto__`, `constructor`, or `prototype`. The
[`langfuse` profile](observability-cloud.md#the-langfuse-profile) turns
each label into filterable Langfuse metadata.

The control plane derives every identifier server-side and never
takes one from end-user input: a forged session ID places runs inside
another user's session. The URL must not carry credentials. Before a
value is stamped on a span or persisted, the harness passes it through
the secret scrubber, which replaces secret-shaped substrings such as
API keys and `token=` query values with `[REDACTED]`; the scrubber is a
backstop, not a licence to send credentials.

The block is recorded with the rest of the config in JSONL and GCS
trace files and in recordings, and survives `RunConfig.Redact()` apart
from that scrub. A follow-up run in the same session reuses the block.
The span attributes are listed in
[`observability-cloud.md`](observability-cloud.md#span-tree).

#### Upstream parent

`--traceparent` and `--tracestate` nest a CLI run under a span the
caller already holds. Both take the W3C Trace Context header values
verbatim, for example
`--traceparent 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`.
With the `otel` trace emitter, a valid `traceparent` makes that span
the parent of the run's root `invoke_agent stirrup` span, and the run's
spans share the caller's trace ID. Sampling follows the parent: a
sampled parent (trace flags `01`) exports the run, and an unsampled
parent (`00`) suppresses export of every span in it, although
`RunResult.traceReference` still reports the trace ID with
`sampled: false`. An `OTEL_TRACES_SAMPLER` value that is not
`parentbased_*` replaces that behaviour; see
[`observability-cloud.md`](observability-cloud.md#trace-context-exchanged-with-a-control-plane).
The `jsonl` and `gcs` emitters record no spans, so the flags do not
change their trace files.

The flags set the run's context, not its config. They are registered
on `stirrup harness` only, never appear in a `RunConfig`, and are
rejected by `stirrup run-config` as unknown flags, so a replayed or
stored config can never re-parent a run. A value the W3C parser
rejects, such as a wrong length or an unsupported version, is ignored
and the run starts its own trace. `--tracestate` without a valid
`--traceparent` is ignored. The harness logs a warning when a real run
ignores the parent because the value is invalid or the trace emitter
records no spans; `--dry-run` and `--output-runconfig` stay silent.
The parent applies to the primary run; follow-up runs in the same
session start new traces.

### Run output

At end-of-run the harness emits a post-run summary. The `--output`
flag selects the surface:

| Flag | Default | Notes |
|---|---|---|
| `--output`, `-o` | `text` | One of `text`, `json`, `none`. `text` (default) prints the freeform human-readable summary to stderr. `json` emits a single `STIRRUP_RESULT <json>` line on stdout (parseable as [`types.RunResult`](../types/result.go)) and suppresses the stderr summary. `none` suppresses both surfaces. |

When `--output=json` is set alongside `resultSink.type=stdout-json`,
the harness emits the `STIRRUP_RESULT` line exactly once — the flag
wins because it is the more explicit signal. The wire shape is
identical between the two paths, so consumers do not need to detect
which surface produced the line.

`--output` is a `stirrup harness` flag with no `RunConfig` field of its
own. A gRPC control plane driving `stirrup job` gets the same
`STIRRUP_RESULT` line by setting `resultSink.type: "stdout-json"` in
the `task_assignment`'s `RunConfig` (proto `ResultSinkConfig`); `job`
emits it through the configured sink once each run ends. `job` writes
the text summary to stderr for every run that produced a trace, so the
flag's suppression of that summary (`json` and `none`) has no `job`
counterpart. Under `job`, `resultSink.type` alone controls the
`STIRRUP_RESULT` line: `"none"` or unset omits it.

The exit code reflects the run outcome regardless of `--output`: a
failed or cancelled run still exits non-zero. A cancellation
mid-flight emits a partial `RunResult` carrying the cancellation
outcome rather than nothing, so callers parsing the JSON line always
see a structured record.

`RunResult.traceReference` identifies the run's OTel root span:
`traceId` (32 lowercase hex characters), `runSpanId` (16 lowercase hex
characters) and `sampled` (the trace flags' sampled bit). It is set
when `traceEmitter.type` is `otel` and omitted for the `jsonl` and
`gcs` emitters, which record no OTel span. The same value reaches a
gRPC control plane on the `run_started` event and the `done` summary;
see [`integration-guide.md`](integration-guide.md#events-from-the-harness).
Under `--transport stdio`, the CLI default, the `run_started` event is
also written to stdout as a JSON line, after the run's `ready` line:

```json
{"type":"run_started","id":"run-123","traceReference":{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","runSpanId":"00f067aa0ba902b7","sampled":true}}
```

Pair `--output=json` with a trace emitter that does not target
stdout. The default JSONL trace writes to a file (or to nothing when
`--trace` is unset), so the stdout channel stays reserved for the
`STIRRUP_RESULT` line. A future JSONL emitter that writes to stdout
would conflict with `--output=json`.

`RunResult.FinalAssistantText` carries the loop's last assistant
text onto the `STIRRUP_RESULT` line and any configured `resultSink`.
An unbounded value here risks exceeding the per-entry size limit most
log transports enforce (Cloud Logging's is roughly 256 KiB); a
truncated log entry corrupts the trailing JSON and defeats a
`grep | tail -n1`-style extraction of the line.

`resultSink.maxFinalAssistantTextBytes` bounds the field. Zero or
unset uses the default of 128 KiB — sized to leave headroom under a
256 KiB entry ceiling once the rest of the `RunResult` envelope and
JSON string escaping are accounted for. A positive value overrides
the default; a negative value fails validation. The bound applies
regardless of `resultSink.type`, including `none`: it protects the
`RunResult` value itself, so an embedder reading `RunResult` directly
gets the same guarantee as the `stdout-json` sink.

Truncation is explicit, not silent. When the field is cut, the
harness appends the marker `... [truncated by harness]` and sets
`RunResult.FinalAssistantTextTruncated` to `true`. Truncation never
splits a multi-byte UTF-8 rune, so the emitted JSON stays well-formed.
The cap and the marker apply only to the `RunResult` copy of the
text — the full, untruncated text is still recorded on the trace
(JSONL / GCS / OTel) independently of `resultSink`.

`resultSink.maxFinalAssistantTextBytes` bounds the raw string, not
its JSON-encoded size. JSON escaping can inflate a byte: the `<`,
`>`, and `&` case is closed by the `stdout-json` sink disabling
`json.Encoder`'s HTML escaping, but Go's encoder unconditionally
escapes U+2028 and U+2029 to a 6-byte `\uXXXX` sequence regardless of
that setting, so a payload dense in those runes can still encode past
the intended ceiling. The `stdout-json` sink closes the remaining gap
itself: it bounds the fully-encoded `STIRRUP_RESULT` line (sentinel,
JSON, and newline together) to 250 KiB — below, not at, Cloud
Logging's ~256 KiB per-entry ceiling, since that ceiling wraps the
whole log entry (timestamp, `insertId`, resource labels, and other
fields the logging agent adds around this payload) and Google
documents it as approximate — truncating `VerifierVerdict.Feedback`
and then `FinalAssistantText` and re-encoding until the line fits,
setting `FinalAssistantTextTruncated` to `true` when the latter is
touched. This second cap is internal to the sink and not
operator-configurable; it is a backstop against escaping inflation,
not a replacement for `maxFinalAssistantTextBytes`, and it only
protects the emitted line — an embedder reading `RunResult` directly
still relies on `maxFinalAssistantTextBytes` alone.

The bound is best-effort, not absolute: if the rest of the
`RunResult` envelope — the fields other than `FinalAssistantText` and
`VerifierVerdict.Feedback` — alone exceeds 250 KiB, there is nothing
left for the sink to truncate, and it emits the over-cap line rather
than dropping the result entirely. That case logs a warning naming
the encoded size and the cap so the condition is visible rather than
silently exceeding the intended bound.

### Workspace export

At end-of-run the executor's workspace can be tarred, gzipped, and
uploaded to a GCS URI — the result-collection surface for serverless
targets with no persistent filesystem (see
[`cloud-run-jobs.md`](cloud-run-jobs.md#shape-b-workspace-tarball-from-gcs)
for the operator walkthrough).

| Flag | Default | Notes |
|---|---|---|
| `--export-workspace-to` | (none) | Destination URI for the workspace tarball (e.g. `gs://bucket/runs/<runId>/workspace.tar.gz`). Only `gs://` is supported in v1. Overrides `executor.workspaceExportTo` from `--config` when explicitly set; an explicit empty value clears the field. JSON path: `executor.workspaceExportTo`. |
| `--export-workspace-required` | `true` when a destination is set | Whether a failed workspace export turns a successful run into outcome `handoff_failed` and a non-zero exit. `--export-workspace-required=false` records the failure in `RunResult.workspaceExport` without failing the run. Requires a destination. JSON path: `executor.workspaceExportRequired`. A gRPC control plane sets `executor.workspaceExportRequired` (proto `ExecutorConfig.workspace_export_required`) in the `task_assignment`'s `RunConfig`; `stirrup job` applies it to every run, and leaving it unset keeps the destination default. |

The export runs even when the run itself failed, so the workspace
state stays inspectable after a non-zero exit. Uploads authenticate
via the `gcp-workload-identity` credential source — the GCE/GKE
metadata server that Cloud Run, GKE Workload Identity, and plain GCE
VMs expose. There is no credential override for the exporter in v1.

With a follow-up grace window configured, every run exports once it
completes. The primary run uploads to the configured URI verbatim;
each follow-up run uploads to the same URI with its own run ID
inserted as the path segment before the object name
(`gs://bucket/runs/r1/workspace.tar.gz` →
`gs://bucket/runs/r1/<followUpRunId>/workspace.tar.gz`), so later
runs never overwrite the primary tarball. The same
`--export-workspace-required` policy applies to every run.

### Dry-run

The preflight flags. See [Dry-run preflight](#dry-run-preflight) for the
full workflow, the per-step report, and how the flags compose.

| Flag | Default | Notes |
|---|---|---|
| `--dry-run` | `false` | Run every initialisation step short of the first agentic turn, print a per-step preflight report, then exit. Spends no provider tokens. |
| `--no-probe-provider` | `false` | Skip the provider metadata probe. Meaningless without `--dry-run` (exit `4`). |
| `--no-probe-mcp` | `false` | Skip the MCP server handshake probe. Meaningless without `--dry-run` (exit `4`). |
| `--no-probe-trace` | `false` | Skip the trace-emitter reachability probe. Meaningless without `--dry-run` (exit `4`). |
| `--no-probe-egress` | `false` | Skip the egress-allowlist DNS probe. Meaningless without `--dry-run` (exit `4`). |
| `--no-probe-executor` | `false` | Skip the container-engine probe (container executor only). Meaningless without `--dry-run` (exit `4`). |
| `--dry-run-timeout` | `30s` | Total wall-clock budget for the preflight. Meaningless without `--dry-run` (exit `4`). |

`--output=json` (above) emits the `PreflightReport` to stdout when paired
with `--dry-run`; otherwise the report goes to stderr.

### Exit codes

The CLI distinguishes failure classes through the process exit code so
a wrapper script can branch on *why* a command failed without parsing
stderr. The scheme is uniform across `harness`, `job`, `run-config`,
and `healthcheck`:

| Code | Class | Examples |
|---|---|---|
| `0` | Success | The command completed; for `harness`, a run reached a terminal outcome; for `healthcheck`, the marker file is present and readable. |
| `1` | Validation / precondition | `ValidateRunConfig` (or `run-config --validate`) rejected the resolved config; a required prompt had no source; `job` ran without `CONTROL_PLANE_ADDR`; `healthcheck` found no marker file. Also the default for any failure not in a more specific class. |
| `2` | Parse error | The JSON in a `--config` file or piped stdin failed to decode (syntax error, unknown field, type mismatch). |
| `3` | I/O error | A `--config` or `--prompt-file` path could not be opened, read, or stat'd; an empty / oversize input; an `--output-runconfig` write or close failure; `healthcheck` found a marker file it could not open (e.g. a permission error). |
| `4` | Usage error | An invalid flag combination — currently a `--dry-run` probe gate (`--no-probe-provider`/`--no-probe-mcp`/`--no-probe-trace`/`--no-probe-egress`/`--no-probe-executor`) or `--dry-run-timeout` supplied without `--dry-run`. See [Dry-run preflight](#dry-run-preflight). |

A failed `--dry-run` (one or more probes reported `fail`) exits `1` on
the default path, not `4`: code `4` is reserved for the command-line
combination itself being incoherent, not for a probe finding a real
misconfiguration.

A failed or cancelled *run* (as opposed to a configuration failure)
exits non-zero on the same `1` default path; the `RunResult` on stdout
carries the run's outcome for callers that need finer detail than the
exit code. The interactive first-contact hints (a bare `stirrup` or a
bare `stirrup harness` on a terminal) are a success surface and exit
`0`.

Process exit code uses a binary success/non-success split on
`RunTrace.Outcome` ("success" is the only success value; every other
documented outcome — hard errors, exhausted resource limits, or an
interruption — exits non-zero) deliberately distinct from the eval
suite's passed/failed/inconclusive taxonomy
([`docs/eval.md`](eval.md)). Eval outcome answers "was the change any
good", folding limit-hit and cancelled runs into a separate
inconclusive bucket so aggregates don't punish a run that never got a
fair shot. Exit code answers a narrower question — "did this
invocation produce a usable result" — where inconclusive and failed
both mean no. This also matches the failure-signalling contract Cloud
Run and K8s Jobs need: an orchestrator should retry or alert on any run
that was not a genuine success.

## Component-selection limits

The CLI deliberately exposes only a subset of each component's
configuration space — the common cases. Anything below requires
`--config`:

| Component | What needs `--config` |
|---|---|
| `editStrategy` | `composite` (chains other strategies). |
| `verifier` | `composite` (chains other verifiers). |
| `permissionPolicy` | `policy-engine` requires `policyFile`; the optional `fallback` field (`allow-all`, `deny-all`, `deny-side-effects`, `ask-upstream`) defaults to `deny-side-effects` when unset. Chained policy engines are rejected. |
| `codeScanner` | `composite` requires `codeScanner.scanners` (each entry from the non-composite set). |
| `guardRail` | `composite` requires `guardRail.stages`. Per-phase restriction (`phases`), bespoke criteria, and the classifier timeout are file-only. An empty `phases` runs the guard at all three phases, except that `shieldstral` then checks only `pre_turn` and `post_turn`; `pre_tool` is opt-in (see [`guardrails.md`](guardrails.md#shieldstral)). |
| `traceEmitter` | `bucket` / `objectPrefix` / `credential` (the `gcs` emitter's routing — selectable via `--trace-emitter gcs` but configurable only by file). |
| `provider` | Multi-provider routing via `providers{}` plus a `modelRouter` of type `dynamic` or `per-mode`. |
| `tools.mcpServers` | Remote MCP server registration. |
| `tools.controlPlane` | Control-plane-fulfilled tools (gRPC transport only). See [Control-plane tools](#control-plane-tools). |
| `tools.commandOutput` | Full-stream command capture limits and model preview thresholds. |
| `tools.maxResultBytes` | Upper bound on the model-visible text of one tool result: default 100 KiB, accepted range 1 KiB to 1 MiB. Longer results are cut to their head with a marker naming the original size. `run_command` and `read_command_output` bound their own output and are exempt. |
| `traceEmitter.archive` | Explicit local or GCS destination for compressed command-output sidecars. |

The CLI flags for each of these set the *type* selection only.

## Tool permission flags

Tools carry two independent permission flags:

- `WorkspaceMutating` means the tool changes workspace state: files,
  processes, or other on-disk artefacts. Read-only modes reject these
  tools structurally before the loop starts.
- `RequiresApproval` means the operator may want an upstream approval
  prompt before the tool runs. It includes mutating tools, but also
  covers non-mutating sensitive tools such as `web_fetch` and
  `spawn_agent`.

`RequiresApproval` does not by itself mean "always prompt". The active
`permissionPolicy` decides how to interpret the flag:

| Tool examples | Flags | `allow-all` | `deny-all` | `deny-side-effects` | `ask-upstream` |
|---|---|---|---|---|---|
| `read_file`, `list_directory`, `grep_files`, `find_files`, `git_status`, `git_changed_files`, `git_diff`, `git_show` | neither flag | Allow | Deny | Allow | Allow |
| `web_fetch`, `spawn_agent` | `RequiresApproval` only | Allow | Deny | Allow | Prompt |
| `run_command` | `WorkspaceMutating` + `RequiresApproval` | Allow | Deny | Deny | Prompt |
| `edit_file`, `write_file` | `WorkspaceMutating` + `RequiresApproval` | Allow | Deny | Deny | Prompt |
| `tools.controlPlane[]` entries | neither flag, or `RequiresApproval` when `requiresApproval: true` | Allow | Deny | Allow | Allow, or Prompt with `requiresApproval` |
| `tool_search` ([lazy tool loading](#lazy-tool-loading)) | neither flag; the policy is never consulted | Allow | Allow | Allow | Allow |

`policy-engine` evaluates the Cedar policy first. When Cedar returns
no decision, the configured fallback (`deny-side-effects` by default)
applies exactly the same rules as the corresponding non-Cedar policy.
For example, a policy engine with `fallback: "deny-side-effects"`
still allows `web_fetch` and `spawn_agent` unless a Cedar `forbid`
matches them, because neither tool mutates the workspace. Pair a
permit-based allow-list policy file with `fallback: "deny-all"` when
everything the file does not grant must be denied — including
non-mutating tools like `web_fetch`.

A `policy-engine` policy file is linted before the run starts, and a
finding aborts construction rather than warning: a clause keyed on an
attribute the harness never populates (`context.inputs`,
`principal.runID`, `context.input.cmd` where `run_command` declares
`command`) parses cleanly and silently authorises nothing. The rule
table, the `@stirrupLintIgnore` escape hatch, and the two-tier split —
structural rules run under `--dry-run`, tool-schema rules run at
real-run construction — are documented in
[`safety-rings.md`](safety-rings.md#load-time-policy-lint).

Choose `ask-upstream` when every `RequiresApproval` tool must prompt.
Choose `deny-side-effects` when the goal is to block workspace
mutation while still allowing non-mutating tools that may have network
or budget exposure. `deny-all` is rarely useful as the top-level
policy (it denies read tools too); its role is the allow-list
fallback described above.

## Read-only modes

`planning`, `review`, `research`, and `toil` enforce a structural
invariant via `ValidateRunConfig`: the tool list must exclude
`write_file`, `run_command`, and `edit_file`, and the permission
policy must not be `allow-all` — nor may a `policy-engine`'s
`fallback` be `allow-all`, which would be equivalent whenever no
policy matches. The validator rejects any `RunConfig` that violates
this before any component is constructed.

A read-only mode that names no `permissionPolicy.type` is resolved to
`deny-side-effects` during validation, so every entrypoint — CLI, gRPC,
and direct embedding — lands on the same policy. An editable mode is
not defaulted: validation rejects an empty `permissionPolicy.type`
rather than inferring `allow-all`, which the CLI applies only because
`--mode execution` is itself an explicit choice.

`planning` is the CLI default. A bare `stirrup harness --prompt "..."`
invocation therefore lands in a read-only posture with no write or
shell capability and the `deny-side-effects` permission policy.
Operators wanting the editable, shell-capable behaviour opt in
explicitly with `--mode execution` (or by selecting one of the
restrictive `permissionPolicy` types in [`safety-rings.md`](safety-rings.md)
for finer-grained control). Because read-only enforcement is based on
`WorkspaceMutating`, a read-only mode can still include non-mutating
sensitive tools such as `web_fetch`; under the default
`deny-side-effects` policy those tools run without prompting. Switch
to `ask-upstream` when those calls should require operator approval.

The read-only modes differ from each other only in prompt template:
`planning` for "describe and reason before acting" first-touch use,
`review` for change-review tasks, `research` for investigation across
a codebase or the web, and `toil` for structured-briefing workflows.

## Control-plane tools

`tools.controlPlane` declares tools whose result the control plane
supplies over the gRPC stream instead of the harness computing it.
Each entry becomes an async tool: a call validates the model's input
against `inputSchema`, emits a `tool_result_request` event, and
blocks on the matching `tool_result_response`. The block is
file-configurable and mirrored on the proto (`ToolsConfig.control_plane`),
so a control plane can send it in `task_assignment`; there is no CLI
flag because the CLI topology has no control plane to answer.

| Field | Required | Semantics |
|---|---|---|
| `name` | yes | Tool name as the model sees it. `^[A-Za-z_][A-Za-z0-9_-]{0,63}$`; must not collide with a built-in tool name, carry the `mcp_` prefix, or repeat within the list. |
| `description` | yes | Tool description shown to the model. |
| `inputSchema` | yes | JSON Schema object with `"type": "object"`. Input is validated against it before any request is emitted. |
| `timeoutSeconds` | no | Wait for the response, 0–3600. `0` selects the harness default of 60 s. Expiry is a tool failure the model sees; the run continues. |
| `requiresApproval` | no | When `true`, each call goes through the permission policy first (see the table above). |

Validation rejects the block unless `transport.type` is `grpc`, the
same rule as `executor.sandboxIdentity`. Control-plane tools are
never `WorkspaceMutating`, so the read-only modes admit them next to
an explicit `tools.builtIn` list. Registration order is built-ins,
then control-plane tools, then MCP-imported tools. A toolset profile
aliases only built-in IDs; a control-plane tool named after an alias
target (`grep`, `find`, `bash` under `coding-classic`) is renamed by
the presenter's collision resolution, so avoid those names when a
profile is set. The control-plane side of the contract — response shape,
`is_error`, cancellation — is in
[`integration-guide.md`](integration-guide.md#control-plane-fulfilled-tools-tool_result_request).

## System prompt templating

The shipped per-mode system prompts are Go
[`text/template`](https://pkg.go.dev/text/template) documents rendered
per run against a *prompt model* — a model identity that selects which
guidance the prompt carries. Frontier-generation models degrade under
prescriptive process scaffolding and receive lean behavioural
additions; open-weight models receive exactly that scaffolding. A
model matching neither set renders the unconditional base prompt, so
an unrecognised or brand-new model always gets a functional prompt
(fall-through is structural, not configured).

### Prompt model resolution

The prompt model resolves in order:

1. `promptBuilder.promptModel` (`--prompt-model`) when set — the
   explicit override for prompt/model comparison runs;
2. otherwise the model the router serves for this run's mode: the
   `modeModels[mode]` entry for a per-mode router, else
   `modelRouter.model`;
3. otherwise the harness default model.

Dynamic routers may switch models between turns, but the prompt is
rendered once per run against the router's default model: re-rendering
per turn would invalidate provider prompt caches and change agent
guidance mid-run.

The resolved prompt model and its tier are recorded on the run's root
OTel span as `prompt.model` and `prompt.tier` (always on — these are
config metadata, not content), so comparison runs remain attributable
from traces alone.

### Tiers

Tier membership is a pair of glob tables in
`harness/internal/prompt/modeltier.go` — adding a new model to a tier
is a one-line change there. Globs use `path.Match` with the same
semantics as the provider-quirks registry (`*` does not cross `/`;
gateway-prefixed IDs are matched by `*/`-prefixed variants).

| Tier | Members (initial) | Guidance shape |
|---|---|---|
| `frontier` | `claude-fable-5*`, `claude-mythos-5*`, `claude-sonnet-5*`, `claude-opus-4-8*`, `claude-opus-5*`, `gpt-5.5*`, `gpt-5.6*`, `gpt-6*` | Lean behavioural additions: act when ready, treat the run as unattended, scope discipline, stop once checks pass, evidence-grounded progress claims, outcome-first summaries. |
| `open-weight` | `gemma*`, `glm-*`, `deepseek*`, `qwen*` | Explicit process scaffolding: read/edit/test loop, invoke-don't-describe, restated output formats and stopping conditions. |
| `default` | everything else | Base prompt only — byte-identical to the pre-templating prompts. |

Inspect any rendering offline (no provider call, no credentials):

```sh
stirrup prompt render --mode execution --prompt-model claude-fable-5
stirrup prompt render --mode execution --prompt-model gemma-4 --template ./tuned.tmpl
```

The rendered preamble prints to stdout; the resolved tier prints to
stderr. Structural sections the harness appends at run time (working
directory, turn budget, workspace tree, git status, dynamic context)
are omitted because they depend on a live workspace.

### Operator templates and the override

Three prompt-content surfaces exist, in precedence order:

1. **`systemPromptOverride`** — a raw string used verbatim as the
   preamble. Never template-parsed, so content containing `{{` (for
   example a prompt compiled by an external prompt-management system)
   passes through untouched.
2. **`promptBuilder.template`** — an operator-supplied Go
   text/template replacing the shipped mode prompt. It renders against
   the same data surface as the shipped prompts, so model-conditional
   content and `--prompt-model` comparison keep working for tuned
   prompts. Syntax is checked by `ValidateRunConfig`; the template is
   additionally trial-rendered at component construction so execution
   errors fail before the run starts.
3. **Shipped mode templates** — the default.

In both 1 and 2 the structural sections (workspace path, turn budget,
workspace tree, git status, dynamic context with `untrusted_context`
wrapping) are still appended.

Combining `systemPromptOverride` with either `promptBuilder.template`
or `promptBuilder.promptModel` is rejected by validation rather than
resolved by precedence: the override bypasses template rendering, and
in an eval sweep the silent no-op would masquerade as a valid
prompt/model comparison.

Templates render against this data surface (methods are called
without arguments):

| Expression | Value |
|---|---|
| `.Model` | The resolved prompt model string. |
| `.Mode` | The run mode. |
| `.Tier` | `"frontier"`, `"open-weight"`, or `"default"`. |
| `.ModelIs "glob" ...` | `true` when the prompt model matches any `path.Match` glob (a malformed glob is a non-match). |

Template rendering is pure string work: the data surface exposes no
filesystem, environment, or network reach.

### Control-plane prompt management (Langfuse)

An upstream control plane that manages prompts in an external system
such as [Langfuse prompt management](https://langfuse.com/docs/prompt-management/get-started)
has two integration paths:

- **Compiled prompt → `systemPromptOverride`.** Fetch the prompt by
  name/label, compile its `{{variable}}` placeholders with the
  Langfuse SDK, and ship the resulting string. Per-model tuning
  happens control-plane-side (e.g. one Langfuse label per model
  family). Because the override is never template-parsed, leftover
  mustache syntax cannot be misinterpreted — but an *uncompiled*
  prompt pasted into `promptBuilder.template` fails validation
  loudly, which is the intended guard against confusing the two
  surfaces.
- **Go template → `promptBuilder.template`.** Store a Go
  text/template in the external system and ship it verbatim. The
  harness renders it per run with the model-conditional surface
  above, so one stored prompt can serve every model tier and
  `--prompt-model` comparisons still work.

Stirrup's OTel emitter stays vendor-neutral either way: prompt
name/version linkage attributes specific to Langfuse are not emitted.
Use `prompt.model` / `prompt.tier` (and `gen_ai.system_instructions`
under `--otel-capture-content`) to correlate runs with prompt
versions.

## Rule-of-Two configuration

The `ruleOfTwo` block tunes [Ring 4](safety-rings.md#ring-4--rule-of-two-pre-flight-invariant--runtime-classifier).
It has no CLI flag — the override must be stored in the RunConfig so it is
reviewable in pull requests. The behavioural semantics (what trips the
latch, the arming matrix, the on-detect actions) are documented in
[`safety-rings.md`](safety-rings.md#the-runtime-classifier); this table is the
schema reference.

| Field | Type | Default | Notes |
|---|---|---|---|
| `ruleOfTwo.enforce` | `bool` (pointer) | `true` (unset = enforce) | `false` overrides the pre-flight all-three rejection *and* downgrades the runtime classifier to observe-only. Detection events still fire; a `rule_of_two_disabled` event is emitted at run start. |
| `ruleOfTwo.runtime.classifier` | enum | `""` | `""` lets the factory decide from the static Rule-of-Two state; `"patterns"` forces the deterministic detector on; `"none"` disables runtime detection entirely. |
| `ruleOfTwo.runtime.onDetect` | enum | `""` (= `block-external`) | Action once the latch trips: `block-external` (revoke external-comm tools), `ask-upstream`, `redact`, `abort`, `warn`. |
| `ruleOfTwo.runtime.guardCriteria` | `[]string` | `["sensitive_data", "pii"]` | LLM-guard `Decision.Criterion` values that also trip the latch (one-way). Each entry must match the same snake_case constraint as `guardRail.customCriteria` keys. |

`ValidateRunConfig` enforces the closed sets and three cross-field
rules:

- `onDetect: "ask-upstream"` requires `transport.type: "grpc"` — `stdio`
  has no upstream control plane to answer an approval request.
- `onDetect` must be empty when `classifier: "none"`: the detector is
  disabled, so the action could never fire, and the combination is
  rejected rather than stored as dead configuration.
- `onDetect: "abort"` is rejected when the run is already statically
  sensitive (`sensitiveData: true` or a `sensitive` dynamic-context
  entry): the latch is pre-tripped, so no runtime transition can fire
  and the abort would never trigger.

Validation never injects a `runtime` block, so the `Redact()`-persisted
config reflects exactly what the operator declared; default arming is
factory behaviour, not config mutation.

## Toolset profiles

`tools.profile` selects the *model-facing presentation* of the tool
set — the names and descriptions a model sees on the wire — without
changing the tools the harness dispatches to. It is a closed enum:

| Value | Presentation |
|---|---|
| `""` / `default` | Identity. Tools present under their internal names (`grep_files`, `find_files`, `run_command`, `edit_file`, …). This is the zero value, so a config that omits the field behaves exactly as before profiles existed. |
| `coding-classic` | Presents the terse coding-CLI aliases some models call by reflex: `grep_files` → `grep`, `find_files` → `find`, `run_command` → `bash`. Tools not in the alias table (including `read_file`, `edit_file`, and every MCP tool) present unchanged. |

An alias changes only the name. The internal dispatch identity is
untouched: a model that calls `grep` reaches the same `grep_files`
handler, the permission policy still gates it as `grep_files`, and the
security guard still keys on `grep_files`. Aliasing therefore cannot
broaden capability — it cannot surface a tool the registry did not
register, and an alias for a tool a read-only mode excluded does not
exist because the excluded tool is never registered to alias.

Every gating and guard surface keys on the internal tool ID, never the
alias: the permission policy, the workspace-mutation guard, the
guardrail's `PhasePreTool` classifier input, and the sub-agent
recursion filter all see `grep_files`, not `grep`. **Cedar policies and
permission configs must reference internal tool IDs** (`run_command`,
`grep_files`, `edit_file`, …), not profile aliases. A Cedar rule that
forbids `"bash"` under the `coding-classic` profile matches nothing —
the policy engine is never shown the alias. Write the rule against
`run_command`.

Existing configs keep working. Because the default profile is the
identity presentation, a config that names tools by their internal IDs
in `tools.builtIn` (or a model that calls them by those IDs) continues
to resolve under any profile — the internal name is always accepted in
addition to the alias.

Alias collisions — two tools whose profile aliases land on the same
string — are resolved by the same deterministic normalization the
provider function-name layer uses (see
[`provider-quirks.md`](provider-quirks.md) and the `toolname` package):
one keeps the bare alias, the other gains a short stable hash suffix, so
the binding never silently routes a call to the wrong handler.

Traces record both names. Each tool-call trace and record carries the
model-facing `name` and, when an alias was resolved, the internal
`internalName`; under the default profile the two coincide and
`internalName` is omitted, keeping the trace wire shape unchanged. An
absent `internalName` is therefore ambiguous in isolation — it means
either "called by internal name under the default profile" or "the name
did not resolve to a known tool under a non-default profile". The active
`tools.profile` is recorded in the trace's attached `RunConfig`, so the
two cases are distinguishable by reading it alongside the record.

## Lazy tool loading

`tools.lazyLoading` keeps the tool definitions each turn carries small.
Tools outside an *eager* set are *deferred*: they stay registered and
permission-gated, and are dispatchable once loaded, but their
definitions reach the model only once a tool search loads them. A run
with dozens of MCP-imported or control-plane tools stops paying for
every definition on every turn, and the model chooses among fewer tools
at once. Lazy loading is off by default; a run that does not set
`enabled` sends every registered tool on every turn.

Lazy loading pays off for runs with ten or more deferrable tools, or
more than about 10k tokens of tool definitions. It has costs: the model
cannot call a deferred tool before a search has loaded it, and the
harness strategy spends an extra round trip on every search.

```json
{
  "tools": {
    "mcpServers": [{"name": "github", "uri": "https://mcp.example.com/github"}],
    "lazyLoading": {
      "enabled": true,
      "strategy": "auto",
      "maxResults": 5
    }
  }
}
```

| Field | Default | Semantics |
|---|---|---|
| `enabled` | `false` | Turns lazy loading on. |
| `strategy` | `auto` | `auto` uses the provider's native tool search where the routed model has one, and the harness `tool_search` tool otherwise. `harness` always uses `tool_search`. |
| `eager` | every built-in tool | Internal tool IDs sent on every turn. Empty selects every registered tool that is neither MCP-imported (`mcp_` prefix) nor a [control-plane tool](#control-plane-tools), so only those two groups are deferred. A non-empty list makes exactly those tools eager and defers all remaining registered tools, built-ins included. At most 256 entries; `tool_search` may not be listed because it is always eager. |
| `maxResults` | `5` | How many tools one harness `tool_search` loads, 1–20. The `tool_search` schema advertises this value as both its default and its maximum. A native search applies the provider's own limit. |

The eager list names internal tool IDs (`run_command`, not the
`coding-classic` alias `bash`). An MCP tool's internal ID is
`mcp_<server>_<tool>`, where `<tool>` is the server's tool name cut to
128 characters. Each entry must be non-empty, at most 256 bytes, and
free of whitespace and control characters; `*`, `?` and `[` are
rejected because they are reserved for a future pattern form. The list
interacts with `tools.builtIn` only through registration: a tool
`tools.builtIn` leaves out is never registered, so it is neither eager
nor deferred. MCP tool names are known only after discovery, so an
`eager` entry that matches no registered tool is not a validation error. The run starts, logs it, and sends a `warning` event naming it after
`run_started`. Like the info line that reports the strategy, the eager
count and the deferred count by group (built-in, MCP, control-plane),
the warning is sent once per session, not again for follow-up runs or
sub-agents.

`tools.mcpServers` and `tools.controlPlane` have no CLI flags, so a
flag-only run with the default eager list has nothing to defer. Lazy
loading over MCP tools takes the servers from a `--config` file:

```sh
stirrup harness --config run.json --lazy-tools
```

A non-empty eager list defers built-ins too, which works without a
config file:

```sh
stirrup harness --lazy-tools --lazy-tools-eager read_file --lazy-tools-eager grep_files
```

`--lazy-tools=false` turns off an `enabled: true` from `--config`.

`stirrup harness --dry-run` reports the resolved split before any run
starts: the eager and deferred counts per group, the tool search each
routed provider and model uses, and every `eager` entry that matches no
tool. See [Dry-run preflight](#dry-run-preflight). An evaluation suite
sets the same fields in a `tools { lazy_loading { ... } }` block of its
inline `run_config`.

### Visibility, not authority

Deferral changes what the model sees, not what it may call. Any
deferred tool can be loaded by a search, and a native search sends
every deferred definition to the provider on every turn. To withhold a
tool, leave it out of `tools.builtIn` or an MCP server's
`allowedTools`, or deny it in the permission policy. Search ranking is
keyword scoring and is not a trust signal: a tool's position in a
result says nothing about where it came from. The harness search caps
how many results one group can take, which limits a server that fills
its tool names and descriptions with keywords so that another server's
tool never loads, but the cap bounds that squatting and does not
prevent it.

### Strategies and providers

Under `auto`, the harness consults the quirks registry for the routed
provider, its [endpoint profile](providers.md#endpoint-profiles), and
the model on every turn, so a router that switches models mid-run
switches search paths with it:

| Provider and model | `auto` uses |
|---|---|
| `anthropic`: Claude Opus 4.5–4.8, Sonnet 4.5–4.6, Haiku 4.5 and the Claude 5 generation | Native search: the Anthropic BM25 tool search tool, with deferred tools sent with `defer_loading` |
| `openai-responses`: `gpt-5.4` and later 5.x, the `gpt-6` family | Native search: the hosted `tool_search` tool, with deferred tools sent with `defer_loading` and each MCP server's and the control plane's deferred tools grouped into a namespace; deferred built-ins keep their names and descriptions visible (see below). Only on the `openai` [endpoint profile](providers.md#endpoint-profiles), which an empty `provider.endpointProfile` derives for OpenAI's own API. A custom `baseUrl` derives `generic` and uses the harness `tool_search`, because a gateway can accept the hosted search entry and silently drop it, leaving every deferred tool unloadable (observed with LM Studio; other gateways are unprobed). `azure-openai` and `bedrock-mantle` are accepted but resolve like `generic` until probed, so they also use the harness `tool_search`. A relay in front of OpenAI's API declares `provider.endpointProfile: "openai"` to keep the hosted search. When the profile is derived as `generic` for a model that would have the hosted search, the run logs a notice at startup; see [Hosted tool search](providers.md#hosted-tool-search) |
| `bedrock` | The harness `tool_search` tool: the adapter uses the Converse API, and Anthropic documents server-side tool search on Bedrock only for InvokeModel |
| Every other provider and model (Gemini, `openai-compatible`, older Claude and GPT models) | The harness `tool_search` tool |

With a native search, every registered tool except the harness
`tool_search` is sent on every turn and the deferred ones are flagged
for deferred loading, so the request stays the same from turn to turn
until the history is compacted (see
[Lifetime of the loaded set](#lifetime-of-the-loaded-set)); the
provider searches and expands the definitions of the tools it finds.
A native search gives the model no inventory of the deferred
tools, so a system-prompt hint naming the tool categories helps it
search for the right words. A native search also needs at least one
eager tool besides `tool_search`, because a provider rejects a request
whose every tool is deferred; without one, `auto` falls back to the
harness search.

With the harness search, a turn sends the eager tools, the tools loaded
so far, and `tool_search` itself, whose description names the deferred
groups (each MCP server, control-plane tools, and built-ins) and their
sizes without listing the tools. `tool_search` matches keywords, not
meaning: the query is lowercased and split on every character that is
not a letter or digit into at most 32 distinct terms. Each term
scores the strongest field it appears in as a substring: 3 for the tool
name, 2 for the description, 1 for a top-level parameter name or
description. It is neither BM25 nor semantic search. The best-scoring
tools not yet loaded are loaded, and the next turn sends their full
definitions. Tools that match but are already loaded are not loaded
again and do not count toward the limit; the result names them on an
`Already loaded:` line. Anthropic rejects a signed thinking block
replayed under a tool list other than the one the block was produced
under, so the turn after a load drops the thinking blocks of all
earlier turns from the history.

The harness search also keeps one group from filling every slot. A
group is an MCP server, the MCP tools no configured server claims,
the control-plane tools, or the built-ins. While other groups have
matches, one group takes at most half the limit, rounded up: 1 of 1,
1 of 2, 3 of 5. Slots a cap leaves free are then filled with the
skipped matches in score order, so a group that alone matches is not
held back. A tool whose name, internal ID, or (for an MCP server's
tool) the server's own name for it equals the whole query, ignoring
case and surrounding whitespace, is loaded ahead of every scored match.
Such a tool counts toward its group's cap, so a group takes the larger
of the cap and its number of exact matches. A tool belongs to the MCP
server that registered it, whatever the tool is named. Each result line
names the group after the tool name, as `(MCP server "github")`,
`(MCP)`, `(control plane)`, or `(built-in)`; the server name comes from
the `tools.mcpServers` configuration and not from the server. Control
characters in a server-supplied tool name or description are dropped
from result lines, so neither can start a line of its own.

The harness search keeps every deferred tool's name and description out
of the request until it is loaded, on every provider. The OpenAI hosted
search hides a deferred tool only inside a namespace. The harness groups
each configured MCP server's deferred tools into a namespace named
`mcp_<server>`, and the deferred control-plane tools into one named
`control_plane`, so the model sees only each namespace's name and a
generated description until it loads the namespace. Deferred
built-in tools, and MCP tools whose name no configured server claims,
are sent as flat functions whose names and descriptions stay visible;
only their parameter schemas are deferred. See
[Hosted tool search](providers.md#hosted-tool-search).

Under the harness strategy, each search that loads a tool changes the
tool list, so the next request misses the provider's prompt cache. A
native search keeps the request stable and the cache warm.

Both native searches are also sent on batch turns. An OpenAI batch turn
never captures the provider's output items, so a hosted search made in
a batch turn is always replayed by best-effort reconstruction. Both
batch paths were probed against the live API on 2026-10-26: an Anthropic
batch (`claude-haiku-4-5`) completed in about 98 s and an OpenAI batch
(`gpt-5.4-mini`) in 338 s, each
returning the same shapes as a streamed turn.

### Nothing to defer

When every registered tool is eager (for example a run with no MCP
servers or control-plane tools and an empty `eager` list), lazy loading
changes nothing: the harness sends the plain tool list, without
`tool_search`, and asks for no native search. The run-start info line
says so.

### Calling an unloaded tool

A call to a deferred tool that no search has loaded is rejected before
the permission check and the `pre_tool` guardrail, with text naming the
search the turn offered; `<name>` is the tool name the model used:

- harness search: `Tool "<name>" is not loaded; call tool_search to load it first`
- native search: `Tool "<name>" is not loaded; search for it with <search tool> first`,
  where `<search tool>` is `tool_search_tool_bm25`,
  `tool_search_tool_regex`, or the hosted `tool_search`.

The rejection is recorded with the `tool_not_loaded` failure category,
whose metric keeps the tool's name instead of the `__unknown__` label an
unresolvable name gets, so a skipped search is distinguishable from an
invented tool. Repeated rejections count
toward the stall detector's limit of consecutive failed calls. A loaded
tool is dispatched exactly like an eager one.

A native search result is trusted only on a turn that offered the
native search. On any other turn its references are dropped and load
nothing, and on a native turn only names that resolve to registered
deferred tools are kept, at most 256 per result.

### Lifetime of the loaded set

The loaded set lasts for the run. A follow-up run keeps it when it
continues the previous run's history (`followUpHistory: "continue"`)
and starts empty otherwise. Tools load as soon as a search reports
them, before the turn commits, so a turn that is later abandoned still
leaves its tools loaded.

A provider's native search expands a loaded tool's definition only
through the search result that loaded it, so that result must be in
the request. It is missing when the context strategy dropped or
summarised the exchange that held it, when a router switch loaded the
tool through the harness `tool_search` or another provider's search,
or when output-limit recovery discarded a paused turn's continuation
after its search had loaded tools. A native-search request then sends
each such tool's full definition without the deferred flag or a
namespace, like an eager tool. This applies to that request only: the
loaded set does not change, and every later request is checked again.
The tool list changes whenever a tool's search result leaves or
re-enters the request, and each change costs one cache write of the
tools and system prefix. The check is per request because a run-long
un-defer would, on the Responses API, place a now-flat tool inside a
rebuilt `tool_search_output`, which is the shape the API rejects. When
every deferred tool is loaded and none has its search result in the
request, the request defers nothing and the adapter leaves the native
search off it, because there is nothing left to load. A continuation of
a paused turn that ends on an unanswered native search keeps its tools
deferred, so the search tool stays on the request.

### Sub-agents

A sub-agent starts with the eager tools of its own tool set and nothing
loaded, and its searches never change the parent's loaded set. A
read-only sub-agent drops every MCP tool, because MCP tools are
registered as workspace-mutating, but keeps control-plane tools, which
stay deferred; such a child still gets `tool_search`. A child with
nothing left to defer gets no `tool_search`.

### Permissions

`tool_search` is neither `WorkspaceMutating` nor `RequiresApproval`, so
the permission policy is never consulted for it, even under `deny-all`,
and read-only modes admit it: it changes only which definitions the
model sees. Every loaded tool keeps its own flags and is gated by the
permission policy when it is called, exactly as without lazy loading.
Because the policy is never consulted for `tool_search`, a Cedar rule
naming it has no effect. The pre-turn guardrail and the Rule-of-Two
sensitive-data monitor screen a harness search result like any other
tool result. That screening covers only the 200-byte first-line preview of each
loaded tool's description, not the full definition the next request
sends.

### What is recorded

| Where | What it shows |
|---|---|
| `tools_loaded` event (gRPC and stdio) | One event per search that loaded a tool not already loaded: `source` (`native` or `harness`), the search tool's `name`, its `input`, the `turn`, and the internal IDs in `tools`. See [`integration-guide.md`](integration-guide.md#watching-a-run-live). |
| `toolsLoaded` on a JSONL turn record, a sub-agent's included | The internal IDs the turn's searches loaded. |
| `tool_references` in a turn record's `modelOutput` | The internal IDs a native search returned. |
| The `tool_search` tool call's output | The tools a harness search loaded, by the names the model sees. |

The per-turn summary (`TurnTrace.ToolsLoaded`) carries the same list
but only in process; no durable emitter writes it.

In the JSONL trace, a turn record's `modelInput.tools` lists the
definitions the turn sent, and the two strategies look different
there. A native-search turn lists every deferred tool, loaded or not,
with `deferred: true`, and no `tool_search`; a loaded tool whose search
result is not in the request is listed without the flag (see
[Lifetime of the loaded set](#lifetime-of-the-loaded-set)). A native-search turn
also records each deferred MCP or control-plane tool's `namespace` and
`namespaceDescription`, with the unnormalised name, whichever provider
the turn went to. A harness-search turn
lists the eager tools, the tools loaded so far, and `tool_search`, and no
tool carries the flag. A native search appears in the turn's
`modelOutput` as a `tool_search` block (the search input) and a
`tool_search_result` block whose `tool_references` lists internal IDs.
A failed search sets `is_error`, with the provider's error code in
`name` and its message in `content`. The same blocks in a later turn's
`modelInput.messages` carry the names the model sees (a toolset
profile's aliases), because the harness presents them before each
request. A harness search is an ordinary `tool_search` tool call and
result. Search blocks are replayed only to the provider that produced
them; another provider drops them.

## Command output capture

`run_command` streams complete stdout and stderr into a run-scoped secure
store. Output up to `tools.commandOutput.inlineMaxBytes` remains inline after
secret scrubbing. Larger output returns scrubbed tails plus opaque
`stirrup://command-output/...` references; the read-only
`read_command_output` tool reads those references in 32 KiB pages
(128 KiB maximum per call).

`read_command_output` is `run_command`'s companion: it registers
automatically whenever `run_command` registers with capture enabled — a
spilled reference without its reader would be unusable, and `tools.builtIn`
allowlists written before capture existed keep working unchanged. Listing
`read_command_output` in `tools.builtIn` is only needed for standalone
replay runs; listing it while `enabled` is `false` is rejected as
contradictory at validation.

```json
{
  "tools": {
    "commandOutput": {
      "enabled": true,
      "failurePosture": "strict",
      "inlineMaxBytes": 32768,
      "previewBytesPerStream": 4096,
      "maxBytesPerStream": 52428800,
      "maxBytesPerRun": 524288000
    }
  },
  "traceEmitter": {
    "type": "jsonl",
    "filePath": "run.jsonl",
    "archive": {"type": "local", "filePath": "run.command-output.tar.gz"}
  }
}
```

Capture defaults to on; `"enabled": false` reverts `run_command` to the
legacy bounded-inline behaviour, registers no `read_command_output` tool,
and writes no archive — the lever for A/B comparison of the feature under
`stirrup-eval`.

The stream and run maxima are compliance boundaries, not truncation limits:
crossing one always cancels the offending command. What happens next is the
`failurePosture`. Under `"strict"` (the default) the store refuses further
captures and an otherwise-successful run reports
`command_output_capture_failed` (or `command_output_archive_failed` when the
sidecar itself cannot be written or uploaded); a run that already failed for
a primary reason keeps that outcome. Under `"bestEffort"` later commands
keep capturing, the run outcome is never overridden, and the failure stays
visible in the archive manifest and per-command trace records.

Output is redacted on the way to disk. Each stream passes through a chunked
scrubber before it reaches its run-scoped spool file (0600, under a 0700
temporary directory), and that file is the archive member — there is no
second, unredacted copy at any point, so an unclean shutdown (crash,
SIGKILL) leaves behind the same scrubbed bytes the archive would have
carried, subject to the residual below. Archives contain scrubbed streams
only, while raw byte counts and SHA-256 hashes, taken from an in-flight hash
of the stream as the command produces it, remain as integrity metadata.

The scrubber buffers up to 64 KiB plus a 16 KiB carry window, cutting each
chunk on a line boundary where one is available. The carry window is what
lets a secret split across two chunks still be redacted as one span. Every
pattern the scrubber knows matches a span shorter than the window from
realistic output: a fat JWT, and a PEM key block, which spans the whole
key (about 3.3 KiB for RSA-4096 and 6.4 KiB for RSA-8192), are pinned
under it by `TestSecretPatternsMatchSingleSpanUnderCarryWindow`. Three
consequences follow:

- **Residual.** A span longer than the carry window cannot be held for
  reassembly, so it is cut mid-match: its leading portion is redacted and
  the remainder reaches the spool. Anything that span swallowed shares its
  fate, including a shorter secret nested inside it. Reaching it takes
  something like an unterminated `Bearer` token running for tens of
  kilobytes, a key block larger than any RSA key in use, or an
  unterminated key header followed by more than 16 KiB of base64 lines.
- **Trailing output on a crash.** Up to one buffer of the most recent output
  is still in memory when a process is killed and is lost rather than
  spooled. A crashed run writes no archive, so the spool is forensic
  material only.
- **Storage-failure latency.** A capture still fails closed on a disk error,
  but the error surfaces at the first chunk flushed after it starts rather
  than at the first byte written, so a command can run for up to one buffer
  longer before its capture is cancelled.

`maxBytesPerStream` and `maxBytesPerRun` count raw bytes as the command
produces them. The file holds scrubbed bytes, and `[REDACTED]` can be longer
than the span it replaces, so a stream at its limit can occupy up to roughly
1.9× that many bytes on disk.

Creating a store also sweeps the OS temp directory for spool roots
(`stirrup-command-output-*`) where nothing has been written for 24 hours,
reclaiming what a crashed run left behind. The gate reads the whole tree,
not the root's own timestamp, because a directory's timestamp does not
advance while a capture appends to a file beneath it. Nothing caps a run's
wall clock, so the gate is a heuristic: a run that captures no command
output for a full day can have its spool reclaimed by a second stirrup
process starting on the same host. The sweep skips symlinks and stops after
4096 entries.

The `eval/suites/command-output-ab-{on,off}.hcl` pair measures the
pipeline's context-saving claim: identical tasks with capture forced to
spill versus disabled, compared via `stirrup-eval compare` (mean tokens at
equal-or-better pass rate). See [`eval/suites/README.md`](../eval/suites/README.md).
Without an explicit destination, JSONL and GCS derive an adjacent archive;
other emitters retain a local archive and report its absolute path in
`RunResult.commandOutputArchive`.

## Lifecycle hooks

`hooks` configures operator-authored, deterministic shell commands that
run around the agentic session (issue #461): `preRun` before the
session starts, `postRun` after it ends. A hook is exec, not a tool —
it runs through the same `Executor` the agent's tools use, and its
output is trace-only and never enters the model's context.

```json
"hooks": {
  "preRun":  [ { "type": "command", "name": "...", "command": "...", "timeoutSeconds": 300, "continueOnError": false } ],
  "postRun": [ { "command": "...", "runOn": "always", "continueOnError": false } ]
}
```

| Field | Meaning | Default / bound |
|---|---|---|
| `type` | Hook kind. Closed set. | `""` (defaults to `"command"`, the only value v1 accepts) |
| `name` | Trace label. Purely descriptive. | `""`; ≤ 64 bytes, printable, no control characters |
| `command` | Shell command run via `sh -c` through the run's `Executor`. Required. | ≤ 16 KB; rejected outright if it contains a `secret://` reference |
| `timeoutSeconds` | Per-hook timeout. | `0` → 300 s; max 1800 s (30 minutes) |
| `continueOnError` | A non-zero exit or timeout is recorded as a warning instead of failing the phase. | `false` |
| `runOn` | `postRun` only: filters this hook by the run's outcome. Closed set: `""` / `"always"`, `"success"`, `"failure"`. Rejected outright on a `preRun` hook. | `""` (runs regardless of outcome) |

Each phase is capped at 32 hooks. The sum of every `postRun` hook's
effective timeout must not exceed 1800 s — it sizes the detached
budget the loop grants `postRun` after the run's own wall-clock
timeout may have already expired (see
[`architecture.md`](architecture.md)). A `preRun` sum that exceeds the
run's own `timeout` is a warning, not a validation error: `preRun`
hooks run serially inside the same budget as the rest of the run, so
this usually still succeeds but is very likely to blow the timeout.

Credentials never belong in a hook `command` — `command` is operator
config recorded in the trace, the same treatment `verifier.command`
gets, so a `secret://` reference there would defeat the `SecretStore`
contract. `ValidateRunConfig` rejects it structurally
(case-insensitively), and the trace emitter also scrubs `command` for
secret-shaped substrings before persistence as defence-in-depth.
Resolve clone/deploy credentials via control-plane runtime bindings
instead.

### Failure semantics

| Situation | Outcome | Notes |
|---|---|---|
| A `preRun` hook without `continueOnError` fails or times out | `setup_failed` | The session never starts; zero turns run. `postRun` is skipped entirely — `Run()` returns before reaching it. |
| The run's own context is already dead (timeout/cancel) when the `preRun` failure is observed | The ctx-cause outcome (`timeout` / `cancelled`), not `setup_failed` | The hook almost certainly failed *because* the deadline hit or a control-plane cancel arrived mid-exec — that is the more useful outcome to report. |
| A `postRun` hook without `continueOnError` fails or times out, and the run would otherwise report `success` | `hook_failed` | Overrides `success` only. |
| Same, but the run's outcome was already non-`success` (e.g. `max_turns`, `error`) | The original outcome, unchanged | A `postRun` failure never masks the primary failure cause; it is still visible in `RunTrace.HookResults` and `RunResult.hookFailures`. |
| A hook with `continueOnError: true` fails or times out | The phase's outcome is unaffected | Recorded as a transport `warning` event and in `HookResults`; the remaining hooks in the phase still run. |

Every trace emitter (`jsonl`, `otel`, `gcs`) records each hook's result
in `RunTrace.HookResults`. The `otel` emitter also exports one `hook`
span per execution under its `hooks.preRun` / `hooks.postRun` phase
span, and attaches the output tail only when `captureContent` is on;
see [Span tree](observability-cloud.md#span-tree).

`postRun` hooks run on every outcome by default (`runOn: ""` /
`"always"`) — including `timeout` and `cancelled` — on a context
detached from the run's own cancellation and deadline
(`context.WithoutCancel`), so an artifact upload can still finish after
wall-clock expiry. `runOn: "success"` / `"failure"` scope a hook to one
branch. See [`security.md`](security.md) for the heartbeat caveat this
detachment carries.

Lifecycle hooks are **file / stdin / gRPC only** — there are no `--`
flags to select or configure a hook, matching the composite-config
convention already used for `tools.mcpServers` and multi-provider
routing (see [Component-selection limits](#component-selection-limits)
above). Set `hooks` in a `--config` file, over stdin, or in the
`task_assignment` a control plane sends over gRPC.

Hooks are allowed in every mode, including the read-only modes
(`planning`, `review`, `research`, `toil`). The [read-only-modes
invariant](#read-only-modes) bounds the *agent's* tools — what the
model can reach mid-conversation — not operator-authored, deterministic
commands declared in reviewable `RunConfig`. A `preRun` clone hook is
exactly what a planning run needs to have something to read; precedent
already exists for exec outside the tool surface in read-only modes
(the test-runner verifier's command, the `deterministic` git strategy's
branch creation).

A hook is not restricted to read-only actions in any mode, including a
read-only-named one: a `postRun` hook in a `planning`-mode run can
`git push`, open a pull request, or otherwise mutate state outside the
workspace, exactly as it could in `execution` mode. "Read-only mode"
describes what the *model* can do through its tools, not a guarantee
that the run as a whole has no side effects — a template author
inheriting hooks into a `planning` config should not assume the run is
side-effect-free on that basis alone.

Hooks add no agent-reachable capability, so they play no part in the
Rule of Two: their output never enters the model's context, and they
share the run's existing egress posture rather than opening a new
external-communication surface.

A worked example — clone the repository and install dependencies
before the session starts, submit an artifact after it ends:

```json
{
  "hooks": {
    "preRun": [
      { "name": "clone", "command": "git clone https://github.com/example/repo.git .", "timeoutSeconds": 60 },
      { "name": "bundle-install", "command": "bundle install", "timeoutSeconds": 900 }
    ],
    "postRun": [
      { "name": "submit-artifact", "command": "curl -sf -X POST -T build/output.tar.gz https://artifacts.example.com/upload", "runOn": "success", "continueOnError": true }
    ]
  }
}
```

## Sandbox identity and git-proxy wiring

`executor.sandboxIdentity` and `executor.gitProxy` (issue #516)
request a short-lived, control-plane-issued credential ("sandbox
identity token") for the sandbox and compose the non-secret git
configuration that routes clone/push traffic for private
repositories through a git-credential proxy such as
[haybale](https://github.com/rxbynerd/haybale). Neither block carries
the token itself — `RunConfig` and its `Redact()`ed form never see
the JWT; only the (non-secret) request shape and env-var wiring live
here. See [`deployment.md`'s "Sandbox identity token
issuance"](deployment.md#sandbox-identity-token-issuance-control-plane-implementers)
for the gRPC wire contract a control plane implements to answer the
request these fields configure.

```json
"executor": {
  "type": "container",
  "sandboxIdentity": {
    "source": "control-plane",
    "audience": "https://haybale.internal",
    "envVar": "HAYBALE_TOKEN"
  },
  "gitProxy": {
    "url": "http://haybale.internal:8466",
    "hosts": ["github.com"],
    "rewriteSsh": true,
    "tokenEnvVar": "HAYBALE_TOKEN"
  },
  "network": {
    "mode": "allowlist",
    "allowlist": ["haybale.internal:8466"]
  }
}
```

| Field | Meaning | Default / bound |
|---|---|---|
| `sandboxIdentity.source` | Token issuer. Closed set. | Required; only `"control-plane"` is supported |
| `sandboxIdentity.audience` | Intended JWT `aud` claim. Informational — the control plane may override it. | `""` |
| `sandboxIdentity.envVar` | Sandbox environment variable carrying the token as issued at sandbox creation. Not updated by refresh — `<envVar>_FILE` names the token file that is; see [Token file and refresh](#token-file-and-refresh). | `""` → `HAYBALE_TOKEN`; non-empty values must match `^[A-Za-z_][A-Za-z0-9_]*$` |
| `gitProxy.url` | The proxy's base URL, e.g. `http://haybale.internal:8466`. | Required when `gitProxy` is set; must be an absolute `http`/`https` URL with a host |
| `gitProxy.hosts` | Git hosts to rewrite through the proxy, e.g. `["github.com"]`. | Required (at least one host) when `gitProxy` is set |
| `gitProxy.rewriteSsh` | Also rewrite the `git@<host>:` and `ssh://git@<host>/` URL forms, not just `https://<host>/`. | `false` |
| `gitProxy.tokenEnvVar` | Token variable the git-proxy wiring is bound to. Must resolve to the same value as `sandboxIdentity.envVar` (both apply the same `HAYBALE_TOKEN` default when empty). The composed credential helper reads the token file rather than this variable, so refreshed tokens take effect on the next git operation. | `""` → `HAYBALE_TOKEN`; non-empty values must match `^[A-Za-z_][A-Za-z0-9_]*$` |

Both blocks are optional and pointer-typed: a `RunConfig` with neither
set validates exactly as it did before this feature existed,
regardless of `transport` or `executor.type` — none of the rules
below are gated on anything but the blocks' own presence.

`ValidateRunConfig` enforces:

- **`transport: "grpc"` is required.** Requesting a sandbox identity
  token means asking the control plane over the gRPC control stream;
  `stdio` has no upstream to ask. Setting `sandboxIdentity` or
  `gitProxy` under `transport: "stdio"` (or the unset/default
  transport) is a validation error.
- **Executor scope (v1).** `sandboxIdentity` / `gitProxy` are only
  valid for `executor.type` `"container"`, `"k8s"`, or
  `"k8s-sandbox"` — the executors that create a sandbox environment
  the harness controls at creation time. `"local"` is deferred (a
  developer machine already carries its own git credentials);
  `"api"` and `"none"` have no sandbox environment to inject into.
- **`gitProxy` requires `sandboxIdentity`.** v1's only token source
  is control-plane issuance via `sandboxIdentity` — a `gitProxy`
  block with no `sandboxIdentity` names an environment variable that
  would never be populated, so the combination is a validation
  error rather than a silent no-op.
- **`gitProxy.tokenEnvVar` must match the effective
  `sandboxIdentity.envVar`.** Both fields apply the same
  `HAYBALE_TOKEN` default when left empty, so two unset fields match
  by default; an explicit value on one side with no matching
  explicit value on the other is a mismatch and a validation error.
- **`gitProxy.url` is required and must be a well-formed endpoint.**
  An empty `gitProxy.url` is a validation error, and a non-empty one
  must parse as an absolute URL with scheme `http` or `https` and a
  non-empty host — the same shape check `guardRail.endpoint` applies.
- **`gitProxy.hosts` must name at least one host.** `hosts` is what
  drives how many proxy rewrite rules get composed; an empty list
  would let the block validate cleanly while silently routing
  nothing through the proxy, so it is a validation error.
- **`sandboxIdentity.envVar` and `gitProxy.tokenEnvVar` must be legal
  POSIX environment variable names when non-empty.** Both names are
  interpolated into a shell credential-helper string when the sandbox
  git configuration is composed, so each must match
  `^[A-Za-z_][A-Za-z0-9_]*$`. This is a charset check only — it does
  not block semantically sensitive but syntactically valid names such
  as `PATH`.

**Requesting a token requires the control-plane job entrypoint, not
just `transport: "grpc"`.** The `transport: "grpc"` validation rule
above is necessary but not sufficient: the token exchange must
complete *before* the executor is built, which only works when the
caller supplies an already-connected transport. In practice that means
the `stirrup job` entrypoint, which dials the control plane and blocks
for `task_assignment` before building the loop — see
[`deployment.md`](deployment.md#the-stirrup-job-subcommand). A bare
`stirrup harness` invocation (or any embedder calling
`BuildLoop`/`BuildLoopWithTransport(ctx, config, nil)`) that
configures `sandboxIdentity` fails closed at loop-build time with
`executor.sandboxIdentity requires a pre-established transport (only
supported via the control-plane job entrypoint, e.g. "stirrup job")`,
rather than silently building its own transport too late to serve the
request.

### Token file and refresh

The token is delivered to the sandbox twice over: once as the
`envVar` environment variable, baked in at sandbox creation, and as
a file at `/run/stirrup/sandbox-identity/token` (mode `0600`, owned
by the sandbox's unprivileged uid) that the harness writes
immediately after the sandbox starts and rewrites on every refresh.
`<envVar>_FILE` (`HAYBALE_TOKEN_FILE` by default) names that path so
consumers other than git can find it. The composed git credential
helper reads the file on every invocation, never the variable, so a
refreshed token is presented on the next git operation without
recreating the sandbox.

The `envVar` copy is never updated: after the first refresh it holds
an expired token, and nothing warns when that happens. Anything other
than the composed git configuration that needs the token — a
lifecycle hook, a `curl` against the proxy, an agent instruction —
must read `<envVar>_FILE` rather than `envVar`.

The file lives outside `/workspace` on a private memory-backed mount
(a dedicated tmpfs on the container executor, a `medium: Memory`
`emptyDir` on `k8s`/`k8s-sandbox`), so a workspace export never
includes it and a `git add -A` never sweeps it up. `read_file` cannot
reach it, and `write_file` and `edit_file` cannot overwrite it, either
by path or through a symlink committed into the workspace. On both the
container and the Kubernetes executors these tools first resolve the
real path inside the sandbox (`readlink -f`) and refuse anything that
resolves outside `/workspace`, because the
engine archive API, `tar`, and `mkdir -p` would otherwise follow such
a link (see
[path traversal prevention](security.md#path-traversal-prevention)).
The file is replaced by an atomic
rename so a credential-helper read racing a refresh sees the old
token or the new one, never a torn file, and the rename only happens
once the staged file's byte count matches the length the harness
announced, so an interrupted delivery leaves the previous token in
place rather than a truncated one. Each delivery runs `sh -c 'umask
077 && cat > … && mv -f …'` inside the sandbox with the token on the
command's stdin and only its byte length in argv, so the token never
appears in an exec argv — which the Docker daemon records on the exec
instance and the Kubernetes API server records in its audit log.
Inside the sandbox the file is exactly as reachable as the
environment variable already was (`run_command` can `cat` either),
so this adds no exposure to the model.

Refresh is driven by `sandbox_token_response.expires_at`. When the
control plane sets it, the harness schedules a new
`sandbox_token_request` once 80% of the token's remaining lifetime
has elapsed (a 15-minute token is refreshed after 12, with up to 5%
jitter so runs whose tokens were minted together do not refresh in
lockstep), delivers the new token to the file, and repeats from the
new expiry. A run sends at most **eight** `sandbox_token_request`s in
total — the initial exchange plus up to seven refreshes, matching the
control plane's per-stream cap. With an issuer lifetime of ten minutes
or more that budget outlasts the longest permitted `timeout` (3600 s);
a shorter lifetime exhausts it earlier and the run warns at that
point. When `expires_at` is absent no refresh is scheduled and the
token as issued stands for the whole run.

The 20% of lifetime left after the scheduled refresh is working
slack: a refresh whose request times out or cannot be sent is retried
after half of whatever lifetime then remains, for as long as any
remains and the request budget allows. A control plane that answers
— with a decline, an empty, oversized, or malformed token — is never
retried, since it would only answer the same way.

Every terminal outcome that leaves a token in the sandbox past its
expiry is reported, never silent: a declined refresh, a transient
failure with no lifetime left to retry in, a delivery failure, or an
exhausted request budget while the run's wall-clock budget still
exceeds the token's expiry each emits a transport `warning` event and
a `warn`-level log line naming the expiry, and the schedule stops. The
previous token stays in place — it remains valid until its own
expiry. A refreshed token that arrives without `expires_at` is
delivered and ends the schedule.

**Sandbox image and engine prerequisites (documented, not
enforced).** The composed git configuration relies on the sandbox
image providing `git` ≥ 2.31, for `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`
support, and a POSIX shell with `cat`, `mv`, `wc`, `tr`, and `rm`,
for the inline `!f() { ... }; f` credential-helper form and the
token-file delivery command. On the
container executor the engine must answer an exec start with a
`101 Switching Protocols` upgrade so stdin can be streamed — Docker
and Podman do; a socket proxy in front of the engine that does not
forward upgrades fails the initial token delivery. None of this is
checked by `ValidateRunConfig` — an older `git` fails at
git-invocation time inside the sandbox, and a shell-less image (e.g.
a fully static distroless image with no `/bin/sh`) or a non-upgrading
engine fails the initial token delivery at loop-build time, which
tears the sandbox down. The default sandbox image
([`ghcr.io/rxbynerd/stirrup-sandbox`](container-publishing.md))
qualifies.

Operators using `executor.network.mode: "allowlist"` should add the
proxy's `host:port` to `executor.network.allowlist` (as in the
example above) and deliberately leave the git hosts themselves
(`github.com`) off it, so the proxy is the only route to them — Ring
2's egress allowlist otherwise gives the sandbox no route to the
proxy at all. `ValidateRunConfig` warns (via `slog`, not a validation
error) when `gitProxy.url`'s `host:port` is absent from the
allowlist in `allowlist` mode; the check is a plain exact-match
against `gitProxy.url`, so an allowlist entry that only covers the
proxy host via a wildcard (`*.internal:8466`) may still trigger a
spurious warning even though the sandbox has a working route — the
common case being a proxy already covered by a wildcard-subdomain
allowlist entry, where the warning is expected and can be ignored.

## Limits and budgets

`ValidateRunConfig` enforces hard caps on values that could otherwise
be unbounded:

| Field | Cap |
|---|---|
| `maxTurns` | 100 |
| `timeout` | required; > 0 and ≤ 3600 s (same bound on `stirrup harness` and `stirrup job`) |
| `provider.batch.maxWaitSeconds` | the run's `timeout`; defaults to it when unset. See [`batch.md`](batch.md#the-wait-budget) |
| `followUpGrace` | 3600 s |
| `heartbeatSeconds` | `0` (default 30 s) or 5–600 s |
| `maxTokenBudget` | 50 M |
| `maxCostBudget` | $100 (not enforced — see below) |
| `temperature` | `0.0` ≤ `t` ≤ `2.0` |
| `hooks.preRun` / `hooks.postRun` | 32 hooks per phase |
| `hooks[].command` | 16 KB |
| `hooks[].timeoutSeconds` | 1800 s (30 min) per hook |
| sum of `hooks.postRun[].timeoutSeconds` | 1800 s (30 min) |

Read-only modes additionally require the tool list to be set.

Provider streams have no total time cap of their own. Each streamed
response is bounded by a fixed 120 s idle-read timeout instead: a
stream that keeps delivering bytes, keep-alive pings included, runs for
as long as the model generates, and one that stays silent for 120 s
fails with a read error (`stream idle for 120s` on the `net/http`
adapters). The run `timeout` is the total bound. Because
`ValidateRunConfig` rejects a missing or zero `timeout`, a slow but live
stream cannot outlast the run under `stirrup harness` or `stirrup job`.
Per-client timeouts are listed in
[`security.md`](security.md#http-client-hardening).

`maxTokenBudget` is the only budget the harness enforces. It is
checked between provider calls and terminates the run with
`outcome: "budget_exceeded"`. Consumption is input plus output summed
over every provider call on the run's credentials, including sub-agent
turns and summary, llm-judge and cloud-judge calls. Input is the
provider-reported figure, cached tokens included in full, wherever the provider reports one, and the harness
estimate otherwise; Gemini output includes thought tokens. See
[`trace-inspection.md`](trace-inspection.md#token-usage) for the
per-provider sources.

`maxCostBudget` is **accepted and bounded but not enforced**. The
harness carries no per-model price table, computes no cost, and never
terminates a run because of this field; `RunTrace.cost_usd` on the gRPC
wire is unpopulated for the same reason. Setting the field logs a
warning at config validation:

```
WARN maxCostBudget is accepted and bounded but not enforced: the harness
computes no cost, so no run terminates on this budget; cap spend in the
control plane from provider billing data maxCostBudget=25.5
```

The field stays on the schema for two reasons: retiring a proto field
number is a wire-compatibility break, and the JSON surfaces reject
unknown fields (`--config` files and stdin both decode with
`DisallowUnknownFields`), so removing the key would hard-fail every
existing config that sets it. The gRPC surface is not at risk either
way — proto3 ignores unknown fields. Spend limits belong upstream,
enforced from provider billing data; `maxTokenBudget` is the harness-side
proxy for the same intent. Enforcing a cost budget in the harness would
mean owning a versioned price table plus defined behaviour for unknown
models, dynamic routing, cached tokens, and reasoning tokens — a wrong
cost cap is worse than an absent one.

`temperature` accepts the union of provider-side ranges (Anthropic
`[0, 1]`, OpenAI / Gemini `[0, 2]`). A value inside the union may
still be rejected by the chosen provider's narrower range; the
adapter surfaces that rejection at request time rather than at
validation, so a single config can target multiple providers. Nil /
omitted means "use the harness default" (currently `0.1`); the
agentic loop forwards a non-nil value verbatim, including explicit
`0.0` for greedy decoding. Reasoning models that reject `temperature`
on the wire are handled separately by the provider adapter — the
field on the run config still represents intent.

`reasoningEffort` is the second provider-neutral model knob and
follows the same contract: a closed enum (`minimal`, `low`, `medium`,
`high`, `xhigh`, `max`) validated at startup, projected onto the
provider's native control by the adapter (Gemini `thinkingLevel`,
Anthropic `output_config.effort`, OpenAI-compatible `reasoning_effort`,
OpenAI Responses `reasoning.effort`), ignored for models without a
probed control, with per-model acceptance enforced by the
[provider quirks registry](provider-quirks.md#32-reasoning-effort)
before the request is sent. The Responses adapter omits `temperature`
from any request that carries `reasoning.effort`.

### Reasoning summaries

`reasoningSummary` is the third provider-neutral model knob. It
decides whether the run streams the model's reasoning text to the
transport as `thinking_delta` events, for a control plane that renders
a live transcript. It accepts `auto` and `off`, validated at startup.
An empty value is projected by each adapter: every adapter treats it as
`auto` except `openai-responses`, which treats it as `off` (see below),
and `bedrock`, which streams no reasoning text whatever the value. It
never changes what is kept in conversation history: reasoning the
provider needs replayed (Anthropic thinking blocks, DeepSeek
`reasoning_content`, OpenAI Responses reasoning items, Gemini thought
signatures) is persisted and replayed exactly as before.

| Adapter | `auto` | `off` |
|---|---|---|
| `anthropic` | Streams thinking the request already enables through `reasoningEffort`; no request change. | No request change; the loop forwards no `thinking_delta`. |
| `openai-compatible` | Streams each `reasoning_content` delta the server sends (DeepSeek, GLM, Qwen), or each `reasoning` delta (OpenRouter); no request change, because Chat Completions has no reasoning-summary control. | No request change; the loop forwards no `thinking_delta`. |
| `openai-responses` | Adds `reasoning.summary: "auto"` to any request that carries `reasoning.effort`, and streams `response.reasoning_summary_text.delta`. A request with no projected effort carries no `reasoning` object and no summary. | Sends no `reasoning.summary`. An empty value does the same. |
| `gemini` | Sends `generationConfig.thinkingConfig.includeThoughts: true` when the model's quirks accept a thinking config (Gemini 3.6, 3.7, 3.8, 3.1 Pro and 2.5 Flash) or `reasoningEffort` is set, and streams each `thought: true` part. Other models get no `thinkingConfig`, because models that do not think reject one. Thought parts never become answer text or tool input. | Sends no `includeThoughts`. |
| `bedrock` | No effect: the adapter streams no reasoning text and has no request control for it. | No effect. |

`openai-responses` requests summaries only on an explicit `auto`
because OpenAI may require organisation verification before it returns
them on its newest reasoning models, and that requirement has not
been probed against OpenAI's own API; an unset value sends no
`reasoning.summary`, which unverified organisations can send. OpenRouter's Responses endpoint
accepts the control and streams the summary (see
[provider quirks](provider-quirks.md#9-open-risks), risk 12).

Each turn record's `request.reasoningSummary` holds the value resolved
for the selected adapter, so an unset field is recorded as `off` on
`openai-responses` and `auto` elsewhere. A recorded `auto` on
`openai-responses` still means no summary was sent when the request
carried no `reasoning.effort`.

Reasoning text is best-effort: providers summarise rather than
transcribe, and may return no summary for a short turn. The harness's
own model calls (the context summariser, the LLM-judge verifier, and
the cloud guard judge) never request reasoning text.

## RunConfig examples

The shipped example files cover the common deployment shapes. Each
passes `ValidateRunConfig` end-to-end.

| File | What it demonstrates |
|---|---|
| [`examples/runconfig/full.json`](../examples/runconfig/full.json) | Container executor with `runsc` runtime, multi edit strategy, OTel trace emitter, deterministic git, dynamic model router, Cedar destructive-shell backstop over an `allow-all` fallback (the pairing where the forbid is the run's only gate on `run_command` — a `deny-side-effects` fallback would deny every `run_command` on no-match), Granite Guardian guardrail, and one MCP server. The most comprehensive example. |
| [`examples/runconfig/openai_responses.json`](../examples/runconfig/openai_responses.json) | OpenAI Responses API provider, local executor, multi edit strategy, JSONL trace emitter, static router on `gpt-4.1`. |
| [`examples/runconfig/azure-openai.json`](../examples/runconfig/azure-openai.json) | Azure OpenAI Foundry's Responses endpoint via the `openai-responses` provider, with `apiKeyHeader: "api-key"` and `queryParams: {"api-version": "preview"}`. Switch the header to an empty string to use Entra ID bearer tokens. |
| [`examples/runconfig/vertex-gemini.json`](../examples/runconfig/vertex-gemini.json) | Vertex AI Gemini on `gemini-2.5-pro`. Auth is GCP IAM via Application Default Credentials by default. |
| [`examples/runconfig/vertex-gemini-wif.json`](../examples/runconfig/vertex-gemini-wif.json) | Vertex AI Gemini reached from a non-GCP runtime via Workload Identity Federation. Surfaces an EKS-style `aws-irsa` token source. |
| [`examples/runconfig/anthropic-wif-github-actions.json`](../examples/runconfig/anthropic-wif-github-actions.json) | Anthropic Messages API authenticated via WIF from a GitHub Actions runner. |
| [`examples/runconfig/anthropic-wif-eks-irsa.json`](../examples/runconfig/anthropic-wif-eks-irsa.json) | Anthropic Messages API authenticated via WIF from an EKS pod with IRSA. |
| [`examples/runconfig/azure-openai-wif-aks.json`](../examples/runconfig/azure-openai-wif-aks.json) | Azure OpenAI from AKS via Entra ID Workload Identity Federation. |
| [`examples/runconfig/azure-openai-wif-github-actions.json`](../examples/runconfig/azure-openai-wif-github-actions.json) | Azure OpenAI from GitHub Actions via Entra ID Workload Identity Federation. |
| [`examples/runconfig/openai-wif-github-actions.json`](../examples/runconfig/openai-wif-github-actions.json) | OpenAI API (Responses) authenticated via WIF from a GitHub Actions runner. |
| [`examples/runconfig/openai-wif-eks-irsa.json`](../examples/runconfig/openai-wif-eks-irsa.json) | OpenAI API (Chat Completions) authenticated via WIF from an EKS pod with IRSA. |
| [`examples/runconfig/grafana-cloud.json`](../examples/runconfig/grafana-cloud.json) | Native OTLP/HTTP export to Grafana Cloud's managed gateway. No Alloy/OTel-Collector sidecar needed. |

For an annotated walkthrough of `full.json` see
[`examples/runconfig/README.md`](../examples/runconfig/README.md).

## Shell completions

Both `stirrup` and `stirrup-eval` emit completion scripts for `bash`,
`zsh`, `fish`, and `powershell`. The scripts cover subcommands, flag
names, the closed-set enum flags (`--mode`, `--provider`, `--executor`,
`--edit-strategy`, `--verifier`, `--git-strategy`, `--transport`,
`--trace-emitter`, `--otel-protocol`, `--container-runtime`,
`--code-scanner`, `--guardrail`), and filesystem completion for the
path-shaped flags (`--config`, `--workspace`, `--prompt-file`,
`--gcp-credentials-file`, `--replay-file`, `--permission-policy-file`,
`--trace`, `--output-runconfig`).

Closed-set enum completion is sourced from the same maps the validator
consults, so a value added to `types/runconfig.go` extends the
completion surface automatically.

### Installation

```sh
# bash (current session)
source <(stirrup completion bash)
source <(stirrup-eval completion bash)

# bash (persistent, Linux)
stirrup completion bash | sudo tee /etc/bash_completion.d/stirrup >/dev/null
stirrup-eval completion bash | sudo tee /etc/bash_completion.d/stirrup-eval >/dev/null

# zsh
stirrup completion zsh > "${fpath[1]}/_stirrup"
stirrup-eval completion zsh > "${fpath[1]}/_stirrup-eval"

# fish
stirrup completion fish > ~/.config/fish/completions/stirrup.fish
stirrup-eval completion fish > ~/.config/fish/completions/stirrup-eval.fish

# powershell
stirrup completion powershell | Out-String | Invoke-Expression
stirrup-eval completion powershell | Out-String | Invoke-Expression
```

On zsh, `compinit` must be initialised before the completion script
loads — most distributions ship a `~/.zshrc` template that does this;
operators with a hand-rolled `.zshrc` need an explicit
`autoload -Uz compinit && compinit` ahead of the `source` line.

## Eval CLI

```sh
go build -o stirrup-eval ./eval/cmd/eval

# Run an eval suite
./stirrup-eval run --suite path/to/suite.hcl --output results/ \
  [--harness path/to/harness] [--trials 3] [--dry-run]

# Compare two eval results (exit 1: the gate blocks; exit 2: usage or I/O error)
./stirrup-eval compare --current results/result.json \
  --baseline baseline/result.json \
  [--warn-margin 0.10] [--flip-threshold 0.5] [--output comparison.json]

# Pull production metrics as a baseline
./stirrup-eval baseline --lakehouse path/to/lakehouse \
  [--after 2026-03-01] [--mode execution] [--output metrics.json]

# Mine failures into eval tasks
./stirrup-eval mine-failures --lakehouse path/to/lakehouse \
  [--after 2026-03-01] [--limit 20] [--output suite.hcl]

# Detect metric drift between time windows
./stirrup-eval drift --lakehouse path/to/lakehouse \
  --window 7d [--compare-window 7d] [--mode execution]

# Compare eval results against production metrics
./stirrup-eval compare-to-production --results results/result.json \
  --lakehouse path/to/lakehouse \
  [--after 2026-03-01] [--experiment-id exp1]

# Convert an eval result to JUnit XML
./stirrup-eval convert --results results/result.json --format junit
```

Eval suites are HCLv2; `eval run --suite` requires a `.hcl`
extension. `mine-failures` output is canonical HCL loadable without
conversion. `drift` exits 1 on pass-rate drops greater than 5
percentage points or turn increases greater than 20%.

Full reference: [`eval.md`](eval.md).
