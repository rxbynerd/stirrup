package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// OpenAIResponsesAdapter implements ProviderAdapter for the OpenAI Responses
// API (POST /v1/responses). The Responses API is a separate endpoint from
// Chat Completions with a different request/response shape: a top-level
// "instructions" field replaces the system message, conversation history is
// expressed as a typed "input" array (message / function_call /
// function_call_output items), tools use a flatter schema, and the streaming
// SSE protocol uses named events such as "response.output_text.delta" and
// "response.function_call_arguments.delta".
//
// The two adapters are kept separate (rather than auto-detecting) because
// users who have explicitly opted into one or the other shape want it to be
// honoured deterministically — silent fallback would mask configuration
// errors and complicate observability.
type OpenAIResponsesAdapter struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
	Tracer     oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics    *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
}

// NewOpenAIResponsesAdapter creates an adapter for the OpenAI Responses API.
// The baseURL should be the API root (e.g. "https://api.openai.com/v1");
// the /responses path is appended automatically. Pass an empty string for
// the default OpenAI URL — kept consistent with NewOpenAICompatibleAdapter
// so callers can switch the provider type without re-deriving the URL.
func NewOpenAIResponsesAdapter(apiKey, baseURL string) *OpenAIResponsesAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIResponsesAdapter{
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

// --- Responses API wire format ---

// responsesRequest is the JSON body sent to POST /v1/responses.
//
// The `store` field is set explicitly to false: stirrup manages its own
// conversation history and does not rely on OpenAI-side state. Leaving
// `store` unset would default to server-side persistence on some endpoints
// (a privacy concern for self-hosted gateways and a billing concern for
// long-running runs).
type responsesRequest struct {
	Model           string           `json:"model"`
	Instructions    string           `json:"instructions,omitempty"`
	Input           []responsesInput `json:"input"`
	Tools           []responsesTool  `json:"tools,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Temperature     float64          `json:"temperature"`
	Stream          bool             `json:"stream"`
	Store           bool             `json:"store"`
}

// responsesInput is one item in the Responses API input array. The Type
// field selects which other fields are populated; this matches the
// discriminated-union shape OpenAI publishes for typed input items.
type responsesInput struct {
	Type      string                  `json:"type"`                // "message" | "function_call" | "function_call_output"
	Role      string                  `json:"role,omitempty"`      // for "message"
	Content   []responsesContentBlock `json:"content,omitempty"`   // for "message"
	Name      string                  `json:"name,omitempty"`      // for "function_call"
	CallID    string                  `json:"call_id,omitempty"`   // for "function_call" / "function_call_output"
	Arguments string                  `json:"arguments,omitempty"` // for "function_call" — JSON string
	Output    string                  `json:"output,omitempty"`    // for "function_call_output"
}

// responsesContentBlock is one part inside a message item.
// OpenAI uses "input_text" for user/system messages and "output_text" for
// assistant messages — the asymmetry is part of their wire format.
type responsesContentBlock struct {
	Type string `json:"type"` // "input_text" | "output_text"
	Text string `json:"text"`
}

// responsesTool describes a tool in the Responses API's flatter format.
// (Compare with Chat Completions, which nests under a "function" object.)
type responsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// --- SSE event payload types ---

// responsesOutputItem is a single item in the response.output array.
// Streaming events deliver these incrementally via response.output_item.added
// and response.output_item.done.
type responsesOutputItem struct {
	Type      string `json:"type"` // "message" | "function_call" | "reasoning"
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"`
}

// responsesUsage tracks token usage on response.completed.
type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

// responsesResponse is the response object delivered on response.completed
// and response.incomplete. Only the fields stirrup acts on are unmarshalled.
type responsesResponse struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Output []responsesOutputItem `json:"output,omitempty"`
	Usage  *responsesUsage       `json:"usage,omitempty"`
}

// responsesErrorResponse is the error JSON returned for non-2xx responses.
// OpenAI uses the same envelope across Chat Completions and Responses.
type responsesErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// responsesCallState tracks an in-flight function call assembled across
// multiple SSE events. function_call_arguments.delta carries text fragments
// keyed by item_id (or output_index when item_id is absent on partner
// gateways); we accumulate them in argsBuf and flush once on the matching
// .done event.
type responsesCallState struct {
	itemID    string
	outputIdx int
	callID    string
	name      string
	argsBuf   strings.Builder
	done      bool // set when arguments.done or output_item.done has fired
	emitted   bool // emitted at most once even if both done events fire
}

