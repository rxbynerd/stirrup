package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// OpenAIAuthConfig carries the optional auth/URL knobs both OpenAI adapters
// share. It is a separate struct so the adapter constructors do not grow an
// unbounded number of positional arguments. A zero value preserves today's
// behaviour: Authorization: Bearer auth and no extra query parameters.
type OpenAIAuthConfig struct {
	// APIKeyHeader, when non-empty, replaces the default
	// "Authorization: Bearer <key>" header with "<APIKeyHeader>: <key>".
	// Used by Azure OpenAI key auth ("api-key") and similar gateways.
	APIKeyHeader string

	// QueryParams are appended to every request URL. Keys here override any
	// duplicate keys already present in BaseURL's query string — explicit
	// configuration always wins over BaseURL-encoded defaults.
	QueryParams map[string]string
}

const (
	openaiDefaultBaseURL   = "https://api.openai.com/v1"
	openaiMaxToolInputSize = 10 * 1024 * 1024 // 10 MB cap on streamed tool argument JSON
)

// OpenAICompatibleAdapter implements ProviderAdapter for the OpenAI Chat
// Completions API. It works with any OpenAI-compatible endpoint: OpenAI,
// LiteLLM, Azure OpenAI, vLLM, Ollama, llama.cpp server.
//
// Azure OpenAI deployments accept either Entra ID bearer tokens (default
// behaviour: empty APIKeyHeader → "Authorization: Bearer <token>") or a
// plain API key (set APIKeyHeader: "api-key"). Required api-version pins
// (and similar gateway parameters) are conveyed through OpenAIAuthConfig's
// QueryParams; values supplied there override any duplicate keys present
// in baseURL's query string.
type OpenAICompatibleAdapter struct {
	bearer       credential.BearerTokenFunc
	httpClient   *http.Client
	baseURL      string
	apiKeyHeader string
	queryParams  map[string]string
	Tracer       oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics      *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
	RetryPolicy  RetryPolicy            // optional, set by factory; zero value disables retry
	Logger       *slog.Logger           // optional, set by factory; nil falls back to slog.Default()
	// Registry resolves per-(provider, model) wire-shape and behaviour
	// overrides at the top of every Stream call. The constructor seeds
	// this with quirks.DefaultRegistry() so callers that ignore the
	// field still get the built-in rule set. Tests and the factory's
	// compat-profile injection path overwrite it directly; the public
	// API of the adapter does not need a WithRegistry option.
	Registry *quirks.Registry

	// strictSchemas memoises strict-mode schema rewrites within this
	// adapter's lifetime so repeated turns in the same run do not
	// re-walk the same JSON schema. Keyed by (model, tool-name,
	// schema-hash) so a model switch inside a run still hits the
	// normaliser. nil → no caching (NormalizeStrictSchema runs on
	// every turn); the constructor initialises a non-nil instance.
	strictSchemas *strictSchemaCache
}

// NewOpenAICompatibleAdapter creates an adapter for an OpenAI-compatible
// Chat Completions endpoint. The baseURL should be the API root
// (e.g. "https://api.openai.com/v1"); the /chat/completions path is appended
// automatically. Pass an empty string for the default OpenAI URL. The auth
// argument carries optional header-name and query-parameter overrides; pass
// a zero value for OpenAI-default behaviour. The retry argument is the
// resolved retry policy; pass a zero RetryPolicy to disable retries (one
// attempt with no backoff).
//
// bearer is invoked on every Stream call to fetch the current API key. For
// Azure Entra ID and other refresh-aware credentials this lets the
// underlying credential.Source rotate tokens transparently; static keys
// return a captured value with no IO. A nil bearer or one returning an
// empty string is treated as "no auth header" (some local gateways accept
// anonymous requests).
func NewOpenAICompatibleAdapter(bearer credential.BearerTokenFunc, baseURL string, auth OpenAIAuthConfig, retry RetryPolicy) *OpenAICompatibleAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	// Trim trailing slash so we get a clean URL join.
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAICompatibleAdapter{
		bearer: bearer,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		baseURL:       baseURL,
		apiKeyHeader:  auth.APIKeyHeader,
		queryParams:   auth.QueryParams,
		RetryPolicy:   retry,
		Registry:      quirks.DefaultRegistry(),
		strictSchemas: newStrictSchemaCache(),
	}
}

// --- OpenAI wire format types ---

