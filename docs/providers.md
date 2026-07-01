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

Default safety thresholds: none — the harness does not configure
Anthropic safety settings; the API defaults apply.

No model-ID allowlist: `RunConfig.Model` is forwarded to the wire
verbatim, so a newly released Claude model works with no code change
as long as its request/response shape matches the Messages API
contract this adapter already speaks. Claude Opus 4.7 and later, Claude
Sonnet 5, and Claude Fable 5 / Mythos 5 reject a non-default
`temperature` outright (HTTP 400) rather than ignoring it; since the
harness always resolves a non-nil default temperature
(`core.defaultTemperature = 0.1`) when `RunConfig.Temperature` is
unset, a per-model quirk rule omits the field for those models before
it reaches the wire — see [Per-model wire-shape
quirks](#per-model-wire-shape-quirks) below.

## AWS Bedrock

**File:** `harness/internal/provider/bedrock.go`

AWS ConverseStream API via `aws-sdk-go-v2`. Translates between the
harness's internal `Message` / `ToolCall` types and Bedrock's union-type
wire format. Auth is IAM (not API key); `config.LoadDefaultConfig()`
resolves credentials from the SDK default chain. Accepts an optional
`aws.CredentialsProvider` for cross-cloud credential federation (e.g.
`WebIdentityAWSSource` exchanging a GKE OIDC token for STS credentials).

## OpenAI Chat Completions

**File:** `harness/internal/provider/openai.go`

OpenAI chat completions streaming. Works with OpenAI, LiteLLM, Azure
OpenAI, vLLM, and Ollama via configurable `baseURL`. Key configuration
knobs:

- `provider.apiKeyHeader`: header name for the API key. Empty (default)
  sends `Authorization: Bearer`. Set to `api-key` for Azure OpenAI key
  auth.
- `provider.queryParams`: appended to every request URL. Use for Azure
  api-version pins (`api-version=preview`) or gateway-specific params.

Azure Entra ID bearer tokens work with the default empty `apiKeyHeader`
— the `Authorization: Bearer` header carries the Entra token normally.
See `examples/runconfig/azure-openai.json`.

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
`file_search`, `computer_use`, `code_interpreter`), server-side state
via `previous_response_id`, and reasoning controls. The harness manages
its own conversation history and does not delegate to server-side state.

Azure Foundry's `/openai/v1/responses` endpoint is wire-compatible:
point `provider.baseUrl` at the Azure resource, set
`provider.apiKeyHeader: "api-key"` for key auth (or leave empty for
Entra ID Bearer), and add `provider.queryParams: {"api-version":
"preview"}`. Azure-only Responses extensions ride the existing
forward-compatible "unknown SSE event" path and are silently ignored.
See `examples/runconfig/azure-openai.json`.

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
  categories. Override via `provider.geminiSafetySettings`.
- **Request/schema translation.** JSON Schema → Gemini OpenAPI Schema
  conversion: `provider/gemini_schema.go`. Request assembly:
  `provider/gemini_request.go`.

**Intentional exclusions:** multimodal input, server-side built-in
tools (`google_search`, `code_execution`, etc. — tracked as issue #93),
AI Studio direct support.

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
parameters, the newest Claude tier (Opus 4.7+, Sonnet 5, Fable 5 /
Mythos 5) rejects a non-default temperature the same way, Z.ai GLM
requires the legacy `max_tokens` key, Gemini 3.x emits a
`thoughtSignature` blob that must survive turn boundaries, and
DeepSeek v4's default-on thinking mode requires the
`reasoning_content` it streams replayed back on every request after
a tool-call turn (the API returns 400 otherwise). Rather than
encoding these as adapter-internal model substring checks, the
harness routes them through a registry-driven quirks layer at
`harness/internal/provider/quirks/`. DeepSeek v4 runs through the
stock Chat Completions adapter (`provider.type:
"openai-compatible"` with `provider.baseUrl:
"https://api.deepseek.com"`); the built-in `deepseek-v4*` and
`deepseek/deepseek-v4*` rules supply the replay threading, sampling
suppression, and legacy token key with no operator configuration.

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