// --- Message translation ---

// translateMessagesResponses converts stirrup's []types.Message format into
// the Responses API's typed input[] array. The system prompt is NOT placed
// in input[] — it goes into the top-level "instructions" field, which is
// returned separately.
//
// Tool calls and tool results become standalone function_call /
// function_call_output items (rather than being attached to the assistant
// message), matching the Responses API's model.
func translateMessagesResponses(messages []types.Message) []responsesInput {
	var out []responsesInput

	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			var textParts []string
			var calls []responsesInput

			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_use":
					args := string(block.Input)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, responsesInput{
						Type:      "function_call",
						CallID:    block.ID,
						Name:      block.Name,
						Arguments: args,
					})
				}
			}

			if len(textParts) > 0 {
				out = append(out, responsesInput{
					Type: "message",
					Role: "assistant",
					Content: []responsesContentBlock{
						{Type: "output_text", Text: strings.Join(textParts, "")},
					},
				})
			}
			out = append(out, calls...)

		case "user":
			var textParts []string
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_result":
					content := block.Content
					if block.IsError {
						content = "Error: " + content
					}
					out = append(out, responsesInput{
						Type:   "function_call_output",
						CallID: block.ToolUseID,
						Output: content,
					})
				}
			}
			if len(textParts) > 0 {
				out = append(out, responsesInput{
					Type: "message",
					Role: "user",
					Content: []responsesContentBlock{
						{Type: "input_text", Text: strings.Join(textParts, "")},
					},
				})
			}
		}
	}

	return out
}

