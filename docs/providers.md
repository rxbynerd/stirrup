# Provider adapters

Stirrup ships five provider adapters. All share the same `ProviderAdapter`
interface and are selected via `provider.type` in `RunConfig` (or
`--provider` on the CLI). Authentication is decoupled through the
`credential` package — see
[`docs/credential-federation.md`](credential-federation.md) for the
full two-tier `TokenSource` → `credential.Source` abstraction.

## Anthropic

**File:** `harness/internal/provider/anthropic.go`

SSE streaming via `net/http` + `bufio.Scanner`. Hand-rolled against the
Messages API; no Anthropic SDK dependency. Auth: API key resolved from
a `secret://` reference, or keyless via Anthropic Workload Identity
Federation — see [`docs/anthropic-wif.md`](anthropic-wif.md).

The two auth modes use different, non-interchangeable request headers:
a static API key (`sk-ant-api03-...`) goes in `x-api-key`, while a WIF
OAuth access token (`sk-ant-oat01-...`) requires `Authorization:
Bearer`. Sending a WIF token via `x-api-key` returns an Anthropic 401;
the adapter selects the header from the credential source's type, not
by inspecting the token.

Default safety thresholds: none — the harness does not configure
Anthropic safety settings; the API defaults apply.

No model-ID allowlist: `RunConfig.Model` is forwarded to the wire
verbatim, so a newly released Claude model works with no code change
as long as its request/response shape matches the Messages API
contract this adapter already speaks. Claude Opus 4.7 and later
(including Opus 5 and 5.5), Claude Sonnet 5 and 5.5, and Claude Fable 5
/ 5.1 and Mythos 5 reject a non-default `temperature` outright (HTTP
400) rather than ignoring it; since the harness always resolves a
non-nil default temperature (`core.defaultTemperature = 0.1`) when
`RunConfig.Temperature` is unset, a per-model quirk rule omits the field
for those models before it reaches the wire — see [Per-model wire-shape
quirks](#per-model-wire-shape-quirks) below.

`RunConfig.reasoningEffort` maps to `output_config.effort` for the
Claude models whose accepted levels have been probed (Opus 4.5 onward,
Sonnet 4.6 onward, Fable 5 onward); other Claude models receive no
effort field. Sonnet 5.5, Opus 5.5 and Fable 5.1 reject forced tool
choice, so the harness never sends `tool_choice` `any` or `tool` to
them. These models think by default and stream `thinking` blocks; the
adapter drops them, and the API accepts a replayed tool-use turn
without them.

**Prompt caching.** The loop re-sends the tool list, the system prompt
and the full history on every turn, and the history only grows between
compactions, so each request shares a long prefix with the one before
it. The API caches nothing unless the request marks a breakpoint, so
the adapter marks up to two on every Claude model (the `PromptCaching`
[quirk](provider-quirks.md#21-providerquirks)). The rule was probed on
Sonnet 5.5, Haiku 4.5, Sonnet 4.5 and Opus 4.5 on 2026-09-30; the other
families are documented, not probed. `claude-opus-4-1` returned 404 on
the same date, so the API no longer serves it.

- `system` goes out as a single text block carrying
  `"cache_control": {"type": "ephemeral"}` on every request. The cached
  prefix is tools, then system, then messages, so this breakpoint keeps
  tools and system cached for the whole run, including after a context
  strategy rewrites earlier messages. One-shot callers (the summarise
  context strategy, the LLM-judge verifier and the cloud-judge guard)
  carry it too, but their system prompts fall below every cacheable
  minimum, so nothing is cached for them.
- A top-level `"cache_control": {"type": "ephemeral"}` turns on automatic
  caching: the API places a second breakpoint on the last cacheable block
  of each request, so the history written on one turn is read back on
  the next. Only main-loop turns send it, identified by the per-run cache
  key described under [OpenAI Responses](#openai-responses-api); a one-shot
  call never extends a history, so a write there would not be read back.

On a main-loop turn with an empty system prompt, only the top-level
field is sent. The [batch path](batch.md) sends neither, since
consecutive batch turns rarely land within the cache lifetime.

The first request of a run writes the prefix (`cacheWrite`); each later
request reads it back (`cacheRead`) and writes only the new tail. Reads
bill at 0.1x the base input rate (0.05x on Opus 5.5, 0.025x on Fable
5.1) and writes at 1.25x, with the default five-minute cache lifetime.
One Sonnet 5.5 run on 2026-09-30 wrote 6,505 tokens on turn 0 and read
6,505 to 6,716 on each later turn, leaving 2 to 4 input tokens uncached
per turn.

A prompt shorter than the model's cacheable minimum is sent uncached,
with no error: 512 tokens on Opus 5 / 5.5, Sonnet 5.5, Fable 5 / 5.1 and
Mythos 5 / 5.1; 1,024 on Opus 4.8, Sonnet 5, Sonnet 4.6 and Sonnet 4.5;
4,096 on Haiku 4.5. The execution-mode toolset clears all of these
(Haiku 4.5 wrote 4,887 tokens on turn 0 of a live run), but a run with
fewer tools, such as a reduced `tools.builtIn` list, can fall below
Haiku's line; a live Haiku run whose turns sent 1,571 to 1,822 input
tokens completed normally with nothing cached.

A change invalidates the cache from its position in the prefix onward:

| Change within a run | Cache invalidated |
|---|---|
| Tool list: a tool added, removed or reordered, or a schema changed | everything |
| Model: a router selects a different model for a turn | everything; each model keeps its own cache |
| System prompt | system and messages |
| Effort, thinking, or `tool_choice` (the missed-tool escalation forces one for a single turn) | messages (documented, not probed) |
| Context-strategy compaction | messages, on every turn after the first overflow |

Once the history first overflows the context budget, every later turn
misses the message cache under all three strategies: sliding-window
drops more of the head on each turn, summarise re-summarises, and
offload rewrites tool results as they age. Tools and system stay cached
through the system breakpoint. A long `run_command` or a blocking
sub-agent spawn can outlast the five-minute cache lifetime, so the
parent's next turn writes its whole prefix to the cache again at 1.25x.

Automatic caching looks back 20 content-block positions for a previous
write, counting a run of consecutive `tool_use` blocks, or of
`tool_result` blocks, as one position, so a turn of parallel tool calls
does not push the previous write out of reach.

The `anthropic prompt cache` Debug log line reports `cache.read`,
`cache.write` and `input.uncached` for every stream that reports input
usage, from the same figures that reach the trace as `cacheRead` and
`cacheWrite` (see [Token usage](trace-inspection.md#token-usage)). A
turn after the first whose `cache.read` is zero points at one of the
invalidations above. The Bedrock adapter sends no cache breakpoints,
and the legacy Bedrock surface does not offer automatic caching.

**Refusals and other stop reasons.** A response the model declines
ends with `stop_reason: "refusal"`, which becomes the run outcome
verbatim. The adapter reads the `stop_details` object on the streamed
`message_delta`: `type` (`"refusal"`), `category`, and `explanation`.
`category` keeps the documented values (`cyber`, `bio`,
`frontier_llm`, `reasoning_extraction`, `general_harms`), reports any
other non-empty value as `other`, and is empty when the API sends
null. `type` is capped at 64 bytes and `explanation` at 1 KiB, and a
malformed field is dropped without discarding the others.

The loop records the details on the turn's `TurnTrace.stopDetails`
and, when the refusal ends the run, on `RunTrace.stopDetails`. It logs
`provider refused to respond` at Warn with `category` and
`explanation`, and sets `stop.category` on the `provider.stream` span.
The explanation is scrubbed of secret-shaped content before it reaches
a trace or log. `RunTrace.stopDetails` is omitted when the run's
final outcome is not the stop that produced them, for example when a
cancellation observed after the refusal becomes the outcome. Operators
read the details from the trace emitters (the JSONL and GCS
`RunTrace`, and the OTel span attribute) and from the Warn log line.
The gRPC `RunTrace.stop_details` field mirrors them, but `stirrup job`
leaves `done.trace` unset
([issue #453](https://github.com/rxbynerd/stirrup/issues/453)).

`model_context_window_exceeded` (the response filled the model's
context window and is truncated) is also returned verbatim as the
outcome. The harness neither retries nor continues after either stop
reason, and tool-choice escalation does not re-prompt after them.
Refusals cannot be triggered benignly, so the `stop_details` shape is
documented, not probed.

**Stream errors.** A failure after streaming has begun arrives as an
SSE `error` event, for example `overloaded_error`. The adapter reports
it as `anthropic API stream error (<type>): <message>`, so the run
ends with outcome `error` and the `provider stream failed` log line and
transport `warning` carry the provider's error type. Like every
failure after the stream opens, it is not retried.

## AWS Bedrock

**File:** `harness/internal/provider/bedrock.go`

AWS ConverseStream API via `aws-sdk-go-v2`. Translates between the
harness's internal `Message` / `ToolCall` types and Bedrock's union-type
wire format. Auth is IAM (not API key); `config.LoadDefaultConfig()`
resolves credentials from the SDK default chain. Accepts an optional
`aws.CredentialsProvider` for cross-cloud credential federation (e.g.
`WebIdentityAWSSource` exchanging a GKE OIDC token for STS credentials).

Token usage arrives on the Converse `metadata` event. The adapter
reports input as `totalTokens` − `outputTokens` when `totalTokens` is
present, which counts the whole prompt whether or not `inputTokens`
includes the cache figures, and otherwise as `inputTokens` plus
`cacheReadInputTokens` and `cacheWriteInputTokens`. Neither shape has
been probed against Bedrock.

The Bedrock adapter does not consult the [provider quirks
registry](provider-quirks.md): it forwards the harness default
temperature to every model and ignores `reasoningEffort`. Anthropic
documents that Claude Opus 4.7 onward, Sonnet 5 onward and Fable 5
onward reject a non-default temperature on every request, so those
models are expected to fail through Bedrock until the adapter applies
the same rules; this has not been verified against Bedrock itself.

## OpenAI Chat Completions

**File:** `harness/internal/provider/openai.go`

OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure
OpenAI, vLLM, Ollama, and LM Studio via configurable `baseURL`. Key
configuration knobs:

- `provider.apiKeyHeader`: header name for the API key. Empty (default)
  sends `Authorization: Bearer`. Set to `api-key` for Azure OpenAI key
  auth.
- `provider.queryParams`: appended to every request URL. Use for Azure
  api-version pins (`api-version=preview`) or gateway-specific params.

Azure Entra ID bearer tokens work with the default empty `apiKeyHeader`
— the `Authorization: Bearer` header carries the Entra token normally.
See `examples/runconfig/azure-openai.json`.

### Local models via LM Studio (Qwen 3.6)

LM Studio exposes the Chat Completions wire format at
`http://<host>:1234/v1`, so a locally-hosted model runs through the
stock adapter with `provider.type: "openai-compatible"` and
`provider.baseUrl` pointed at the server. LM Studio ignores the API key
but the adapter requires a non-empty credential, so supply any
placeholder `apiKeyRef` (e.g. `secret://LMSTUDIO_API_KEY` with the env
var set to any value). See
[`examples/runconfig/qwen3.6-lmstudio.json`](../examples/runconfig/qwen3.6-lmstudio.json).

Qwen 3.6 is a default-thinking model, and the integration was validated
end-to-end against LM Studio. The operationally relevant behaviours:

- **No wire-shape quirks are required.** Unlike DeepSeek v4 and Z.ai
  GLM, Qwen 3.6 does not return HTTP 400 when prior-turn reasoning is
  omitted, accepts the modern `max_completion_tokens` key, and welcomes
  sampling parameters. It needs no `compatProfile` and no replay rule —
  the chain-of-thought LM Studio surfaces in a `reasoning_content` field
  is dropped between turns by design, matching Qwen's own template.
- **Thinking is always on.** The `/no_think` directive and an
  `enable_thinking` request parameter are not honoured through the plain
  Chat Completions surface, so every turn spends reasoning tokens. The
  default per-turn response budget is generous, so this is a latency and
  cost consideration, not a correctness one.
- **Set `temperature` to 0.6.** Qwen recommends 0.6 for thinking-mode
  coding and warns against greedy decoding (`temperature: 0`), which can
  trigger repetition. The harness default is 0.1; the example raises it.
- **`contextStrategy.maxTokens` scales its response reserve to fit
  small windows.** LM Studio loads models with a conservative context
  window that is usually far below the model's native ceiling. Above
  64k, the harness reserves a flat 64k tokens for the response, same as
  always. At or below 64k, the reserve scales down to a quarter of
  `contextStrategy.maxTokens` instead of staying flat — a fixed 64k
  reserve on a small window used to leave negative room for the prompt,
  truncating to the last two messages every turn and surfacing as an
  `empty stop reason` error; the scaled reserve keeps a real, usable
  prompt budget regardless of window size. The harness logs a WARN and
  emits a transport `warning` event on the run's first turn whenever
  this scaling kicks in, naming the configured `maxTokens` and the
  resulting reserve, so a run against an unexpectedly small LM Studio
  window is diagnosable rather than silent. Setting
  `contextStrategy.maxTokens` well above 64k (the example uses 131072)
  and configuring the LM Studio context window to hold at least that
  much remains the best choice when the model's native context
  supports it — a larger prompt budget is strictly more useful than a
  scaled-down one — but is no longer required for small-context local
  deployments to work.
- **Vendor-prefixed model ids.** LM Studio serves some models under
  `vendor/model` ids (e.g. `qwen/qwen3.6-27b`). The quirks registry
  advertises the openai-compatible tool surface for one level of prefix,
  so native `tool_choice` and `parallel_tool_calls` work for these ids
  just as they do for bare ids.

## OpenAI Responses API

**File:** `harness/internal/provider/openai_responses.go`

Targets the Responses API (`POST /v1/responses`) — a distinct wire
format from Chat Completions:

- Top-level `instructions` field (not a system message in the array).
- Typed `input[]` items: `message`, `function_call`, `function_call_output`.
- Flat tool schema.
- `max_output_tokens` (not `max_tokens`).
- Explicit `store: false`.
- Named SSE events: `response.output_text.delta`,
  `response.function_call_arguments.delta`, `response.completed`,
  `response.incomplete`, `response.failed`.

Selected explicitly via `provider.type: "openai-responses"`. There is
**no auto-detection** between the two OpenAI adapters; silent fallback
would mask configuration errors.

**Intentional exclusions:** OpenAI built-in tools (`web_search`,
`file_search`, `computer_use`, `code_interpreter`) and server-side state
via `previous_response_id`. The harness manages its own conversation
history and does not delegate to server-side state; reasoning items are
not replayed between turns. `RunConfig.reasoningEffort` maps to
`reasoning.effort` for models whose accepted levels are known (the
GPT-6 family).

**Prompt caching.** The Responses API caches automatically, with no
breakpoint in the request: the cached prefix covers the tools,
`instructions` and the input history, and GPT-5.6 and later need a
prefix of at least 1,024 tokens. The adapter adds a per-run
`prompt_cache_key` (the `PromptCacheKey`
[quirk](provider-quirks.md#21-providerquirks)): the first 32 hex
characters of the SHA-256 of the run ID, so it is stable across the
run's turns and distinct between runs and sub-agents. It does not send
the run ID, but the digest is not an anonymiser: run IDs are not
secret, and anyone holding a candidate run ID can recompute the key.
The key is only as unique as the run ID, so control planes should keep
run IDs unique per credential; a collision affects routing affinity
only. On models before GPT-5.6 a stable key routes related
requests to the same cache, and OpenAI suggests keeping each key to
about 15 requests per minute; on GPT-5.6 and later routing is automatic
and the key only keeps cache accounting separate per run. Summariser,
LLM-judge and guard calls send no key. The Chat Completions adapter
never sends one, since compatible servers may reject the field. This
behaviour is documented, not probed.

OpenAI documents these request changes as breaking the cached prefix:
`model`, the tool list (names, descriptions, schemas or order),
`parallel_tool_calls`, `text.format`, `reasoning.effort`,
`text.verbosity`, `context_management`, and any rewrite of earlier
input, which includes a context-strategy summarise, offload or
sliding-window drop. The `cacheRead` and `cacheWrite` trace fields
carry `input_tokens_details.cached_tokens` and `.cache_write_tokens`
(see [Token usage](trace-inspection.md#token-usage)).

**GPT-6.** GPT-6 Astra and GPT-6.1 Sol call tools only through this
API, and GPT-6 Sol and Luna only at `reasoning_effort: "none"` on Chat
Completions, which the harness never sends. Agentic runs on a
first-party `gpt-6*` id therefore need `provider.type:
"openai-responses"`; the Chat Completions adapter rejects such a run
before sending, with an error naming this provider type.

Azure Foundry's `/openai/v1/responses` endpoint is wire-compatible:
point `provider.baseUrl` at the Azure resource, set
`provider.apiKeyHeader: "api-key"` for key auth (or leave empty for
Entra ID Bearer), and add `provider.queryParams: {"api-version":
"preview"}`. Azure-only Responses extensions ride the existing
forward-compatible "unknown SSE event" path and are silently ignored.
See `examples/runconfig/azure-openai.json`.

**Error codes.** The adapter appends OpenAI's `error.code` to every
error it reports (the HTTP error path, `response.failed`, and the SSE
`error` event) as `<message> (code: <code>)`, and keeps the
message-only form when no code is present. A numeric code, as some
gateways send, is rendered as its number. The code, not the message
text, identifies the failure. `misalignment_policy_violation` means
misalignment monitoring stopped the conversation: it arrives as HTTP
403 before streaming, or as a stream error after output has begun, and
must not be retried.

A 429 reporting an exhausted billing, spend, or quota limit is not
retried; the full classification is in
[`configuration.md`](configuration.md#retry-policy). A failure after
the stream opens is never retried. These codes are documented in
OpenAI's error-codes and misalignment-monitoring guides; they have not
been probed against the live API.

## Google Gemini via Vertex AI

**File:** `harness/internal/provider/gemini.go`

Vertex AI `:streamGenerateContent` with `?alt=sse`. SSE-framed,
hand-rolled HTTP. Auth is GCP IAM (OAuth2 Bearer tokens) — **never** an
AI Studio API key.

Key implementation notes:

- **ADC only in production.** Application Default Credentials are the
  default; user-mode `gcloud` credentials are explicitly rejected
  (autonomy invariant — a personal `gcloud` login must not drive
  production workloads).
- **Tool-call ID synthesis.** Vertex does not echo IDs through
  `functionResponse`, so the adapter synthesises them:
  `gemini-{streamN}-{partIdx}`.
- **`finishReason: STOP` remapping.** Vertex uses STOP for both
  end-of-turn and tool-dispatch turns. The adapter remaps STOP to
  `tool_use` whenever the same stream emitted at least one
  `functionCall` part.
- **Safety thresholds.** Defaults to `BLOCK_NONE` for all five
  categories. A coding harness that produces security tooling cannot
  tolerate false positives on legitimate code samples, so BLOCK_NONE
  is the only sane default; operators requiring stricter behaviour
  override via `provider.geminiSafetySettings`.
- **Request/schema translation.** JSON Schema → Gemini OpenAPI Schema
  conversion: `provider/gemini_schema.go`. Request assembly:
  `provider/gemini_request.go`.
- **Role mapping.** A user message with text becomes one
  `{role:"user"}` Content; a user message with `tool_result` blocks
  emits a separate Content per result (Vertex does not allow user-text
  and function-response parts to share a Content). The role on those
  function-response Contents is model-dependent and comes from the
  resolved quirks, not a literal: `function` through Gemini 3.5, and
  `user` from 3.6 on. Vertex AI still accepts `function` for those
  newer families; the AI Studio surface that shares this request
  schema returns an HTTP 400 for it from 3.6 on, and `user` is
  accepted everywhere, so the newer families use the shape that
  survives Vertex adopting the stricter validator. An assistant
  message collapses
  into one `{role:"model"}` Content preserving block order. When a
  user message has both text and `tool_result` blocks, the
  function-response Contents are emitted first, mirroring the OpenAI
  Responses adapter's ordering — otherwise Vertex would receive a
  user-text turn before the function-response it depends on.
- **Thinking level.** The provider-neutral `reasoningEffort` config
  field projects to `generationConfig.thinkingConfig.thinkingLevel`.
  Left empty, nothing is sent and the model applies its own default.
  Accepted levels vary by model — Gemini 3.7 Flash rejects `minimal`,
  which 3.6 Flash accepts — so the quirks registry carries a per-model
  allow-list and the adapter fails the request before any wire bytes
  are sent. See [`provider-quirks.md`](provider-quirks.md).
- **Output tokens include thought tokens.** Vertex reports
  `thoughtsTokenCount` beside `candidatesTokenCount`, so the adapter
  reports their sum as output tokens, matching billing, and the
  thought count as reasoning tokens.

**Intentional exclusions:** multimodal input, server-side built-in
tools (`google_search`, `code_execution`, etc. — tracked as issue #93),
AI Studio direct support.

### Schema translation (`provider/gemini_schema.go`)

`ConvertSchema` converts a JSON Schema (Draft 2020-12) document into
the Gemini OpenAPI-3.0-flavoured Schema dialect used in
`tools[].functionDeclarations[].parameters`. It is pure (no I/O, no
globals) and returns an error rather than silently dropping fields
when it hits an unsupported keyword, so the adapter's `Stream` call
fails fast at request-build time instead of sending a schema Gemini
will reject.

Supported transformations:

- JSON Schema lowercase type names → Gemini UPPERCASE.
- Type arrays of the form `["X","null"]` → `nullable: true` with a
  single type.
- Recursive descent into `properties` and `items`.
- Pass-through of validation keywords (`description`, `enum`,
  `required`, etc.).
- Drop of metadata keywords (`$schema`, `$id`, `$defs`,
  `definitions`, `$comment`, `additionalProperties`).

Hard errors:

- `$ref` — Gemini does not resolve refs; the caller must inline the
  referenced schema.
- `oneOf` / `anyOf` with more than one non-null branch (Gemini Schema
  has no discriminated-union support).
- `allOf` — no merge logic.
- Type values outside the Gemini type table.
- Type arrays with more than two values, or two values where neither
  is `"null"`.

Empty input (`""` or `"{}"`) returns an empty object schema. Unknown
keywords are passed through verbatim — Vertex tolerates them, and
silently dropping them would mask future schema features.

### Configuration

| Field | Default | Notes |
|---|---|---|
| `provider.gcpProject` | (none) | GCP project hosting the Vertex AI usage. Required when `--provider=gemini`. |
| `provider.gcpLocation` | `global` | Vertex AI location: `global` or a region like `us-central1`. |
| `provider.gcpCredentialsFile` | (none) | Path to a service account JSON key. When set, implies `gcp-service-account`. Otherwise falls back to ADC. |
| `provider.credential.type` | inferred | `gcp-default` (ADC), `gcp-service-account` (key file), or `gcp-workload-identity` (GKE/GCE metadata). |

See `examples/runconfig/vertex-gemini.json` and
`examples/runconfig/vertex-gemini-wif.json`.

## Per-model wire-shape quirks

Provider/model pairs sometimes diverge from the adapter's canonical
wire shape: OpenAI's reasoning-class models reject sampling
parameters, the newest Claude tier (Opus 4.7+, Sonnet 5+, Fable 5+ /
Mythos 5) rejects a non-default temperature the same way, Z.ai GLM
requires the legacy `max_tokens` key, Gemini 3.x emits a
`thoughtSignature` blob that must survive turn boundaries, and
DeepSeek v4's default-on thinking mode requires the
`reasoning_content` it streams replayed back for every prior assistant
turn once tools are present (the API returns 400 otherwise). Rather than
encoding these as adapter-internal model substring checks, the
harness routes them through a registry-driven quirks layer at
`harness/internal/provider/quirks/`. DeepSeek v4 runs through the
stock Chat Completions adapter (`provider.type:
"openai-compatible"` with `provider.baseUrl:
"https://api.deepseek.com"`); the built-in `deepseek-v4*`,
`deepseek-flash*` (V4.1 Flash) and gateway-prefixed rules supply the
replay threading, sampling suppression, and legacy token key with no
operator configuration.

Operators do not author quirk rules. Two surfaces are available:

- `provider.compatProfile` on `ProviderConfig` — a closed enum that
  selects from a small set of compatibility profiles. Only legal
  value in v1: `"zai-glm"`, which loads the Z.ai GLM compat rule
  (legacy `max_tokens` key and the `tool_stream: true` extension).
  Unknown values fail at startup via `ValidateRunConfig`.
- `stirrup providers quirks --provider X --model Y` — introspection
  subcommand that prints the resolved `ProviderQuirks` value plus
  every contributing rule's description, last-verified date, and
  staleness flag as JSON. Side-effect-free.

Full reference: [`provider-quirks.md`](provider-quirks.md).

## Credential federation

All five providers consume credentials through `credential.Source.Resolve()`,
which returns a `Resolved` value with either a static secret or a
`BearerToken` closure. Adapters call the closure on every provider request
so short-lived tokens are refreshed without restarting the run.

The token-source abstraction (`TokenSource`) is reusable across targets:
the same EKS IRSA projected token can be exchanged for AWS credentials,
GCP credentials (via WIF), an Anthropic service account token, or an
OpenAI access token. The `openai-compatible` and `openai-responses`
providers reach the OpenAI API keyless via OpenAI Workload Identity
Federation — see [`docs/openai-wif.md`](openai-wif.md).

Full reference: [`docs/credential-federation.md`](credential-federation.md).