// openaiRequest is the JSON body sent to the Chat Completions API.
//
// The token-limit field is serialised as "max_completion_tokens" rather than
// the legacy "max_tokens" because reasoning models (o1/o3/o4-mini and the
// gpt-5.x family) reject "max_tokens" with HTTP 400 on both OpenAI and
// Azure OpenAI Chat Completions endpoints; "max_completion_tokens" is
// required there and accepted by every non-reasoning model, so the rename
// is strictly safer than feature-detecting per model.
//
// Temperature is *float64 with omitempty: a nil pointer omits the key
// entirely (reasoning models reject "temperature" outright); a non-nil
// pointer transmits the dereferenced value verbatim, including an
// explicit 0.0 for greedy decoding. This mirrors the upstream
// StreamParams.Temperature pointer type so the unset-vs-explicit-zero
// distinction survives marshalling. See issue #200.
//
// TokenField and OmitSamplingParams carry the resolved quirks for
// this request and drive MarshalJSON, which is the single point that
// translates the canonical struct into the wire-shape selected by the
// rule. The field name mirrors quirks.OpenAIBehaviourFlags.OmitSamplingParams
// so a search for either form finds both sides of the projection.
// ExtraBodyFields carries provider-specific top-level keys (e.g.
// Z.ai's "tool_stream") that are merged after the canonical fields.
// None of these three are serialised under their own JSON keys —
// they steer the MarshalJSON projection only.
type openaiRequest struct {
	Model              string          `json:"-"`
	Messages           []openaiMessage `json:"-"`
	Tools              []openaiTool    `json:"-"`
	MaxTokens          int             `json:"-"`
	Temperature        *float64        `json:"-"`
	Stream             bool            `json:"-"`
	TokenField         quirks.OpenAITokenField
	OmitSamplingParams bool
	ExtraBodyFields    map[string]any
	// ToolChoice is the wire value for the OpenAI "tool_choice" field:
	// a string ("auto"/"required"/"none") or an object naming a function.
	// A nil interface omits the field entirely so the zero-value
	// ToolChoiceAuto request is byte-identical to the pre-#230 shape.
	// Populated by buildOpenAIRequest only when the resolved capability
	// advertises support for the requested mode; steers MarshalJSON, it
	// has no JSON struct tag of its own.
	ToolChoice any
}

// MarshalJSON projects the canonical openaiRequest into the wire body
// the resolved quirks selected. The projection rules are:
//
//   - "model", "messages", "stream" — always emitted.
//   - "tools" — emitted only when non-empty (matches the prior
//     omitempty behaviour).
//   - The token-budget field uses the key selected by TokenField:
//     "max_completion_tokens" (default) or "max_tokens" (Z.ai compat
//     and similar legacy gateways). The value is always emitted, even
//     at zero, matching the prior struct-tag behaviour.
//   - "temperature" — emitted only when both OmitSamplingParams is
//     false AND Temperature is non-nil. OmitSamplingParams = true
//     guarantees the field is suppressed even when the caller supplied
//     a non-nil value (per design risk 2, the adapter's Stream call
//     logs a warning when this suppression fires).
//   - Other sampling params (top_p, presence_penalty,
//     frequency_penalty, logprobs, top_logprobs, logit_bias) are not
//     yet first-class struct fields; they will be omitted by default
//     once added if OmitSamplingParams is true. The flag's contract is
//     declared in OpenAIBehaviourFlags doc comments.
//   - ExtraBodyFields — merged into the body after canonical fields.
//     Key collision with a canonical key is rejected as an error
//     (a misconfigured rule should fail loudly rather than silently
//     overwrite a struct field). The collision set is the canonical
//     OpenAI Chat Completions field surface this adapter emits.
func (r openaiRequest) MarshalJSON() ([]byte, error) {
	tokenKey := openAIWireTokenKey(r.TokenField)
	out := map[string]any{
		"model":    r.Model,
		"messages": r.Messages,
		"stream":   r.Stream,
		tokenKey:   r.MaxTokens,
	}
	if len(r.Tools) > 0 {
		out["tools"] = r.Tools
	}
	if r.ToolChoice != nil {
		out["tool_choice"] = r.ToolChoice
	}
	if !r.OmitSamplingParams && r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	for k, v := range r.ExtraBodyFields {
		if _, exists := out[k]; exists {
			return nil, fmt.Errorf("openai quirk extra body field %q collides with canonical request field", k)
		}
		// Also reject collisions against canonical fields we elide
		// above (temperature when suppressed, tools when empty): the
		// rule author shouldn't be able to sneak a field past via the
		// extras map.
		if isCanonicalOpenAIField(k) {
			return nil, fmt.Errorf("openai quirk extra body field %q collides with canonical request field", k)
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON is the inverse of MarshalJSON: it reads either
// "max_completion_tokens" or "max_tokens" into MaxTokens (setting
// TokenField accordingly) and populates the canonical fields. Used by
// tests that round-trip the wire body through the same struct that
// produced it. Any non-canonical top-level keys are collected into
// ExtraBodyFields so the round-trip is loss-free.
func (r *openaiRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// Canonical fields with simple types.
	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &r.Model); err != nil {
			return fmt.Errorf("openaiRequest.model: %w", err)
		}
		delete(raw, "model")
	}
	if v, ok := raw["messages"]; ok {
		if err := json.Unmarshal(v, &r.Messages); err != nil {
			return fmt.Errorf("openaiRequest.messages: %w", err)
		}
		delete(raw, "messages")
	}
	if v, ok := raw["tools"]; ok {
		if err := json.Unmarshal(v, &r.Tools); err != nil {
			return fmt.Errorf("openaiRequest.tools: %w", err)
		}
		delete(raw, "tools")
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &r.Stream); err != nil {
			return fmt.Errorf("openaiRequest.stream: %w", err)
		}
		delete(raw, "stream")
	}
	if v, ok := raw["temperature"]; ok {
		var t float64
		if err := json.Unmarshal(v, &t); err != nil {
			return fmt.Errorf("openaiRequest.temperature: %w", err)
		}
		r.Temperature = &t
		delete(raw, "temperature")
	}
	if v, ok := raw["tool_choice"]; ok {
		var tc any
		if err := json.Unmarshal(v, &tc); err != nil {
			return fmt.Errorf("openaiRequest.tool_choice: %w", err)
		}
		r.ToolChoice = tc
		delete(raw, "tool_choice")
	}
	// Token budget: accept either canonical key. MarshalJSON emits
	// exactly one key, so a valid request body should not contain
	// both simultaneously. If both are present the input is a caller
	// error (likely a hand-crafted body or a misconfigured rule) and
	// is rejected here rather than silently letting one overwrite the
	// other; without this guard the second decode would clobber the
	// first depending on decode order.
	_, hasMCT := raw["max_completion_tokens"]
	_, hasMT := raw["max_tokens"]
	if hasMCT && hasMT {
		return fmt.Errorf("openaiRequest: both max_completion_tokens and max_tokens present")
	}
	if v, ok := raw["max_completion_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return fmt.Errorf("openaiRequest.max_completion_tokens: %w", err)
		}
		r.TokenField = quirks.TokenFieldMaxCompletionTokens
		delete(raw, "max_completion_tokens")
	}
	if v, ok := raw["max_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return fmt.Errorf("openaiRequest.max_tokens: %w", err)
		}
		r.TokenField = quirks.TokenFieldMaxTokens
		delete(raw, "max_tokens")
	}
	// Remaining keys are provider-specific extras.
	if len(raw) > 0 {
		extra := make(map[string]any, len(raw))
		for k, v := range raw {
			var anyV any
			if err := json.Unmarshal(v, &anyV); err != nil {
				return fmt.Errorf("openaiRequest.%s: %w", k, err)
			}
			extra[k] = anyV
		}
		r.ExtraBodyFields = extra
	}
	return nil
}

