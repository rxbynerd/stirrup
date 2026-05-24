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
		baseURL:      baseURL,
		apiKeyHeader: auth.APIKeyHeader,
		queryParams:  auth.QueryParams,
		RetryPolicy:  retry,
		Registry:     quirks.DefaultRegistry(),
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

// canonicalOpenAIFields enumerates the top-level Chat Completions
// request fields the adapter knows how to emit. ExtraBodyFields rules
// must not collide with any of these — the registry self-test guards
// the relationship at build time.
var canonicalOpenAIFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"tools":                 {},
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

// openaiToolDefinition is the function definition inside an openaiTool.
type openaiToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// --- SSE chunk types ---

// openaiChunk is a single SSE chunk from the streaming Chat Completions API.
type openaiChunk struct {
	ID      string              `json:"id"`
	Choices []openaiChunkChoice `json:"choices"`
	Usage   *openaiUsage        `json:"usage,omitempty"`
}

// openaiChunkChoice is a single choice within a streaming chunk.
type openaiChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
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

// translateTools converts stirrup ToolDefinitions to OpenAI's function format.
func translateTools(tools []types.ToolDefinition) []openaiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openaiTool, len(tools))
	for i, t := range tools {
		out[i] = openaiTool{
			Type: "function",
			Function: openaiToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return out
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
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. top_p on Responses, equivalent constraints on Chat
// Completions), change the return type to (json.RawMessage, error) and
// apply a batch-specific projection here.
func buildOpenAIRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks) openaiRequest {
	return openaiRequest{
		Model:              params.Model,
		Messages:           translateMessages(params.System, params.Messages),
		Tools:              translateTools(params.Tools),
		MaxTokens:          params.MaxTokens,
		Temperature:        params.Temperature,
		Stream:             stream,
		TokenField:         q.BehaviourFlags.OpenAI.TokenField,
		OmitSamplingParams: q.BehaviourFlags.OpenAI.OmitSamplingParams,
		ExtraBodyFields:    q.BehaviourFlags.OpenAI.ExtraBodyFields,
	}
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
	q := registry.Resolve("openai-compatible", params.Model)

	// Warn-level log when a caller-supplied non-nil Temperature is
	// suppressed by a quirk rule (design risk 2). The reasoning-class
	// rules omit temperature outright; without this signal an operator
	// who set --temperature would silently observe greedy decoding.
	if q.BehaviourFlags.OpenAI.OmitSamplingParams && params.Temperature != nil {
		logger := o.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "openai quirks suppressed caller temperature",
			slog.String("provider.type", "openai-compatible"),
			slog.String("provider.model", params.Model),
			slog.Float64("temperature.suppressed", *params.Temperature),
		)
	}

	reqBody := buildOpenAIRequest(params, true, q)

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
		o.consumeSSE(ctx, resp, ch, start, metricAttrs)
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
func (o *OpenAICompatibleAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption) {
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
