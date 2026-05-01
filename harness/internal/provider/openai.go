package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
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
	apiKey       string
	httpClient   *http.Client
	baseURL      string
	apiKeyHeader string
	queryParams  map[string]string
	Tracer       oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics      *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
}

// NewOpenAICompatibleAdapter creates an adapter for an OpenAI-compatible
// Chat Completions endpoint. The baseURL should be the API root
// (e.g. "https://api.openai.com/v1"); the /chat/completions path is appended
// automatically. Pass an empty string for the default OpenAI URL. The auth
// argument carries optional header-name and query-parameter overrides; pass
// a zero value for OpenAI-default behaviour.
func NewOpenAICompatibleAdapter(apiKey, baseURL string, auth OpenAIAuthConfig) *OpenAICompatibleAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	// Trim trailing slash so we get a clean URL join.
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAICompatibleAdapter{
		apiKey: apiKey,
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
	}
}

// --- OpenAI wire format types ---

// openaiRequest is the JSON body sent to the Chat Completions API.
type openaiRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	Tools       []openaiTool    `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	Stream      bool            `json:"stream"`
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

// Stream sends a streaming request to the OpenAI Chat Completions API and
// returns a channel of StreamEvents. The channel is closed when the stream
// ends or an error occurs. Cancelling the context terminates the stream.
func (o *OpenAICompatibleAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "openai-compatible"),
		attribute.String("provider.model", params.Model),
	)

	reqBody := openaiRequest{
		Model:       params.Model,
		Messages:    translateMessages(params.System, params.Messages),
		Tools:       translateTools(params.Tools),
		MaxTokens:   params.MaxTokens,
		Temperature: params.Temperature,
		Stream:      true,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
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
	setOpenAIAuthHeader(req, o.apiKey, o.apiKeyHeader)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// Record HTTP-level metadata on the span from context when OTel is enabled.
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