// openAIWireTokenKey returns the wire JSON key for the resolved token
// field. Defaults to "max_completion_tokens" — the zero value of
// OpenAITokenField — to preserve the established behaviour of openai.go
// on main. TestNoRegressionMaxCompletionTokensDefault pins this.
func openAIWireTokenKey(f quirks.OpenAITokenField) string {
	if f == quirks.TokenFieldMaxTokens {
		return "max_tokens"
	}
	return "max_completion_tokens"
}

// ruleDescriptions returns the Description field of each rule in the
// supplied slice, preserving order. Returned as a non-nil empty slice
// when the input is empty so the slog.Any attribute renders as `[]`
// rather than `null` — easier to grep and parse downstream.
func ruleDescriptions(rules []quirks.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Description)
	}
	return out
}

// summarizeReplayCaptures sums per-path piece counts and the string-or-
// JSON-encoded length of every captured value across the whole map. The
// same length proxy logReplayFieldsCapture uses (raw string length for
// string values, json.Marshal length otherwise) so the span attribute
// and the slog summary agree. Returned as totals only — never per-path,
// never per-value — so callers can attach the summary to an OTel span
// without enumerating field names that would balloon attribute count
// on a multi-rule stream.
func summarizeReplayCaptures(capture map[string][]any) (totalCount, totalLen int) {
	for _, values := range capture {
		totalCount += len(values)
		for _, v := range values {
			switch s := v.(type) {
			case string:
				totalLen += len(s)
			default:
				b, err := json.Marshal(v)
				if err == nil {
					totalLen += len(b)
				}
			}
		}
	}
	return totalCount, totalLen
}

// logReplayFieldsCapture emits a debug-level summary of the per-stream
// ReplayFields capture (design §5, design D12). Length-only reporting:
// the captured values themselves are provider-private blobs (DeepSeek's
// reasoning_content, Gemini's thoughtSignature) that the harness must
// not echo into any log sink. Operators get presence + size; the rule
// fired observably without leaking the content.
//
// Shared by both adapters so the log shape stays uniform; the
// helper lives in openai.go because that is where ruleDescriptions
// already sits.
func logReplayFieldsCapture(ctx context.Context, logger *slog.Logger, providerType, model string, capture map[string][]any) {
	if logger == nil {
		logger = slog.Default()
	}
	// Sort the path keys so the log output is deterministic across
	// runs even though Go map iteration order is not. Stable ordering
	// makes the line greppable across runs and easier to test against.
	paths := make([]string, 0, len(capture))
	for k := range capture {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	// Per-path summary: count of captured pieces and sum of stringified
	// lengths. The string length is a proxy for "size of what was
	// captured" that does not require knowing the underlying type; for
	// non-string values, the JSON encoding's length is the natural
	// proxy. Errors marshalling a value back to JSON degrade to a
	// zero length rather than failing the log line.
	summaries := make([]any, 0, len(paths))
	for _, p := range paths {
		values := capture[p]
		totalLen := 0
		for _, v := range values {
			switch s := v.(type) {
			case string:
				totalLen += len(s)
			default:
				b, err := json.Marshal(v)
				if err == nil {
					totalLen += len(b)
				}
			}
		}
		summaries = append(summaries,
			slog.Group(p,
				slog.Int("count", len(values)),
				slog.Int("total_len", totalLen),
			),
		)
	}
	logger.DebugContext(ctx, "quirks replay fields captured",
		slog.String("provider.type", providerType),
		slog.String("provider.model", model),
		slog.Group("replay_fields_captured", summaries...),
	)
}

// canonicalOpenAIFields enumerates the top-level Chat Completions
// request fields the adapter knows how to emit. ExtraBodyFields rules
// must not collide with any of these — the registry self-test guards
// the relationship at build time.
var canonicalOpenAIFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"tools":                 {},
	"tool_choice":           {},
	"max_completion_tokens": {},
	"max_tokens":            {},
	"temperature":           {},
	"top_p":                 {},
	"presence_penalty":      {},
	"frequency_penalty":     {},
	"logprobs":              {},
	"top_logprobs":          {},
	"logit_bias":            {},
	"stream":                {},
}

