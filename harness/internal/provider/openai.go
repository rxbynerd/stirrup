package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

const (
	openaiDefaultBaseURL   = "https://api.openai.com/v1"
	openaiMaxToolInputSize = 10 * 1024 * 1024 // 10 MB cap on streamed tool argument JSON
)

// OpenAICompatibleAdapter implements ProviderAdapter for the OpenAI Chat
// Completions API. It works with any OpenAI-compatible endpoint: OpenAI,
// LiteLLM, Azure OpenAI, vLLM, Ollama, llama.cpp server.
type OpenAICompatibleAdapter struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
	Tracer     oteltrace.Tracer // optional, set by factory for span instrumentation
}

// NewOpenAICompatibleAdapter creates an adapter for an OpenAI-compatible
// Chat Completions endpoint. The baseURL should be the API root
// (e.g. "https://api.openai.com/v1"); the /chat/completions path is appended
// automatically. Pass an empty string for the default OpenAI URL.
func NewOpenAICompatibleAdapter(apiKey, baseURL string) *OpenAICompatibleAdapter {
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
		baseURL: baseURL,
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
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := o.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
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
		var errResp openaiErrorResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errResp); err == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("openai API returned status %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("openai API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go o.consumeSSE(ctx, resp, ch)
	return ch, nil
}

// consumeSSE reads SSE lines from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
func (o *OpenAICompatibleAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// Track in-flight tool calls by index for argument accumulation.
	toolCalls := make(map[int]*openaiToolCallState)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
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
			o.flushToolCalls(toolCalls, ch)
			return
		}

		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("parse chunk: %w", err)}
			return
		}

		for _, choice := range chunk.Choices {
			// Text content delta.
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				ch <- types.StreamEvent{
					Type: "text_delta",
					Text: *choice.Delta.Content,
				}
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
					ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)}
					return
				}
				state.argsBuf.WriteString(tc.Function.Arguments)
			}

			// finish_reason signals the end of this choice.
			if choice.FinishReason != nil {
				// Flush accumulated tool calls when the model is done.
				o.flushToolCalls(toolCalls, ch)

				stopReason := mapFinishReason(*choice.FinishReason)
				ev := types.StreamEvent{
					Type:       "message_complete",
					StopReason: stopReason,
				}
				if chunk.Usage != nil {
					ev.OutputTokens = chunk.Usage.CompletionTokens
				}
				ch <- ev
			}
		}
	}

	if err := scanner.Err(); err != nil {
		ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)}
	}
}

// flushToolCalls emits tool_call events for all accumulated tool calls and
// clears the state map. Called when the stream signals completion.
func (o *OpenAICompatibleAdapter) flushToolCalls(toolCalls map[int]*openaiToolCallState, ch chan<- types.StreamEvent) {
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
				ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool arguments JSON: %w", err)}
				return
			}
		}
		ch <- types.StreamEvent{
			Type:  "tool_call",
			ID:    state.id,
			Name:  state.name,
			Input: input,
		}
	}
	// Clear the map.
	for k := range toolCalls {
		delete(toolCalls, k)
	}
}
