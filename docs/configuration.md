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
  noninteractive automation.

Piping a config into `stirrup harness` consumes stdin at startup,
before transport initialisation, so the stdio transport sees EOF for
any subsequent control-event input. This matches the batch shape the
pipeline pattern targets — operators who need interactive control
should use `--transport grpc`.

## CLI flags

`stirrup harness --help` is authoritative. The table below documents
the same flags grouped by concern.

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

### Provider

| Flag | Default | Notes |
|---|---|---|
| `--provider` | `anthropic` | One of `anthropic`, `bedrock`, `openai-compatible`, `openai-responses`, `gemini`. |
| `--model` | `claude-sonnet-4-6` | Model id for the static / per-mode router. |
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

Currently honoured only by the `openai-compatible` adapter; the
`anthropic`, `bedrock`, `gemini`, and `openai-responses` adapters
fall through unconditionally pending their own wire-ups (tracked in
follow-up issues).

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

### Components

| Flag | Default | Notes |
|---|---|---|
| `--executor` | `local` | One of `local`, `container`, `api`. |
| `--container-runtime` | (none) | OCI runtime: `runc`, `runsc` (gVisor), `kata`, `kata-qemu`, `kata-fc`. Empty = engine default. Requires the runtime to be registered with the host Docker/Podman daemon. |
| `--edit-strategy` | `multi` | One of `whole-file`, `search-replace`, `udiff`, `multi`. `composite` is reachable only via `--config`. |
| `--verifier` | `none` | One of `none`, `test-runner`, `llm-judge`. `composite` is reachable only via `--config`. |
| `--git-strategy` | `none` | One of `none`, `deterministic`. |
| `--permission-policy-file` | (none) | Path to a Cedar policy file. When set and the policy type is unset elsewhere, implies `permissionPolicy.type=policy-engine`. Starters live under [`examples/policies/`](../examples/policies/). |
| `--code-scanner` | (none) | One of `none`, `patterns`, `semgrep`. `composite` is accepted only via `--config` (it requires `codeScanner.scanners`). Empty defers to the mode-aware default (`patterns` for execution, `none` for read-only modes). |

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
| `--trace-emitter` | `jsonl` | One of `jsonl`, `otel`. |
| `--otel-endpoint` | (none) | OTLP endpoint. Defaults to `localhost:4317` for `--otel-protocol=grpc`; for `http/protobuf` use the gateway base path (e.g. `https://otlp-gateway-prod-us-east-0.grafana.net/otlp`). |
| `--otel-protocol` | (none) | OTLP wire protocol: `""` (defaults to grpc), `grpc`, `http/protobuf`. HTTP/JSON is intentionally not supported. See [`observability-cloud.md`](observability-cloud.md). |
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
| `traceEmitter` | `headers` (for OTLP/HTTP auth). |
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
| `read_file`, `list_directory`, `search_files` | neither flag | Allow | Allow | Allow |
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