// isCanonicalOpenAIField reports whether the given top-level request
// key is owned by the canonical openaiRequest projection. The check
// gates ExtraBodyFields merges to prevent a rule from overriding a
// struct-mediated field by way of the extras map.
func isCanonicalOpenAIField(k string) bool {
	_, ok := canonicalOpenAIFields[k]
	return ok
}

// openaiMessage is a single message in OpenAI's Chat Completions format.
type openaiMessage struct {
	Role       string           `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openaiToolCall represents a tool invocation in an assistant message.
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // "function"
	Function openaiToolFunction `json:"function"`
}

// openaiToolFunction is the function payload inside a tool call.
type openaiToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openaiTool describes a tool in OpenAI's function calling format.
type openaiTool struct {
	Type     string               `json:"type"` // "function"
	Function openaiToolDefinition `json:"function"`
}

// openAINamedToolChoice is the object form of OpenAI's tool_choice that
// forces a specific function. Typed (rather than a map[string]any) so the
// marshalled key order is deterministic — "type" before "function" — and
// to honour the project's anti-`any` rule (wave-2 design D13). The string
// forms ("required"/"none") are emitted directly as a string value, so
// only the named form needs a struct.
type openAINamedToolChoice struct {
	Type     string                    `json:"type"` // "function"
	Function openAINamedToolChoiceFunc `json:"function"`
}

// openAINamedToolChoiceFunc is the function payload inside an
// openAINamedToolChoice.
type openAINamedToolChoiceFunc struct {
	Name string `json:"name"`
}

// openaiToolDefinition is the function definition inside an openaiTool.
//
// Strict is a *bool so the zero-value request body omits the field
// entirely (matching the pre-#228 behaviour) and a quirks rule that
// pins strict mode emits an explicit `"strict": true`. A pointer keeps
// the wire shape from accidentally regressing for adapters that do
// not opt in.
type openaiToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// --- SSE chunk types ---

// openaiChunk is a single SSE chunk from the streaming Chat Completions API.
type openaiChunk struct {
	ID      string              `json:"id"`
	Choices []openaiChunkChoice `json:"choices"`
	Usage   *openaiUsage        `json:"usage,omitempty"`
}

// openaiChunkChoice is a single choice within a streaming chunk.
//
// RawDelta is the un-decoded JSON bytes of the same delta object that
// Delta carries — captured so ReplayFields rules (design D12) can walk
// non-canonical assistant-message fields (e.g. DeepSeek's
// `reasoning_content`) without the adapter declaring them as typed
// fields on openaiDelta. Populated by openaiChunkChoice.UnmarshalJSON;
// the field is on the struct rather than computed lazily so the SSE
// parse loop accesses it at zero cost when no rule is active.
type openaiChunkChoice struct {
	Index        int             `json:"index"`
	Delta        openaiDelta     `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	RawDelta     json.RawMessage `json:"-"`
}

// UnmarshalJSON captures the raw bytes of the `delta` field alongside
// the typed decode. The two-pass shape mirrors openaiRequest's
// MarshalJSON / UnmarshalJSON: the typed fields drive the normal SSE
// loop; the RawMessage gives the ReplayFields path walker a document
// to descend without coupling the walker to the typed struct.
func (c *openaiChunkChoice) UnmarshalJSON(data []byte) error {
	type alias openaiChunkChoice
	var helper struct {
		alias
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(data, &helper); err != nil {
		return err
	}
	*c = openaiChunkChoice(helper.alias)
	if len(helper.Delta) > 0 {
		c.RawDelta = append(c.RawDelta[:0], helper.Delta...)
		if err := json.Unmarshal(helper.Delta, &c.Delta); err != nil {
			// No security.Scrub on the wrapped error: stdlib json
			// decode errors (UnmarshalTypeError, SyntaxError) carry
			// only the Go type name and field path — never the
			// offending value. The gemini adapter does scrub its
			// HTTP error-body branch (gemini.go around the
			// resp.StatusCode != 200 path) because that branch
			// holds the provider's diagnostic prose, which can
			// contain quota identifiers and trace IDs; the per-
			// chunk JSON decode here has no such payload.
			return fmt.Errorf("openaiChunkChoice.delta: %w", err)
		}
	}
	return nil
}

// openaiDelta is the incremental content in a streaming chunk.
type openaiDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   *string               `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`
}

