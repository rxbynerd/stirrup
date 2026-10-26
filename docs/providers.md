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
them.

### Thinking-block replay

Claude models that think by default stream `thinking` blocks alongside
their text and tool calls. The API also documents `redacted_thinking`
blocks, which the adapter handles but which have not been observed
live. The harness sends no `thinking` request field and no beta header,
so each model's default thinking behaviour applies. On
`claude-sonnet-5-5` the reasoning text arrives empty by default and the
block carries only an opaque `signature`.

The adapter captures each block when it closes, and the loop persists
it in the assistant message in stream order: a `thinking` block keeps
its signature in `ContentBlock.ThoughtSignature` and any reasoning text
in `Text`; a `redacted_thinking` block keeps its opaque `data` in
`ThoughtSignature`. The next request replays the blocks verbatim, so the
model keeps its reasoning state across tool-use turns. A block without a
signature cannot be verified by the API and is dropped on egress; an
assistant message left empty by that drop is omitted, and the turns on
either side are joined when they share a role. A response fails when
its thinking blocks exceed 10 MiB in total (reasoning text, signatures
and redacted data counted together) or when it opens more than 64
content blocks at once. Batch results are parsed into the same shape.

While a block streams, the adapter also forwards each fragment of its
reasoning text as it arrives, which the loop sends to the control
plane as `thinking_delta` events after its secret carry (see
[Control-stream scrubbing](security.md#control-stream-scrubbing)); the complete block remains the only
thing persisted. A block whose reasoning text is empty, the
`claude-sonnet-5-5` default, produces no `thinking_delta`. See
[Watching a run live](integration-guide.md#watching-a-run-live).

Replayed thinking is billed as input. In one probe against
`claude-sonnet-5-5` on 2026-09-30, a continuation cost 668 input tokens
with the prior turn's thinking block replayed and 558 without it, a
difference equal to that turn's 110 thinking tokens. The token budget
counts the provider-reported input figure, which includes replayed
thinking. The loop's context-size estimate and the sliding-window
strategy's per-message estimate count reasoning text only, not
signature bytes, so hidden thinking (a block with empty text, the
`claude-sonnet-5-5` default) is under-counted by both estimates; a
compaction can therefore trigger later than the provider's own input
count would suggest.

Anthropic's [preserved-thinking
documentation](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking)
binds a thinking block to the model that produced it and to the exact
history that preceded it. The API enforces the history check by
default only for accounts created on or after 2026-08-31 00:00 UTC;
older accounts opt in per request, and the check was not reproduced on
the account used for probing. Once the context strategy rewrites
history on any turn of a run (a sliding-window trim, a summary, or an
offload), the loop removes every thinking block from that request and
from every later request in the run; the stored history keeps them.
From that point the model continues from the visible transcript alone.
Under sliding-window this starts the first time the budget is
exceeded, and a trim can also separate a `tool_result` from its
`tool_use`, which the API rejects
([#624](https://github.com/rxbynerd/stirrup/issues/624)).

JSONL traces and recordings keep each thinking block's type and its
reasoning text, scrubbed for secrets exactly like assistant text: both
are model output of the same trust class. Signatures and redacted data
never persist, even with `--debug`. OTel content capture omits thinking
blocks, and the GCS trace emitter persists no turn content. Thinking
blocks never reach another provider: the other adapters build their
requests from wire types with no thinking block (see the
[cross-provider confidentiality
invariant](architecture.md#provider-adapters)).

Known limitations:

- Dynamic routing or a fallback can replay a block to a Claude model
  other than the one that produced it, and the API drops such a block
  (documented, not probed).
- Only the `anthropic` adapter replays thinking, and it always sends to
  the Anthropic Messages API, so a run that switches provider loses
  reasoning continuity on the other provider's turns without exposing
  the blocks to it.
- Within one response the harness re-encodes tool input with sorted
  keys and joins adjacent text, so a thinking block that follows text
  or a tool call in the same response would replay after a prefix the
  model did not produce; models that think only before their first
  output block never emit that order, but `between_tools` thinking can.

### Tool input examples

A tool's worked examples (`ToolPresentation.InputExamples`) are sent on
the tool definition's native `input_examples` field, and `input_schema`
is left untouched. The quirk rule matches every Claude model. The field
was accepted without a beta header on `claude-opus-5-5` and
`claude-haiku-4-5` in a direct probe, and through the harness on
`claude-sonnet-5-5`, `claude-sonnet-4-6` and `claude-haiku-4-5`, on
2026-09-30. Older Claude families are covered by the rule on the
strength of Anthropic's documentation, not a probe. The API requires
each example to validate against the tool's input schema, so every
built-in and edit-strategy example is tested against its own schema.

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
  the next. Only main-loop turns send it, identified by carrying the cache
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
The gRPC `RunTrace.stop_details` field on `done.trace` mirrors them.

`model_context_window_exceeded` (the response filled the model's
context window and is truncated) instead makes the harness compact the
history and retry the turn once; a second overflow ends the run as
`context_exhausted`. The harness neither retries nor continues after a
refusal, and tool-choice escalation does not re-prompt after either.
Refusals cannot be triggered benignly, so the `stop_details` shape is
documented, not probed.

**Stream errors.** A failure after streaming has begun arrives as an
SSE `error` event, for example `overloaded_error`. The adapter reports
it as `anthropic API stream error (<type>): <message>`, so the run
ends with outcome `error` and the `provider stream failed` log line and
transport `warning` carry the provider's error type. Like every
failure after the stream opens, it is not retried.

### Tool search

Under [lazy tool loading](configuration.md#lazy-tool-loading) with the
`auto` strategy, the Claude models listed in the configuration
[model table](configuration.md#lazy-tool-loading) use Anthropic's
server-side tool search
([`ToolSearch` quirk](provider-quirks.md#3-wave-2-rules-builtinrules))
instead of the harness `tool_search` tool. The feature is generally
available and needs no beta header; the wire shape below is taken from
Anthropic's documentation and is not probed.

The adapter adds the search tool only when the request carries
`ToolSearch`, the resolved model's `ToolSearch` capability is supported,
and at least one tool is deferred. When `ToolSearch` is requested for a
model without the capability, the adapter logs the downgrade at Debug
and sends every tool loaded. The search tool goes first in `tools` and is
never deferred, which satisfies the API's rule that not every tool may carry
`defer_loading`. Every deferred definition is sent in full with
`"defer_loading": true`, on every turn, loaded or not, so the tool
list stays byte-stable for the prompt cache:

```json
"tools": [
  {"type": "tool_search_tool_bm25_20251119", "name": "tool_search_tool_bm25"},
  {"name": "read_file", "description": "…", "input_schema": {…}},
  {"name": "mcp_github_search_issues", "description": "…", "input_schema": {…}, "defer_loading": true}
]
```

The model's search streams as a `server_tool_use` block whose input
arrives as `input_json_delta`; the result arrives whole on its
`content_block_start`:

```text
event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"tool_search_tool_bm25"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"github issues\"}"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_01","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"mcp_github_search_issues"}]}}}
```

The adapter emits a `tool_search` stream event when the call block
closes and a `tool_search_result` event carrying the referenced tool
names when the result block closes. The loop records both in the
assistant message, in stream order, and marks the referenced tools
loaded. An empty `tool_references` array means the search matched
nothing. A failed search carries `tool_search_tool_result_error` with
an `error_code` (`invalid_tool_input`, `unavailable`,
`too_many_requests`, or `execution_time_exceeded`) and an
`error_message`; the adapter reports both, marks the event as failed,
and the stream continues. The recorded `tool_search_result` block of a
failed search holds the error code in `name` and the message in
`content`, with `is_error` set. A result in any other shape loads
nothing and is logged at Warn, because it leaves the loaded set out of
step with the provider. The harness never sends a `tool_result` for a
`srvtoolu_` call: the search runs on Anthropic's side and the turn's
`stop_reason` is unaffected.

On the next request both blocks are replayed in place, rebuilt in the
exact shape the API returned, so the API keeps the referenced
definitions expanded for the rest of the run. A failed search replays
as a `tool_search_tool_result_error` with the recorded code and
message, and with the code `unavailable` when none was recorded, since
the API rejects an empty code. Only a call naming an Anthropic search
tool (`tool_search_tool_bm25` or `tool_search_tool_regex`) and answered
by a result in the same message is replayed; a search recorded from
another provider, such as the OpenAI hosted `tool_search`, is dropped
together with its result. When a request does not carry the search tool
(the run routed this turn to a model without the capability, or nothing is
deferred), every recorded search block is dropped and every tool is
sent loaded; a message left empty by the drop is omitted.

Batch turns build their request through the same code, so the native
search is sent in a batch turn exactly as in a streamed one, and the
batch response is decoded into the same events and blocks. The batch
path is not probed against the live batch endpoint.

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
temperature to every model and ignores `reasoningEffort`. It streams
no reasoning text, so `reasoningSummary` has no effect on it. Anthropic
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

**Reasoning text.** Servers that stream reasoning in a non-standard
`reasoning_content` delta field (DeepSeek, Z.ai GLM, LM Studio), or in
the `reasoning` field that OpenRouter uses, have
each fragment forwarded, through the loop's secret carry (see
[Control-stream scrubbing](security.md#control-stream-scrubbing)), as
`thinking_delta` events, whether or not a replay rule also captures it
for the next turn. Chat Completions has no
request control for this, so `reasoningSummary` changes nothing on the
wire; `off` only stops the loop forwarding the events. When a delta
carries both fields, a non-empty `reasoning_content` is used and
`reasoning` is ignored. A value that is not a JSON string is ignored.
Only `reasoning_content` is ever captured for replay; OpenRouter does
not require reasoning to be sent back (see
[provider quirks](provider-quirks.md#9-open-risks)).

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
  is dropped between turns by design, matching Qwen's own template. It
  still streams to the transport as `thinking_delta` events unless
  `reasoningSummary` is `off`.
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
- Typed `input[]` items: `message`, `function_call`, `function_call_output`,
  replayed `reasoning` items, and `tool_search_call` /
  `tool_search_output` items under [hosted tool search](#hosted-tool-search).
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
`file_search`, `computer_use`, `code_interpreter`; hosted `tool_search`
is the one exception, sent only under lazy tool loading) and server-side state
via `previous_response_id`. The harness manages its own conversation
history and does not delegate to server-side state.

**Output replay.** With `store: false`, OpenAI documents that stateless
reasoning models need every prior response output item resent. For the
reasoning families (`o[1-9]*`, `gpt-5*` except `gpt-5-chat*`, and
`gpt-6*`), the adapter captures each turn's `reasoning`, `message`,
`function_call`, `tool_search_call`, and `tool_search_output` output
items and replays them, as semantically identical
JSON, on later requests to the same model and endpoint, so reasoning
items (with `encrypted_content`), item ids, `status`, and the assistant
`phase` reach the model. Replay is all-or-nothing per turn: a turn whose
stored items no longer match its persisted content (a rewritten tool
call or text), that came from another model or endpoint, or whose
capture was disabled (an incomplete turn, an unknown item type, a
reasoning item without `encrypted_content`, or more than 1 MiB of
items) is reconstructed without item ids instead. The size of
`encrypted_content` is unmeasured, so the 1 MiB cap is a guard, not a
measurement. Batch requests never capture output items, so a turn
answered through the batch path is always reconstructed. The mechanism
is described in
[`provider-quirks.md` §3.1](provider-quirks.md#31-replayfields-rules).
`reasoning.context` is not sent, so the model default applies
(`all_turns` on GPT-5.6); `current_turn` is the relief valve if
replayed reasoning grows the context too far.

Requests for the same reasoning families carry
`include: ["reasoning.encrypted_content"]` so the encrypted reasoning
is returned for replay. First-party OpenAI documents the include as
optional; it stays protective on Azure and gateways. Non-reasoning
models (`gpt-4o*`, `gpt-4.1*`, `gpt-5-chat*`) receive neither the
include nor replayed items, and the adapter captures nothing for them,
so every turn is reconstructed without item ids. Whether those models
reject the include, or replayed `message` item ids and `status`, with
HTTP 400 is unverified; a probe against a non-reasoning model should
check both before either gate is widened.

**Reasoning effort and sampling.** `RunConfig.reasoningEffort` maps to
`reasoning.effort` for models whose accepted levels are known: GPT-6
and GPT-5.6 (`low`..`max`), GPT-5.4 and GPT-5.5 (`low`..`xhigh`). A
request that sends `reasoning.effort` omits `temperature`. GPT-6 never
receives `temperature`; GPT-5.5 and GPT-5.6 never receive it either,
which is inferred from their `medium` default effort. GPT-5.4 keeps a
configured temperature when no effort is set, because its default
effort is `none`.

**Reasoning summaries.** When `reasoningSummary` is explicitly `auto`,
a request that carries `reasoning.effort` also carries
`reasoning.summary: "auto"`, and each
`response.reasoning_summary_text.delta` event streams as a
`thinking_delta`. The first delta of each later summary part starts
with a blank line, so parts do not run together, and a malformed delta
is dropped rather than failing the turn. With no projected effort the
request has no `reasoning` object, so no summary is requested. The
reasoning output item, with its `summary` array, is stored and
replayed verbatim like any other item. This follows OpenAI's
documented shapes and has not been probed live. OpenAI's
[reasoning guide](https://developers.openai.com/api/docs/guides/reasoning)
says organisation verification may be needed before summarizers work on
its latest reasoning models, so an unset `reasoningSummary` requests no
summary on this adapter and the request is the same as with `off`.

**Strict tools.** The reasoning families send every tool with
`strict: true`, because an omitted `strict` lets the API fall back to
non-strict silently. A tool schema the strict rewriter cannot express
(`$ref`, `oneOf`, `anyOf`, `allOf`, `patternProperties`, tuple
`items`; common in MCP-imported tools) is sent with its original schema
and an explicit `strict: false`, and the adapter logs one WARN for it.
An MCP server that declares an optional parameter as an `anyOf` with
`null`, as FastMCP and Pydantic do, therefore loses constrained
decoding for that one tool while every other tool stays strict. The
harness still validates every tool input against the schema before
dispatch.

The replay, include, GPT-5.x effort, and strict behaviour is
documented, not probed: no live request against an OpenAI endpoint has
exercised it.

**Prompt caching.** The Responses API caches automatically, with no
breakpoint in the request: the cached prefix covers the tools,
`instructions` and the input history, and GPT-5.6 and later need a
prefix of at least 1,024 tokens. The adapter adds a
`prompt_cache_key` (the `PromptCacheKey`
[quirk](provider-quirks.md#21-providerquirks)): the first 32 hex
characters of the SHA-256 of a run ID, so it is stable across the
run's turns and distinct between runs and sub-agents, except that a
follow-up run continuing the previous run's history
(`followUpHistory: "continue"`) reuses that run's key, since it resends
the same prefix. A chain of continued follow-ups therefore sends the key
derived from the run ID that began the chain, normally the primary
run's. A follow-up starting from its prompt alone, a `reset` follow-up,
a sub-agent and a direct `Run` caller each use the digest of their own
run ID. The adapter does not send the run ID, but the digest is not an
anonymiser: run IDs are not secret, and anyone holding a candidate run
ID can recompute the key. The key is only as unique as the run ID, so
control planes should keep run IDs unique per credential; a collision
affects routing affinity only. On models before GPT-5.6 a stable key
routes related requests to the same cache, and OpenAI suggests keeping
each key to about 15 requests per minute; on GPT-5.6 and later routing
is automatic and the key only keeps cache accounting separate per key,
so a chain of continued follow-ups shares one accounting group and
every other run has its own. Summariser,
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

### Hosted tool search

Under [lazy tool loading](configuration.md#lazy-tool-loading) with the
`auto` strategy, `gpt-5.4` and later 5.x models and the `gpt-6*` family
use OpenAI's hosted
[tool search](https://developers.openai.com/api/docs/guides/tools-tool-search)
on the default endpoint only. Only `https://api.openai.com/v1`, with or
without a trailing slash, counts as the default endpoint. A provider with
any other `baseUrl` (Azure OpenAI deployments, LiteLLM, LM Studio, other
gateways, and regional first-party hosts) uses the harness's own
`tool_search` tool. A gateway can answer 200 while silently
dropping the hosted search entry and the `defer_loading` flags, which
leaves every deferred tool unloadable while the run still ends in
success. The run logs the fallback once at startup. The adapter sends it
only when the loop asks for a native search, the resolved quirks
advertise the `ToolSearch` capability, and at least one tool in the
request is deferred. Every other model, every request the loop serves with
the harness's own `tool_search` tool, and every request with nothing
deferred gets no search entry and no `defer_loading`.

The request lists every function tool as usual, adds
`"defer_loading": true` to each deferred one, groups the deferred tools
of each configured MCP server, and the deferred control-plane tools,
into one `namespace` entry per group, and appends the hosted search
entry last, with no field other than its type. On the strict families
`defer_loading` sits beside `strict: true`:

```json
"tools": [
  {"type": "function", "name": "read_file", "description": "Read a file", "parameters": {...}, "strict": true},
  {"type": "namespace", "name": "mcp_jira", "description": "Tools from MCP server \"jira\".", "tools": [
    {"type": "function", "name": "mcp_jira_create_issue", "description": "Create a Jira issue", "parameters": {...}, "strict": true, "defer_loading": true}
  ]},
  {"type": "function", "name": "list_directory", "description": "List a directory", "parameters": {...}, "strict": true, "defer_loading": true},
  {"type": "tool_search"}
]
```

An MCP server's namespace is named `mcp_<server>` and described as
`Tools from MCP server "<server>".`; control-plane tools share the
`control_plane` namespace, described as `Tools provided by the control
plane.` Both come from operator configuration, never from text an MCP
server or the control plane supplies, and namespace names are normalised
to the provider's tool-name rules like tool names. A namespace sits where
its first member would have been. Members keep their full names (such
as `mcp_jira_create_issue`), so dispatch and permissions see the same
names as without namespaces. Until a search loads a namespace, the model
sees only its name and description.

Deferred built-in tools, and MCP tools whose name no configured server
claims, are sent as flat functions. The model still sees each flat
deferred function's name and description from the first turn; only its
parameter schema is deferred. A namespace whose name equals a
function's name is not formed, and its members are sent flat. Operators
who need every deferred tool hidden set
`tools.lazyLoading.strategy: "harness"`, which keeps the names and
descriptions out of the request until a search loads them.

A search arrives as two output items ahead of the function call that
uses a loaded tool: a `tool_search_call`, whose `arguments` is a JSON
object (a `function_call` carries a string), and a `tool_search_output`,
whose `tools` array holds the loaded definitions. The adapter emits them
as a `tool_search` event (ID is the call item's `id`, because hosted
items carry `"call_id": null`; a call without an `id` gets
`tool_search_<output_index>`) and a `tool_search_result` event paired
with the earliest unanswered call. The result names every function in
`tools`, including functions inside a `namespace` entry (a namespace
nested inside another is ignored); other entry types are ignored. The
search's `arguments` names what it loads, as `{"paths": [...]}` holding
namespace or function names. Every `function_call` carries a
`namespace` field: the namespace's name for a namespace member, or the
function's own name for a flat function. The adapter dispatches on
`name` and ignores `namespace` on read. A malformed search item produces
an event with no input or no loaded tools rather than failing the turn. OpenAI documents no failure
shape for hosted search, so the adapter never reports a failed search.

**Replay.** On the reasoning families both items are captured with the
turn's other output items and replay verbatim, in place. They take no
part in the check that the stored items still match the persisted
message, so a search never forces reconstruction. They replay only
to a request that carries the hosted search entry; otherwise the turn
is reconstructed without them. A reconstructed turn rebuilds each search
as a `tool_search_call` and `tool_search_output` with
`"execution": "server"`, `"call_id": null` and
`"status": "completed"`: the call's `arguments` is the persisted search
input, and the output's `tools` lists the request's own definitions of
the loaded tools, each with `defer_loading: true`, with namespace
members inside their namespace entry. A rebuilt `function_call` in a
request with the hosted search carries `namespace`: the namespace's
name for a namespace member, or the function's own name for a flat
deferred function. The API rejects a call to a function that a
`tool_search_output` in the same input loaded unless it carries its
namespace. A call to an eager function carries none, because the API
resolves it in the default namespace. This reconstruction is best
effort. The adapter drops both items of a search, and still sends
the function calls that follow, when the request has no hosted search,
when the search was recorded by another provider (a call not named
`tool_search`, such as an Anthropic `tool_search_tool_bm25` pair left by
a router switch), when a loaded tool has no definition in the request,
when the search failed, or when the call or its result is missing or
repeated.

**Batch.** Batch turns send the hosted search entry under the same
conditions as streamed turns. A batch result is decoded by the same code
as a streamed response, but batch turns never capture output items, so a
hosted search from a batch turn is always replayed through the
reconstruction above. Neither the batch request nor its replay has been
probed against the live API.

The request shape, the output items, verbatim replay and reconstruction
follow OpenAI's guide. The namespace shapes were probed on 2026-10-26
against `gpt-5.4-mini`: a search loads a namespace by name, members
with full names are returned and called by those names, and a rebuilt
call is accepted with its namespace and rejected without it.
Client-executed search (`"execution": "client"`, answered by the
harness) is not implemented.

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
- **Thought summaries.** Unless `reasoningSummary` is `off`, the request
  sets `generationConfig.thinkingConfig.includeThoughts: true` for a
  model whose quirks accept a thinking config (Gemini 3.6, 3.7 and 3.8,
  3.1 Pro, and 2.5 Flash) and for any request with a `reasoningEffort`;
  other models receive no `thinkingConfig`, because a model that does
  not think rejects one. Each response part marked `thought: true`
  streams as a `thinking_delta`.
  Thought parts never become answer text or tool input and are not kept
  in history; the `thoughtSignature` rides on the `functionCall` part
  and round-trips unchanged. Probed on Vertex AI and AI Studio for
  Gemini 3.6, 3.7, and 3.8 Flash and 3.1 Pro, including a replayed
  tool-calling turn. Summaries are best-effort: a short turn at the
  default thinking level often has none.
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
`deepseek-flash*` (V4.1 Flash) rules supply the replay threading,
sampling suppression, and legacy token key with no operator
configuration. The OpenRouter rules (`deepseek/deepseek-v4*`,
`deepseek/deepseek-flash*`, and one rule per listed `~deepseek/...-latest`
alias) supply the sampling suppression, token key and `reasoning_effort`
projection, and no replay, because OpenRouter does not require it.

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
