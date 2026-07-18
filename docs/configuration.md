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

The report lists each step with one of three statuses:

- `ok` — the component constructed and (where applicable) its probe
  succeeded.
- `skip` — the step was intentionally not run: the component has no
  network probe (a local executor, a `jsonl` trace file), the feature is
  not configured (no MCP servers, no workspace export), or a
  `--no-probe-*` gate suppressed it.
- `fail` — construction or a probe failed. The step names the component,
  carries the underlying error, and — where a concrete next step is
  known — a remediation hint.

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
| Every step `ok` or `skip` | `0` |
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
| `--timeout` | `600` | Wall-clock seconds; capped at 3600. |
| `--temperature` | (unset → `0.1`) | Sampling temperature forwarded to the provider on every turn. Range `0.0`–`2.0` (the union of provider-side ranges; see [Limits and budgets](#limits-and-budgets)). Omit the flag to inherit the harness default; pass an explicit `0` for greedy decoding. The runtime distinguishes "flag absent" from `--temperature=0` via cobra's `Changed()` bit. |
| `--log-level` | `info` | One of `debug`, `info`, `warn`, `error`. |

### Loop behaviour

| Flag | Default | Notes |
|---|---|---|
| `--max-tool-parallel` | `0` | Maximum async tool calls dispatched concurrently in a single turn. Range `1`–`16` (hard ceiling enforced by `ValidateRunConfig`); `0` resolves to the library default of `4`. JSON path: `toolDispatch.maxParallel`. |
| `--escalate-tool-choice` | `false` | Recover from a first-turn no-tool answer on a workspace-dependent task by retrying with provider-native required tool choice (a stronger prompt where the provider does not support forcing). Off by default (issue #230). JSON path: `toolChoiceEscalation.enabled`. |
| `--escalate-tool-choice-max-retries` | `0` | Maximum forced retries per inner-loop run. Range `1`–`3`; `0` resolves to the default of `1`. No effect unless `--escalate-tool-choice` is set. JSON path: `toolChoiceEscalation.maxRetries`. |

### Provider

| Flag | Default | Notes |
|---|---|---|
| `--provider` | `anthropic` | One of `anthropic`, `bedrock`, `openai-compatible`, `openai-responses`, `gemini`. |
| `--model` | `claude-sonnet-4-6` | Model id for the static / per-mode router. |
| `--prompt-model` | (none) | Model identity the system prompt templates render against, without changing the wire model. Combine with `--model` to compare a prompt tuned for one model against another. Empty derives the prompt model from the effective model. JSON path: `promptBuilder.promptModel`. See [System prompt templating](#system-prompt-templating). |
| `--api-key-ref` | `secret://ANTHROPIC_API_KEY` | A `secret://` reference. API keys never live in `RunConfig`. Ignored when `--provider=gemini` (Vertex uses GCP IAM). |
| `--base-url` | (none) | Provider base URL. Required for Azure / gateway scenarios. |
| `--api-key-header` | (none) | Header name. Empty = `Authorization: Bearer`; set to `api-key` for Azure key auth. |
| `--query-param key=value` | (none) | Repeatable. Adds query parameters to every provider request URL — e.g. `--query-param api-version=preview` for Azure. Keys here override duplicates already encoded in `--base-url`. |
| `--provider-compat-profile` | (none) | Closed enum that loads a provider-quirks compatibility profile. Only legal value in v1: `"zai-glm"` (Z.ai GLM legacy `max_tokens` key + `tool_stream: true` extension). Unknown values fail at startup. JSON path: `provider.compatProfile`. |

Wire-shape divergences between provider/model pairs (e.g. OpenAI
reasoning-class sampling-param omissions, Z.ai GLM legacy field
names, Gemini 3.x `thoughtSignature` preservation) are not exposed
on `RunConfig`. They are first-party rules in the
`harness/internal/provider/quirks` registry, applied per-stream by
the adapter. The `--provider-compat-profile` flag is the only
operator-facing surface that influences quirk resolution; all other
rules apply automatically based on `provider.type` and `model`.
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
429/5xx on the initial response), never a failure after the stream
has started yielding events. `bedrock` has no raw `*http.Client`
seam — `ConverseStream` goes through the AWS SDK's own transport —
so `maxAttempts` and `maxDelayMs` are mapped onto the SDK's Standard
retryer instead; `initialDelayMs` and `wallClockBudgetMs` have no
SDK-native equivalent and are not applied to `bedrock`.

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
| `--k8s-egress-proxy-url` | (none) | URL the `k8s` / `k8s-sandbox` Pod routes `HTTP_PROXY`/`HTTPS_PROXY` through. Required when the executor is `k8s` or `k8s-sandbox` and the network mode is `allowlist`; rejected otherwise. JSON path: `executor.k8sEgressProxyUrl`. |
| `--edit-strategy` | `multi` | One of `whole-file`, `search-replace`, `udiff`, `multi`. `composite` is reachable only via `--config`. |
| `--verifier` | `none` | One of `none`, `test-runner`, `llm-judge`. `composite` is reachable only via `--config`. |
| `--git-strategy` | `none` | One of `none`, `deterministic`. |
| `--permission-policy-file` | (none) | Path to a Cedar policy file. When set and the policy type is unset elsewhere, implies `permissionPolicy.type=policy-engine`. Starters live under [`examples/policies/`](../examples/policies/). |
| `--code-scanner` | (none) | One of `none`, `patterns`, `semgrep`. `composite` is accepted only via `--config` (it requires `codeScanner.scanners`). Empty defers to the mode-aware default (`patterns` for execution, `none` for read-only modes). |
| `--guardrail` | (none) | GuardRail classifier type: `none`, `granite-guardian`, `cloud-judge`, `composite`. `composite` requires `--config` (`guardRail.stages`). JSON path: `guardRail.type`. See [`guardrails.md`](guardrails.md). |
| `--guardrail-endpoint` | (none) | Classifier endpoint URL for the `granite-guardian` or `cloud-judge` adapter (http/https; a path such as `/v1/chat/completions` is allowed). JSON path: `guardRail.endpoint`. |
| `--guardrail-model` | (none) | Model identifier for the GuardRail classifier. Empty applies the adapter-defined default: `ibm-granite/granite-guardian-4.1-8b` for `granite-guardian`, `claude-haiku-4-5-20251001` for `cloud-judge`. The `cloud-judge` default is in Anthropic API format — when the primary provider is Bedrock, set the Bedrock-format ID (e.g. `us.anthropic.claude-haiku-4-5-20251001-v1:0`). JSON path: `guardRail.model`. |
| `--guardrail-fail-open` | `false` | When set, classifier transport errors / timeouts produce an allow verdict plus a `guard_error` security event instead of blocking the run. Default is fail-closed. Top-level only — governs the whole guardrail tree. JSON path: `guardRail.failOpen`. See [`guardrails.md`](guardrails.md#fail-open-posture). |
| `--tools-profile` | (none) | Model-facing toolset profile. Closed enum: `""`/`default` (no aliasing, internal tool names) or `coding-classic` (terse coding-CLI aliases). Changes only the names the model sees; dispatch identities and gating are unchanged. JSON path: `tools.profile`. See [Toolset profiles](#toolset-profiles). |

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
| `--followup-grace` | `0` | Seconds to keep gRPC open for follow-ups. Env fallback: `STIRRUP_FOLLOWUP_GRACE`. |

### Tracing

| Flag | Default | Notes |
|---|---|---|
| `--trace <path>` | (none) | JSONL trace path. Implies `--trace-emitter=jsonl` unless overridden. |
| `--trace-emitter` | `jsonl` | One of `jsonl`, `otel`, `gcs`. |
| `--otel-endpoint` | (none) | OTLP endpoint. Defaults to `localhost:4317` for `--otel-protocol=grpc`; for `http/protobuf` use the gateway base path (e.g. `https://otlp-gateway-prod-us-east-0.grafana.net/otlp`). |
| `--otel-protocol` | (none) | OTLP wire protocol: `""` (defaults to grpc), `grpc`, `http/protobuf`. HTTP/JSON is intentionally not supported. See [`observability-cloud.md`](observability-cloud.md). |
| `--otel-header` | (none) | Repeatable `key=value` HTTP header attached to every OTLP export request. Values may be `secret://` references resolved at exporter init — never pass raw secrets. Requires `--otel-protocol=http/protobuf` (validation rejects headers on the plaintext gRPC path). Explicit flags replace any `headers` map from `--config`. The `OTEL_EXPORTER_OTLP_HEADERS` env var is the SDK-native fallback when no headers are configured. |
| `--otel-metrics-endpoint` | (none) | OTLP endpoint for the metrics exporter when metrics target a different collector than traces. Defaults to `--otel-endpoint`. |
| `--otel-capture-content` | `false` | Opt the otel emitter into recording prompt/completion content on turn spans via the GenAI semconv attributes (`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`). Off by default: message content is likely to contain PII. Content is scrubbed for secret-shaped substrings before export. See [`observability-cloud.md`](observability-cloud.md#span-content-capture-opt-in). |
| `--deployment-environment` | (none) | OTel `deployment.environment` resource attribute (e.g. `production`, `staging`). Empty falls through to env `OTEL_DEPLOYMENT_ENVIRONMENT`, then to `local`. |
| `--service-namespace` | (none) | OTel `service.namespace` resource attribute (e.g. `stirrup-eval`, `team-a`). Empty falls through to env `OTEL_SERVICE_NAMESPACE`, then to `stirrup`. |

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

The exit code reflects the run outcome regardless of `--output`: a
failed or cancelled run still exits non-zero. A cancellation
mid-flight emits a partial `RunResult` carrying the cancellation
outcome rather than nothing, so callers parsing the JSON line always
see a structured record.

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

### Workspace export

At end-of-run the executor's workspace can be tarred, gzipped, and
uploaded to a GCS URI — the result-collection surface for serverless
targets with no persistent filesystem (see
[`cloud-run-jobs.md`](cloud-run-jobs.md#shape-b-workspace-tarball-from-gcs)
for the operator walkthrough).

| Flag | Default | Notes |
|---|---|---|
| `--export-workspace-to` | (none) | Destination URI for the workspace tarball (e.g. `gs://bucket/runs/<runId>/workspace.tar.gz`). Only `gs://` is supported in v1. Overrides `executor.workspaceExportTo` from `--config` when explicitly set; an explicit empty value clears the field. JSON path: `executor.workspaceExportTo`. |
| `--export-workspace-required` | `false` | When set, a failed workspace export exits the run non-zero — suitable for jobs whose downstream automation depends on the artifact. When unset (default), upload failures are logged and the run's exit code is unchanged. CLI-behaviour flag only: it does not round-trip through `RunConfig`. |

The export runs even when the run itself failed, so the workspace
state stays inspectable after a non-zero exit. Uploads authenticate
via the `gcp-workload-identity` credential source — the GCE/GKE
metadata server that Cloud Run, GKE Workload Identity, and plain GCE
VMs expose. There is no credential override for the exporter in v1.

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
stderr. The scheme is uniform across `harness`, `job`, and
`run-config`:

| Code | Class | Examples |
|---|---|---|
| `0` | Success | The command completed; for `harness`, a run reached a terminal outcome. |
| `1` | Validation / precondition | `ValidateRunConfig` (or `run-config --validate`) rejected the resolved config; a required prompt had no source; `job` ran without `CONTROL_PLANE_ADDR`. Also the default for any failure not in a more specific class. |
| `2` | Parse error | The JSON in a `--config` file or piped stdin failed to decode (syntax error, unknown field, type mismatch). |
| `3` | I/O error | A `--config` or `--prompt-file` path could not be opened, read, or stat'd; an empty / oversize input; an `--output-runconfig` write or close failure. |
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

## Component-selection limits

The CLI deliberately exposes only a subset of each component's
configuration space — the common cases. Anything below requires
`--config`:

| Component | What needs `--config` |
|---|---|
| `editStrategy` | `composite` (chains other strategies). |
| `verifier` | `composite` (chains other verifiers). |
| `permissionPolicy` | `policy-engine` requires `policyFile`; the optional `fallback` field defaults to `deny-side-effects` when unset. Chained policy engines are rejected. |
| `codeScanner` | `composite` requires `codeScanner.scanners` (each entry from the non-composite set). |
| `guardRail` | `composite` requires `guardRail.stages`. Per-phase restriction (`phases`), bespoke criteria, and the classifier timeout are file-only. |
| `traceEmitter` | `bucket` / `objectPrefix` / `credential` (the `gcs` emitter's routing — selectable via `--trace-emitter gcs` but configurable only by file). |
| `provider` | Multi-provider routing via `providers{}` plus a `modelRouter` of type `dynamic` or `per-mode`. |
| `tools.mcpServers` | Remote MCP server registration. |

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

| Tool examples | Flags | `allow-all` | `deny-side-effects` | `ask-upstream` |
|---|---|---|---|---|
| `read_file`, `list_directory`, `grep_files`, `find_files`, `git_status`, `git_changed_files`, `git_diff`, `git_show` | neither flag | Allow | Allow | Allow |
| `web_fetch`, `spawn_agent` | `RequiresApproval` only | Allow | Allow | Prompt |
| `run_command` | `WorkspaceMutating` + `RequiresApproval` | Allow | Deny | Prompt |
| `edit_file`, `write_file` | `WorkspaceMutating` + `RequiresApproval` | Allow | Deny | Prompt |

`policy-engine` evaluates the Cedar policy first. When Cedar returns
no decision, the configured fallback (`deny-side-effects` by default)
applies exactly the same rules as the corresponding non-Cedar policy.
For example, a policy engine with `fallback: "deny-side-effects"`
still allows `web_fetch` and `spawn_agent` unless a Cedar `forbid`
matches them, because neither tool mutates the workspace.

Choose `ask-upstream` when every `RequiresApproval` tool must prompt.
Choose `deny-side-effects` when the goal is to block workspace
mutation while still allowing non-mutating tools that may have network
or budget exposure.

## Read-only modes

`planning`, `review`, `research`, and `toil` enforce a structural
invariant via `ValidateRunConfig`: the tool list must exclude
`write_file`, `run_command`, and `edit_file`, and the permission
policy must not be `allow-all`. The validator rejects any
`RunConfig` that violates this before any component is constructed.

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
| `frontier` | `claude-fable-5*`, `claude-mythos-5*`, `claude-sonnet-5*`, `claude-opus-4-8*`, `gpt-5.5*`, `gpt-5.6*` | Lean behavioural additions: act when ready, scope discipline, evidence-grounded progress claims, outcome-first summaries. |
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
| `sandboxIdentity.envVar` | Sandbox environment variable the token is injected as. | `""` → `HAYBALE_TOKEN`; non-empty values must match `^[A-Za-z_][A-Za-z0-9_]*$` |
| `gitProxy.url` | The proxy's base URL, e.g. `http://haybale.internal:8466`. | Required when `gitProxy` is set; must be an absolute `http`/`https` URL with a host |
| `gitProxy.hosts` | Git hosts to rewrite through the proxy, e.g. `["github.com"]`. | Required (at least one host) when `gitProxy` is set |
| `gitProxy.rewriteSsh` | Also rewrite the `git@<host>:` and `ssh://git@<host>/` URL forms, not just `https://<host>/`. | `false` |
| `gitProxy.tokenEnvVar` | Environment variable the composed git-credential helper reads the token from. Must resolve to the same value as `sandboxIdentity.envVar` (both apply the same `HAYBALE_TOKEN` default when empty). | `""` → `HAYBALE_TOKEN`; non-empty values must match `^[A-Za-z_][A-Za-z0-9_]*$` |

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

**Sandbox image prerequisites (documented, not enforced).** The
composed git configuration relies on two things the sandbox image
must provide: `git` ≥ 2.31, for `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`
support, and a POSIX shell, for the inline `!f() { ... }; f`
credential-helper form the composed config uses. Neither is checked by
`ValidateRunConfig` or the executor — an older `git`, or a shell-less
image (e.g. a fully static distroless image with no `/bin/sh`), fails
at git-invocation time inside the sandbox, not at config-load time.
The default sandbox image
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
| `timeout` | 3600 s |
| `followUpGrace` | 3600 s |
| `maxTokenBudget` | 50 M |
| `maxCostBudget` | $100 |
| `temperature` | `0.0` ≤ `t` ≤ `2.0` |
| `hooks.preRun` / `hooks.postRun` | 32 hooks per phase |
| `hooks[].command` | 16 KB |
| `hooks[].timeoutSeconds` | 1800 s (30 min) per hook |
| sum of `hooks.postRun[].timeoutSeconds` | 1800 s (30 min) |

Read-only modes additionally require the tool list to be set.

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

## RunConfig examples

The shipped example files cover the common deployment shapes. Each
passes `ValidateRunConfig` end-to-end.

| File | What it demonstrates |
|---|---|
| [`examples/runconfig/full.json`](../examples/runconfig/full.json) | Container executor with `runsc` runtime, multi edit strategy, OTel trace emitter, deterministic git, dynamic model router, Cedar policy engine with `deny-side-effects` fallback, Granite Guardian guardrail, and one MCP server. The most comprehensive example. |
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
`--gcp-credentials-file`, `--permission-policy-file`, `--trace`,
`--output-runconfig`).

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
  [--harness path/to/harness] [--dry-run]

# Compare two eval results
./stirrup-eval compare --current results/result.json \
  --baseline baseline/result.json

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