// openaiToolCallDelta is an incremental tool call in a streaming chunk.
type openaiToolCallDelta struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openaiToolFunctionDelta `json:"function"`
}

// openaiToolFunctionDelta is the incremental function data in a tool call delta.
type openaiToolFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openaiUsage tracks token usage in the final chunk.
type openaiUsage struct {
	CompletionTokens int `json:"completion_tokens"`
}

// openaiErrorResponse is the error format returned by the OpenAI API.
type openaiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// openaiToolCallState tracks the accumulation of a single tool call's
// arguments across multiple SSE chunks.
type openaiToolCallState struct {
	id      string
	name    string
	argsBuf strings.Builder
}

// --- Message translation ---

// translateMessages converts stirrup's internal Message/ContentBlock format
// to OpenAI's chat message format. The system prompt is prepended as a
// system message.
func translateMessages(system string, messages []types.Message) []openaiMessage {
	var out []openaiMessage

	if system != "" {
		out = append(out, openaiMessage{
			Role:    "system",
			Content: system,
		})
	}

	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			oai := openaiMessage{Role: "assistant"}
			var textParts []string
			var toolCalls []openaiToolCall

			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_use":
					args := string(block.Input)
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, openaiToolCall{
						ID:   block.ID,
						Type: "function",
						Function: openaiToolFunction{
							Name:      block.Name,
							Arguments: args,
						},
					})
				}
			}

			if len(textParts) > 0 {
				oai.Content = strings.Join(textParts, "")
			}
			if len(toolCalls) > 0 {
				oai.ToolCalls = toolCalls
			}
			out = append(out, oai)

		case "user":
			// User messages can contain text blocks or tool_result blocks.
			// Tool results must be sent as separate "tool" role messages.
			var textParts []string
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_result":
					// Each tool result is its own message with role "tool".
					content := block.Content
					if block.IsError {
						content = "Error: " + content
					}
					out = append(out, openaiMessage{
						Role:       "tool",
						Content:    content,
						ToolCallID: block.ToolUseID,
					})
				}
			}
			if len(textParts) > 0 {
				out = append(out, openaiMessage{
					Role:    "user",
					Content: strings.Join(textParts, ""),
				})
			}
		}
	}

	return out
}

// translateTools converts stirrup ToolDefinitions to OpenAI's function
// format. When strict is true, every tool's Parameters is rewritten by
// NormalizeStrictSchema and the Strict flag is set on the wire entry;
// the cache memoises rewrites by (model, tool-name, schema-hash) so
// repeated turns in the same run skip the recursive walk.
//
// Returns an error when strict-mode normalisation rejects a tool's
// schema — the request must not be sent in that case (design §5
// fail-closed contract).
func translateTools(tools []types.ToolDefinition, strict bool, model string, cache *strictSchemaCache) ([]openaiTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openaiTool, len(tools))
	for i, t := range tools {
		def := openaiToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
		if strict {
			normalised, err := normalizeStrictWithCache(cache, model, t.Name, t.InputSchema)
			if err != nil {
				return nil, err
			}
			def.Parameters = normalised
			truthy := true
			def.Strict = &truthy
		}
		out[i] = openaiTool{
			Type:     "function",
			Function: def,
		}
	}
	return out, nil
}

// openAIToolChoiceFromParams projects the provider-neutral
// StreamParams.ToolChoice onto the OpenAI tool_choice wire value, gated
// on the resolved capability. Returns nil — meaning "emit no field" — for
// the auto mode and for any unsupported mode, leaving the request
// byte-identical to the pre-#230 shape.
//
// OpenAI accepts a string ("auto"/"required"/"none") or an object naming
// a specific function. The named-tool form degrades to nil when the tool
// name is empty or fails ValidateToolChoiceName (not expressible / unsafe)
// rather than emitting an invalid object.
//
// The return type is `any` because OpenAI's tool_choice is a sum type
// (string OR object); the named-tool object is the typed
// openAINamedToolChoice so its key order is deterministic on the wire.
func openAIToolChoiceFromParams(params types.StreamParams, capability quirks.ToolChoiceCapability) any {
	if !capability.Supported {
		return nil
	}
	switch params.ToolChoice {
	case types.ToolChoiceRequired:
		if !capability.Required {
			return nil
		}
		return "required"
	case types.ToolChoiceNone:
		if !capability.None {
			return nil
		}
		return "none"
	case types.ToolChoiceTool:
		if !capability.NamedTool || params.ToolChoiceName == "" {
			return nil
		}
		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("openai-compatible", params.Model, len(params.ToolChoiceName))
			return nil
		}
		return openAINamedToolChoice{
			Type:     "function",
			Function: openAINamedToolChoiceFunc{Name: params.ToolChoiceName},
		}
	default:
		// ToolChoiceAuto (zero value): auto is the wire default, so emit
		// nothing rather than an explicit "auto" string.
		return nil
	}
}