// translateToolsResponses converts stirrup ToolDefinitions into the
// Responses API's flatter tool schema. Unlike Chat Completions, there is no
// nested "function" object — the name/description/parameters live directly
// on the tool item.
func translateToolsResponses(tools []types.ToolDefinition) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, len(tools))
	for i, t := range tools {
		out[i] = responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

// Stream sends a streaming request to the OpenAI Responses API and returns
// a channel of StreamEvents. The channel is closed when the stream ends or
// an error occurs. Cancelling the context terminates the stream.
func (o *OpenAIResponsesAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "openai-responses"),
		attribute.String("provider.model", params.Model),
	)

	reqBody := responsesRequest{
		Model:           params.Model,
		Instructions:    params.System,
		Input:           translateMessagesResponses(params.Messages),
		Tools:           translateToolsResponses(params.Tools),
		MaxOutputTokens: params.MaxTokens,
		Temperature:     params.Temperature,
		Stream:          true,
		Store:           false,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := o.baseURL + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

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
		var errResp responsesErrorResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errResp); err == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("openai responses API returned status %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("openai responses API returned status %d", resp.StatusCode)
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
func (o *OpenAIResponsesAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if o.Metrics == nil {
		return
	}
	o.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads named-event SSE records from the response body and
// dispatches them. Records are separated by blank lines and may contain
// `event: <name>` and `data: <payload>` fields. Unlike the Chat Completions
// adapter (which only reads `data:` lines), Responses streaming relies on
// the event name to disambiguate payloads — there is no `[DONE]` sentinel.
func (o *OpenAIResponsesAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && o.Metrics != nil {
			o.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight function calls. Indexed by a stable key (item_id when
	// present, falling back to a stringified output_index). The value's
	// outputIdx field is preserved so we can flush in deterministic order.
	calls := make(map[string]*responsesCallState)

	scanner := bufio.NewScanner(resp.Body)
	// Increase the buffer ceiling so a single SSE record carrying a large
	// JSON payload (e.g. a response.completed envelope) does not trip
	// bufio's default 64KB scanner limit.
	scanner.Buffer(make([]byte, 0, 64*1024), openaiMaxToolInputSize)

	var currentEvent string
	var dataParts []string

	flushRecord := func() bool {
		// Reset event-record state on return; defer-style guard so any
		// early return below still leaves a clean slate. We do this
		// via explicit assignment because we need the captured values
		// inside the dispatch.
		eventName := currentEvent
		data := strings.Join(dataParts, "\n")
		currentEvent = ""
		dataParts = dataParts[:0]

		if eventName == "" || data == "" {
			return true
		}
		return o.dispatchEvent(eventName, data, calls, emitEvent)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		// Blank line terminates an SSE record. Process the accumulated
		// event/data and reset.
		if line == "" {
			if !flushRecord() {
				return
			}
			continue
		}

		// SSE comments start with ":" and must be ignored.
		if strings.HasPrefix(line, ":") {
			continue
		}

		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimPrefix(line, "event:")
		case strings.HasPrefix(line, "data: "):
			dataParts = append(dataParts, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, "data:"):
			dataParts = append(dataParts, strings.TrimPrefix(line, "data:"))
		}
	}

	// Flush any trailing record without a terminating blank line. A
	// well-behaved server will not do this, but tolerating it avoids
	// dropped final events on premature EOF.
	if currentEvent != "" || len(dataParts) > 0 {
		_ = flushRecord()
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}

// dispatchEvent handles a single completed SSE record. It returns false to
// signal the caller to stop reading (terminal event), true to continue.
func (o *OpenAIResponsesAdapter) dispatchEvent(name, data string, calls map[string]*responsesCallState, emit func(types.StreamEvent)) bool {
	switch name {
	case "response.created":
		// Optional metadata; nothing to emit.
		return true

	case "response.output_item.added":
		var payload struct {
			OutputIndex int                 `json:"output_index"`
			Item        responsesOutputItem `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_item.added: %w", err)})
			return false
		}
		if payload.Item.Type == "function_call" {
			key := callKey(payload.Item.ID, payload.OutputIndex)
			st, exists := calls[key]
			if !exists {
				st = &responsesCallState{
					itemID:    payload.Item.ID,
					outputIdx: payload.OutputIndex,
				}
				calls[key] = st
			}
			if payload.Item.CallID != "" {
				st.callID = payload.Item.CallID
			}
			if payload.Item.Name != "" {
				st.name = payload.Item.Name
			}
			// Some providers include the (complete) arguments string here
			// when the model emitted the call atomically. Seed argsBuf
			// from it so we still produce a tool_call on .done.
			if payload.Item.Arguments != "" {
				st.argsBuf.WriteString(payload.Item.Arguments)
			}
		}
		return true

	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_text.delta: %w", err)})
			return false
		}
		if payload.Delta != "" {
			emit(types.StreamEvent{Type: "text_delta", Text: payload.Delta})
		}
		return true

	case "response.function_call_arguments.delta":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse function_call_arguments.delta: %w", err)})
			return false
		}
		key := callKey(payload.ItemID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			// .added not yet observed for this call (or was dropped);
			// create a state placeholder so we still accumulate.
			st = &responsesCallState{
				itemID:    payload.ItemID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		if st.argsBuf.Len()+len(payload.Delta) > openaiMaxToolInputSize {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
			return false
		}
		st.argsBuf.WriteString(payload.Delta)
		return true

	case "response.function_call_arguments.done":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Arguments   string `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse function_call_arguments.done: %w", err)})
			return false
		}
		key := callKey(payload.ItemID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			st = &responsesCallState{
				itemID:    payload.ItemID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		// If the .done event echoes the full arguments string and the
		// streamed deltas are missing, prefer the echoed copy.
		if st.argsBuf.Len() == 0 && payload.Arguments != "" {
			if len(payload.Arguments) > openaiMaxToolInputSize {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
				return false
			}
			st.argsBuf.WriteString(payload.Arguments)
		}
		st.done = true
		if !flushOneCall(st, emit) {
			return false
		}
		return true

	case "response.output_item.done":
		var payload struct {
			OutputIndex int                 `json:"output_index"`
			Item        responsesOutputItem `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_item.done: %w", err)})
			return false
		}
		if payload.Item.Type != "function_call" {
			return true
		}
		key := callKey(payload.Item.ID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			st = &responsesCallState{
				itemID:    payload.Item.ID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		if payload.Item.CallID != "" {
			st.callID = payload.Item.CallID
		}
		if payload.Item.Name != "" {
			st.name = payload.Item.Name
		}
		// If the .done event carries the full arguments string and the
		// streamed deltas were never seen, prefer the echoed copy.
		if st.argsBuf.Len() == 0 && payload.Item.Arguments != "" {
			if len(payload.Item.Arguments) > openaiMaxToolInputSize {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
				return false
			}
			st.argsBuf.WriteString(payload.Item.Arguments)
		}
		st.done = true
		if !flushOneCall(st, emit) {
			return false
		}
		return true

	case "response.completed":
		// Flush any tool calls that never received a .done event (defensive).
		if !flushPendingCalls(calls, emit) {
			return false
		}
		var payload struct {
			Response responsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse response.completed: %w", err)})
			return false
		}
		ev := types.StreamEvent{
			Type:       "message_complete",
			StopReason: deriveStopReason(payload.Response),
		}
		if payload.Response.Usage != nil {
			ev.OutputTokens = payload.Response.Usage.OutputTokens
		}
		emit(ev)
		return false

	case "response.incomplete":
		if !flushPendingCalls(calls, emit) {
			return false
		}
		var payload struct {
			Response responsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse response.incomplete: %w", err)})
			return false
		}
		stop := "incomplete"
		if payload.Response.IncompleteDetails != nil {
			reason := payload.Response.IncompleteDetails.Reason
			// "max_output_tokens" / "max_tokens" both map to our existing
			// max_tokens stop reason. Anything else passes through verbatim
			// for diagnostic visibility.
			if reason == "max_output_tokens" || reason == "max_tokens" {
				stop = "max_tokens"
			} else if reason != "" {
				stop = reason
			}
		}
		ev := types.StreamEvent{
			Type:       "message_complete",
			StopReason: stop,
		}
		if payload.Response.Usage != nil {
			ev.OutputTokens = payload.Response.Usage.OutputTokens
		}
		emit(ev)
		return false

	case "response.failed":
		var payload struct {
			Response struct {
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
				Status string `json:"status"`
			} `json:"response"`
		}
		_ = json.Unmarshal([]byte(data), &payload)
		msg := "openai responses API: response failed"
		if payload.Response.Error != nil && payload.Response.Error.Message != "" {
			msg = "openai responses API: " + payload.Response.Error.Message
		}
		emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("%s", msg)})
		return false

	case "error":
		var payload struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		_ = json.Unmarshal([]byte(data), &payload)
		msg := "openai responses API stream error"
		if payload.Message != "" {
			msg = "openai responses API stream error: " + payload.Message
		}
		emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("%s", msg)})
		return false

	default:
		// Forward-compatible: unknown events (e.g. reasoning summaries,
		// content_part lifecycle, partial-image deltas) are ignored.
		return true
	}
}

// callKey produces a stable identifier for a function call. item_id is
// preferred when the server provides one; falling back to output_index keeps
// us robust against partner gateways that omit it.
func callKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return itemID
	}
	return fmt.Sprintf("idx:%d", outputIndex)
}

// flushOneCall emits a tool_call event for the supplied call state, parsing
// its accumulated arguments JSON. Returns false on a fatal parse error
// (caller should stop reading the stream). Marks the call as emitted so a
// duplicate .done event does not fire it twice.
func flushOneCall(st *responsesCallState, emit func(types.StreamEvent)) bool {
	if st.emitted {
		return true
	}
	st.emitted = true
	var input map[string]any
	raw := st.argsBuf.String()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool arguments JSON: %w", err)})
			return false
		}
	}
	emit(types.StreamEvent{
		Type:  "tool_call",
		ID:    st.callID,
		Name:  st.name,
		Input: input,
	})
	return true
}

// flushPendingCalls emits any tool calls that were left in flight when the
// terminal response.completed / response.incomplete event arrived. Order is
// stable by output_index so multi-call responses are deterministic.
func flushPendingCalls(calls map[string]*responsesCallState, emit func(types.StreamEvent)) bool {
	pending := make([]*responsesCallState, 0, len(calls))
	for _, st := range calls {
		if !st.emitted {
			pending = append(pending, st)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].outputIdx < pending[j].outputIdx
	})
	for _, st := range pending {
		if !flushOneCall(st, emit) {
			return false
		}
	}
	return true
}

// deriveStopReason maps a Responses API response object to stirrup's stop
// reason vocabulary. Tool calls take precedence over plain end_turn so the
// agentic loop knows to dispatch tools before treating the turn as final.
func deriveStopReason(resp responsesResponse) string {
	hasTool := false
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			hasTool = true
			break
		}
	}
	switch resp.Status {
	case "completed":
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	case "incomplete":
		if resp.IncompleteDetails != nil {
			r := resp.IncompleteDetails.Reason
			if r == "max_output_tokens" || r == "max_tokens" {
				return "max_tokens"
			}
			if r != "" {
				return r
			}
		}
		return "incomplete"
	default:
		if resp.Status != "" {
			return resp.Status
		}
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	}
}