// warnInvalidToolChoiceName logs a single warn when a ToolChoiceTool
// request carried a name that failed ValidateToolChoiceName, so the
// degradation to auto is observable. Shared by all three adapters'
// projection helpers. Uses slog.Default() rather than threading the
// per-adapter logger: this is a should-never-happen defensive path (A1
// has no caller that feeds model-influenced names through ToolChoiceName),
// so the value of trace correlation does not justify widening every
// builder signature.
//
// The offending name is NOT logged. It is caller/model-influenced input
// that could carry log-injection bytes (newlines, control chars), and the
// fixed grammar in the message is enough for an operator to understand
// the rejection. The name's length is the only quantitative signal, which
// is safe to surface.
func warnInvalidToolChoiceName(providerType, model string, nameLen int) {
	slog.Default().Warn("tool choice name failed validation; degrading to auto",
		slog.String("provider.type", providerType),
		slog.String("provider.model", model),
		slog.String("grammar", "^[a-zA-Z0-9_-]{1,64}$"),
		slog.Int("name_len", nameLen),
	)
}

// mapFinishReason converts OpenAI's finish_reason to stirrup's stop reason.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}

// buildOpenAIRequest projects a StreamParams into the Chat Completions wire
// body. The stream argument toggles the "stream" field so a future
// non-streaming caller (batch submission, phase 2 of issue #133) can reuse
// the same projection without duplicating field-by-field copying.
//
// q carries the resolved quirks for the (provider, model) pair. The zero
// value reproduces today's behaviour: max_completion_tokens emitted,
// temperature handled by Temperature *float64 omitempty semantics, no
// extra body fields. Pass quirks.DefaultRegistry().Resolve(...) at call
// sites that have access to a registry; the zero-value path remains
// valid for callers that intentionally skip resolution.
//
// strictCache memoises strict-mode schema rewrites across turns. Pass nil
// to disable caching (test/one-shot callers); the per-adapter cache is
// the production path. Returns an error when a tool's schema fails the
// strict-mode lint — the caller must NOT send a request in that case.
//
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. top_p on Responses, equivalent constraints on Chat
// Completions), apply a batch-specific projection here.
func buildOpenAIRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks, strictCache *strictSchemaCache) (openaiRequest, error) {
	tools, err := translateTools(params.Tools, q.BehaviourFlags.OpenAI.StrictMode, params.Model, strictCache)
	if err != nil {
		return openaiRequest{}, err
	}
	return openaiRequest{
		Model:              params.Model,
		Messages:           translateMessages(params.System, params.Messages),
		Tools:              tools,
		MaxTokens:          params.MaxTokens,
		Temperature:        params.Temperature,
		Stream:             stream,
		TokenField:         q.BehaviourFlags.OpenAI.TokenField,
		OmitSamplingParams: q.BehaviourFlags.OpenAI.OmitSamplingParams,
		ExtraBodyFields:    q.BehaviourFlags.OpenAI.ExtraBodyFields,
		ToolChoice:         openAIToolChoiceFromParams(params, q.ToolChoice),
	}, nil
}

// Stream sends a streaming request to the OpenAI Chat Completions API and
// returns a channel of StreamEvents. The channel is closed when the stream
// ends or an error occurs. Cancelling the context terminates the stream.
func (o *OpenAICompatibleAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "openai-compatible"),
		attribute.String("provider.model", params.Model),
	)

	// Resolve quirks for this (provider, model) pair. The registry is
	// per-stream by design (D4): the same run can switch models turn
	// to turn under a dynamic router. The zero-value Registry shouldn't
	// happen for adapters built through the factory, but tolerate it
	// for callers that construct the adapter directly without going
	// through NewOpenAICompatibleAdapter.
	registry := o.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("openai-compatible", params.Model)

	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Debug-level log lists the descriptions of every rule that
	// contributed to this resolution (design §5). Emitted even when
	// the list is empty so an operator grepping for the line knows
	// the resolution ran; an absent entry would be ambiguous between
	// "no rule fired" and "the log line was suppressed". The list is
	// in apply order — the last entry is the rule that won on
	// overlapping fields.
	logger.DebugContext(ctx, "openai quirks resolved",
		slog.String("provider.type", "openai-compatible"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	// Mirror the applied-rules list onto the OTel span as a
	// `provider.quirk.applied` attribute (design §5). The slog DEBUG
	// line and the span attribute are emitted together so log-only
	// (Cloud Logging) and trace-only (Jaeger, Honeycomb, Datadog)
	// consumers both observe which rules fired. The IsValid guard
	// tolerates the no-tracer case: when no exporter is configured,
	// SpanFromContext returns a non-recording span whose
	// SetAttributes is a no-op anyway, but checking IsValid keeps the
	// attribute slice allocation off the hot path when no tracer is
	// attached.
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}

	// Warn-level log when a caller-supplied non-nil Temperature is
	// suppressed by a quirk rule (design risk 2, §9). The reasoning-
	// class rules omit temperature outright; without this signal an
	// operator who set --temperature would silently observe greedy
	// decoding. The rule descriptions are attached so the operator
	// can name the rule that fired without grepping the source. The
	// suppressed value itself is intentionally NOT logged — it is the
	// caller's input and surfacing it here would leak the value into
	// any log sink that captures warn-level records.
	if q.BehaviourFlags.OpenAI.OmitSamplingParams && params.Temperature != nil {
		logger.WarnContext(ctx, "openai quirks suppressed caller temperature",
			slog.String("provider.type", "openai-compatible"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	// Debug-level log when strict-mode normalisation fires for this
	// stream — paired with the rules list so an operator grepping for
	// the line can name the rule that turned the flag on. The log is
	// emitted before buildOpenAIRequest so a fail-closed lint error
	// surfaces against a context that already shows strict-mode was
	// active.
	if q.BehaviourFlags.OpenAI.StrictMode {
		logger.DebugContext(ctx, "openai strict mode applied",
			slog.String("provider.type", "openai-compatible"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	reqBody, err := buildOpenAIRequest(params, true, q, o.strictSchemas)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Resolve the bearer credential before issuing the HTTP request so a
	// failure in the credential layer surfaces synchronously.
	apiKey, err := resolveBearer(ctx, o.bearer)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, err
	}

	requestURL, err := composeOpenAIURL(o.baseURL, "/chat/completions", o.queryParams)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("compose request URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

	resp, err := DoWithRetry(ctx, o.httpClient, req, RetryOptions{
		Policy:       o.RetryPolicy,
		Logger:       o.Logger,
		Metrics:      o.Metrics,
		ProviderType: "openai-compatible",
		Model:        params.Model,
	})
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// Record HTTP-level metadata on the span from context when OTel is enabled.
	// The rate_limited event fires when DoWithRetry returns a terminal 429
	// response (retries exhausted, budget exhausted, or retries disabled via
	// MaxAttempts=1). DoWithRetry records provider_retry_attempt for any
	// intermediate retries; the user-visible "request failed with 429" signal
	// remains here so existing dashboards and alerts that key off rate_limited
	// continue to function.
	if o.Tracer != nil {
		span := oteltrace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		if resp.StatusCode == 429 {
			retryAfter := resp.Header.Get("Retry-After")
			span.AddEvent("rate_limited", oteltrace.WithAttributes(
				attribute.String("retry_after", retryAfter),
			))
		}
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		o.recordLatency(ctx, start, metricAttrs)
		var errResp openaiErrorResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errResp); err == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("openai API returned status %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("openai API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		o.consumeSSE(ctx, resp, ch, start, metricAttrs, q, logger, params.Model)
		o.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (o *OpenAICompatibleAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if o.Metrics == nil {
		return
	}
	o.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads SSE lines from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// streamStart and metricAttrs are forwarded for ProviderTTFB measurement: the
// first non-empty stream event observed marks "time to first byte" for this
// request. TTFB is recorded at most once per stream.
//
// q carries the resolved per-(provider, model) quirks for this stream
// (design D5). The parser reads q.ReplayFields and captures the named
// paths from each chunk's delta object — design D12. The captured set
// surfaces in a per-stream debug log emitted at the end of the stream
// (Wave 2 lands parse-side recognition only — outbound threading is
// deferred per §9 risk 7).
//
// logger and model are threaded purely so the per-stream replay-fields
// debug summary names the model and stays on the same logger the
// caller-side debug/warn logs use.
func (o *OpenAICompatibleAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption, q quirks.ProviderQuirks, logger *slog.Logger, model string) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// emitEvent sends an event on the output channel and records TTFB on the
	// first non-empty event observed. flushToolCalls invokes this via the
	// closure passed in.
	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && o.Metrics != nil {
			o.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight tool calls by index for argument accumulation.
	toolCalls := make(map[int]*openaiToolCallState)

	// replayFieldsCapture is the per-stream accumulator for the
	// ReplayFields-captured values across every chunk's delta. Keyed by
	// the rule's path string (verbatim from q.ReplayFields). Stored as
	// a slice per path so a multi-chunk delta (e.g. DeepSeek's
	// reasoning_content streamed across multiple chunks) records each
	// piece in arrival order rather than only the last value. The
	// terminal log line summarises lengths, not values.
	replayFieldsCapture := map[string][]any{}
	// Emit the per-stream ReplayFields summary on any exit path
	// (normal end, early return on error, ctx cancel). Length-only
	// reporting per design §5: avoid leaking captured content into
	// any log sink that consumes DEBUG records.
	defer func() {
		if len(replayFieldsCapture) == 0 {
			return
		}
		// Mirror the per-stream capture summary onto the OTel span
		// (design §5): emit `replay_fields_captured.count` and
		// `.total_len` so trace consumers see the rule fired without
		// having to correlate with slog. Length-only — values stay
		// off the trace just as they stay off the log. The IsValid
		// guard keeps the marshal cost off the no-tracer path.
		if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			totalCount, totalLen := summarizeReplayCaptures(replayFieldsCapture)
			span.SetAttributes(
				attribute.Int("replay_fields_captured.count", totalCount),
				attribute.Int("replay_fields_captured.total_len", totalLen),
			)
		}
		logReplayFieldsCapture(ctx, logger, "openai-compatible", model, replayFieldsCapture)
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		// Skip empty lines (SSE separators) and comments.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		// The stream terminator.
		if data == "[DONE]" {
			// Flush any remaining tool calls before ending.
			o.flushToolCallsVia(toolCalls, emitEvent)
			return
		}

		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse chunk: %w", err)})
			return
		}

		for _, choice := range chunk.Choices {
			// ReplayFields capture (design D12, Wave 2 parse-side
			// recognition). Walk the raw delta object once per chunk
			// and merge any matching paths into the per-stream
			// accumulator. The path walker is a no-op when
			// q.ReplayFields is empty so the cost in the zero-rules
			// case is one slice-length check.
			if len(q.ReplayFields) > 0 && len(choice.RawDelta) > 0 {
				if captured := quirks.CaptureFromJSON(choice.RawDelta, q.ReplayFields); len(captured) > 0 {
					for k, v := range captured {
						replayFieldsCapture[k] = append(replayFieldsCapture[k], v...)
					}
				}
			}

			// Text content delta.
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: *choice.Delta.Content,
				})
			}

			// Tool call deltas — accumulate arguments by index.
			for _, tc := range choice.Delta.ToolCalls {
				state, exists := toolCalls[tc.Index]
				if !exists {
					state = &openaiToolCallState{}
					toolCalls[tc.Index] = state
				}
				if tc.ID != "" {
					state.id = tc.ID
				}
				if tc.Function.Name != "" {
					state.name = tc.Function.Name
				}
				if state.argsBuf.Len()+len(tc.Function.Arguments) > openaiMaxToolInputSize {
					emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
					return
				}
				state.argsBuf.WriteString(tc.Function.Arguments)
			}

			// finish_reason signals the end of this choice.
			if choice.FinishReason != nil {
				// Flush accumulated tool calls when the model is done.
				o.flushToolCallsVia(toolCalls, emitEvent)

				stopReason := mapFinishReason(*choice.FinishReason)
				ev := types.StreamEvent{
					Type:       "message_complete",
					StopReason: stopReason,
				}
				if chunk.Usage != nil {
					ev.OutputTokens = chunk.Usage.CompletionTokens
				}
				emitEvent(ev)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}

// composeOpenAIURL parses baseURL, appends path (using Path so existing
// path components survive a trailing slash), then merges queryParams into
// the existing query string. Keys present in queryParams override any
// duplicates already encoded on baseURL — explicit configuration always
// wins over BaseURL-encoded defaults. Shared by both OpenAI adapters so
// switching between provider.type "openai-compatible" and "openai-responses"
// produces identical URL composition behaviour.
func composeOpenAIURL(baseURL, path string, queryParams map[string]string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse baseURL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(queryParams) > 0 {
		q := u.Query()
		for k, v := range queryParams {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// setOpenAIAuthHeader applies the configured auth header to req. With an
// empty apiKey this is a no-op (some local gateways accept anonymous
// requests). With a non-empty apiKeyHeader, the resolved key is sent under
// that header name verbatim — caller-side validation (ValidateRunConfig)
// is responsible for rejecting header names containing CRLF or whitespace.
// With an empty apiKeyHeader, today's "Authorization: Bearer <key>"
// behaviour is preserved.
func setOpenAIAuthHeader(req *http.Request, apiKey, apiKeyHeader string) {
	if apiKey == "" {
		return
	}
	if apiKeyHeader == "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return
	}
	req.Header.Set(apiKeyHeader, apiKey)
}

// resolveBearer invokes the bearer closure to fetch the current API key. A
// nil closure is treated as "no auth"; empty-string returns are also valid
// for local gateways that accept anonymous requests. Errors are wrapped so
// the provider name does not need to be repeated at every call site.
func resolveBearer(ctx context.Context, bearer credential.BearerTokenFunc) (string, error) {
	if bearer == nil {
		return "", nil
	}
	tok, err := bearer(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve bearer token: %w", err)
	}
	return tok, nil
}

// flushToolCallsVia emits tool_call events for all accumulated tool calls via
// the supplied emit function (which may also record TTFB), then clears the
// state map. Called when the stream signals completion.
func (o *OpenAICompatibleAdapter) flushToolCallsVia(toolCalls map[int]*openaiToolCallState, emit func(types.StreamEvent)) {
	// Emit in index order for determinism.
	for idx := 0; idx < len(toolCalls); idx++ {
		state, ok := toolCalls[idx]
		if !ok {
			continue
		}
		var input map[string]any
		raw := state.argsBuf.String()
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool arguments JSON: %w", err)})
				return
			}
		}
		emit(types.StreamEvent{
			Type:  "tool_call",
			ID:    state.id,
			Name:  state.name,
			Input: input,
		})
	}
	// Clear the map.
	for k := range toolCalls {
		delete(toolCalls, k)
	}
}
