package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
	maxToolInputSize    = 10 * 1024 * 1024 // 10 MB cap on streamed tool input JSON
)

// AuthMode selects the authentication header sent on every /v1/messages
// request. See docs/providers.md for why the two header shapes are not
// interchangeable.
type AuthMode int

const (
	// AuthModeAPIKey sends the credential in the x-api-key header (static keys).
	AuthModeAPIKey AuthMode = iota
	// AuthModeBearer sends the credential as Authorization: Bearer (WIF OAuth tokens).
	AuthModeBearer
)

// AnthropicAdapter implements ProviderAdapter for the Anthropic Messages API.
type AnthropicAdapter struct {
	bearer     credential.BearerTokenFunc
	authMode   AuthMode
	httpClient *http.Client
	baseURL    string // overridable for testing

	// AdapterDeps carries the factory-injected Tracer/Metrics/RetryPolicy/
	// Logger; see its doc comment for the field-by-field contract.
	AdapterDeps

	// Registry resolves per-(provider, model) quirks at the top of every
	// Stream call. Defaults to quirks.DefaultRegistry(); the nil-Registry
	// guard in Stream tolerates direct construction.
	Registry *quirks.Registry
}

// NewAnthropicAdapter creates an adapter for the Anthropic Messages API.
//
// bearer is invoked on every Stream call to fetch the current API key,
// letting refresh-aware credential sources (e.g. AnthropicWIFSource)
// rotate the token without rebuilding the adapter.
func NewAnthropicAdapter(bearer credential.BearerTokenFunc, authMode AuthMode) *AnthropicAdapter {
	return &AnthropicAdapter{
		bearer:   bearer,
		authMode: authMode,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		baseURL:  anthropicAPIURL,
		Registry: quirks.DefaultRegistry(),
	}
}

// AuthMode returns the configured authentication header mode.
func (a *AnthropicAdapter) AuthMode() AuthMode {
	return a.authMode
}

// anthropicRequest is the JSON body sent to the Anthropic Messages API.
//
// Temperature is *float64 with omitempty: nil omits the key entirely
// (Anthropic treats an explicit "temperature":0 as greedy decoding, not
// "use the service default"), while a non-nil pointer transmits the value
// verbatim including 0.0. buildAnthropicRequest forces it back to nil when
// BehaviourFlags.Anthropic.OmitSamplingParams is set.
//
// Messages is typed as []anthropicMessage, not []types.Message, to enforce
// a cross-provider confidentiality invariant — see docs/architecture.md
// (Provider adapters).
type anthropicRequest struct {
	Model       string                 `json:"model"`
	System      string                 `json:"system,omitempty"`
	Messages    []anthropicMessage     `json:"messages"`
	Tools       []types.ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature *float64               `json:"temperature,omitempty"`
	// ToolChoice is the Anthropic tool_choice object. A nil pointer omits
	// the field (the zero-value ToolChoiceAuto path). Populated only when
	// the resolved quirks advertise native support for the requested mode.
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Stream     bool                 `json:"stream"`
}

// anthropicToolChoice is the Anthropic Messages API tool_choice object.
// Type is one of "auto", "any", or "tool"; Name is set only for "tool".
// Anthropic has no native "none" — a no-tools turn is expressed by
// omitting the tools array — so ToolChoiceNone never produces this
// struct.
//
// DisableParallelToolUse forbids parallel tool calls; Anthropic has no
// top-level parallel field, so this rides on tool_choice instead. A nil
// pointer omits the key (parallel is Anthropic's default).
type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// applyAnthropicParallel folds a requested parallel-disable onto the
// tool_choice object. Only the disable direction is expressible — Anthropic's
// default is parallel-enabled — so a nil or =true StreamParams.ParallelToolCalls
// is a no-op. The structural "none" mode is left untouched: there are no tool
// calls to parallelise and synthesising auto would contradict the caller.
func applyAnthropicParallel(tc *anthropicToolChoice, params types.StreamParams, capability quirks.ParallelToolCallsCapability) *anthropicToolChoice {
	if params.ParallelToolCalls == nil || *params.ParallelToolCalls {
		return tc
	}
	if !capability.Supported || !capability.Disable {
		return tc
	}
	if tc == nil {
		if params.ToolChoice == types.ToolChoiceNone {
			return nil
		}
		tc = &anthropicToolChoice{Type: "auto"}
	}
	disable := true
	tc.DisableParallelToolUse = &disable
	return tc
}

// translateToolsAnthropic returns a fresh copy of the tool definitions with
// each tool's worked examples folded into its input_schema when the resolved
// capability supports it. The fresh slice preserves the no-aliasing guarantee
// buildAnthropicRequest relies on. Examples are advisory: a merge that cannot
// marshal leaves the schema as-is rather than failing the request.
func translateToolsAnthropic(tools []types.ToolDefinition, examples bool) []types.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]types.ToolDefinition, len(tools))
	copy(out, tools)
	if !examples {
		return out
	}
	for i := range out {
		if merged, err := mergeSchemaExamples(out[i].InputSchema, toolInputExamples(out[i])); err == nil {
			out[i].InputSchema = merged
		}
	}
	return out
}

// anthropicToolChoiceFromParams projects the provider-neutral
// StreamParams.ToolChoice onto the Anthropic tool_choice object, gated on
// the resolved capability. Returns nil — meaning "emit no field" — for the
// auto mode, for an unsupported mode, and for the structural "none" case
// (Anthropic expresses none by omitting tools, not via tool_choice).
func anthropicToolChoiceFromParams(params types.StreamParams, cap quirks.ToolChoiceCapability) *anthropicToolChoice {
	if !cap.Supported {
		return nil
	}
	switch params.ToolChoice {
	case types.ToolChoiceRequired:
		if !cap.Required {
			return nil
		}
		return &anthropicToolChoice{Type: "any"}
	case types.ToolChoiceTool:
		// A named-tool choice with no name is not expressible; treat it
		// as auto (emit nothing) rather than send an invalid object.
		if !cap.NamedTool || params.ToolChoiceName == "" {
			return nil
		}
		// Defense-in-depth: ToolChoiceName may carry a model-influenced
		// value, so reject any name outside the shared grammar and degrade
		// to auto rather than forward it.
		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("anthropic", params.Model, len(params.ToolChoiceName))
			return nil
		}
		return &anthropicToolChoice{Type: "tool", Name: params.ToolChoiceName}
	default:
		// ToolChoiceAuto and ToolChoiceNone both emit no tool_choice field:
		// auto is the wire default, and Anthropic has no native none. Since
		// this adapter does not also strip the tools array for "none", the
		// model may still emit a tool_use block; honouring none strictly
		// would require dropping tools from the request body, so "none" is
		// best-effort.
		return nil
	}
}

// anthropicMessage is the Anthropic-side wire shape for a single message.
// Locally defined so that the set of fields reaching api.anthropic.com is
// an explicit allowlist, not whatever types.ContentBlock happens to carry
// for some other provider's benefit. Adding a field here is an active
// decision that Anthropic's Messages API accepts it on the wire.
type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock is the allowlist of content-block fields the
// Anthropic Messages API accepts. Add a field only if Anthropic's API
// documents support for it; provider-private state on types.ContentBlock
// (added by other adapters) is dropped on egress to Anthropic by
// construction. See docs/architecture.md (Provider adapters).
//
// Content is a json.RawMessage rather than a string because the Messages
// API accepts a tool_result block's `content` as either a JSON string or an
// array of content blocks; the array form is emitted only when the resolved
// StructuredToolResults capability is on and the result carries a structured
// envelope. A nil Content omits the key.
type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// anthropicToolResultPart is one entry in the array form of a tool_result
// block's content. The Messages API's tool_result content array accepts
// "text" (and "image") parts; the harness uses only "text", carrying the
// structured envelope as a JSON-serialised text part alongside the canonical
// text part — there is no native JSON content type for tool_result.
type anthropicToolResultPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// translateMessagesAnthropic copies a slice of types.Message into the
// adapter-local anthropicMessage shape. It is the structural guard that
// enforces the cross-provider confidentiality invariant: any field on
// types.ContentBlock not mirrored onto anthropicContentBlock is dropped
// here, rather than relying on call sites to scrub egress.
func translateMessagesAnthropic(messages []types.Message, cap quirks.StructuredToolResultCapability) []anthropicMessage {
	out := make([]anthropicMessage, len(messages))
	for i, msg := range messages {
		blocks := make([]anthropicContentBlock, len(msg.Content))
		for j, b := range msg.Content {
			blocks[j] = anthropicContentBlock{
				Type:      b.Type,
				Text:      b.Text,
				ID:        b.ID,
				Name:      b.Name,
				Input:     b.Input,
				ToolUseID: b.ToolUseID,
				Content:   anthropicToolResultContent(b, cap),
				IsError:   b.IsError,
			}
		}
		out[i] = anthropicMessage{
			Role:    msg.Role,
			Content: blocks,
		}
	}
	return out
}

// anthropicToolResultContent renders the `content` field of one content
// block for the Anthropic wire. For non-tool_result blocks it returns nil so
// the key is omitted (text/tool_use blocks never carry tool-result content).
// For tool_result blocks it returns the JSON-string form by default; when the
// resolved capability accepts the content-block array shape and the block
// carries a structured envelope, it returns the array form — the canonical
// text part plus a text part holding the structured JSON — so the model
// receives both renderings and the text fallback survives even if a consumer
// reads only the first part.
//
// A marshalling failure falls back to the plain string content: structured
// serialisation is purely additive and must never drop the canonical text.
func anthropicToolResultContent(b types.ContentBlock, cap quirks.StructuredToolResultCapability) json.RawMessage {
	if b.Type != "tool_result" {
		return nil
	}
	// The array form requires a non-empty canonical text: an empty first part
	// ({"type":"text","text":""}) is meaningless, so an empty Content falls
	// through to the nil-return below regardless of the structured payload.
	if cap.Supported && cap.ContentBlockArray && len(b.Structured) > 0 && b.Content != "" {
		parts := []anthropicToolResultPart{
			{Type: "text", Text: b.Content},
			{Type: "text", Text: string(b.Structured)},
		}
		if raw, err := json.Marshal(parts); err == nil {
			return raw
		}
	}

	if b.Content == "" {
		return nil
	}
	raw, err := json.Marshal(b.Content)
	if err != nil {
		// json.Marshal on a string cannot fail in practice; emit an empty
		// JSON string so the wire shape stays valid.
		return json.RawMessage(`""`)
	}
	return raw
}

// SSE event types from the Anthropic API.
type sseContentBlockStart struct {
	Index        int             `json:"index"`
	ContentBlock sseContentBlock `json:"content_block"`
}

type sseContentBlock struct {
	Type  string          `json:"type"` // "text" | "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Text  string          `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type sseContentBlockDelta struct {
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type sseMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// buildAnthropicRequest projects a StreamParams into the Anthropic Messages
// wire body. The stream argument toggles the "stream" field so a future
// non-streaming (batch) caller can reuse the same projection.
//
// q carries the resolved per-(provider, model) quirks. A zero-value q (both
// Supported=false) emits no tool_choice field and string-only tool results,
// so callers that do not route through the registry get the baseline shape.
//
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. thinking_config), change the return type to
// (json.RawMessage, error) and apply a batch-specific projection here.
func buildAnthropicRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks) anthropicRequest {
	temperature := params.Temperature
	if q.BehaviourFlags.Anthropic.OmitSamplingParams {

		temperature = nil
	}
	return anthropicRequest{
		Model:    params.Model,
		System:   params.System,
		Messages: translateMessagesAnthropic(params.Messages, q.StructuredToolResults),

		Tools:       translateToolsAnthropic(params.Tools, q.ToolExamples.Supported),
		MaxTokens:   params.MaxTokens,
		Temperature: temperature,
		ToolChoice:  applyAnthropicParallel(anthropicToolChoiceFromParams(params, q.ToolChoice), params, q.ParallelToolCalls),
		Stream:      stream,
	}
}

// Stream sends a streaming request to the Anthropic Messages API and returns
// a channel of StreamEvents. The channel is closed when the stream ends or
// an error occurs. Cancelling the context terminates the stream.
func (a *AnthropicAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "anthropic"),
		attribute.String("provider.model", params.Model),
	)

	// Resolve quirks for this (provider, model) pair. Per design D4 the
	// resolution is per-stream. The nil-Registry guard tolerates callers
	// that build the adapter outside the factory without going through
	// NewAnthropicAdapter.
	registry := a.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("anthropic", params.Model)

	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Emitted even when the rules list is empty so an operator grepping
	// for the line knows the resolution ran.
	logger.DebugContext(ctx, "anthropic quirks resolved",
		slog.String("provider.type", "anthropic"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}

	// Without this warning, an operator who set RunConfig.Temperature
	// explicitly would silently observe it dropped. The suppressed value
	// itself is intentionally NOT logged.
	if q.BehaviourFlags.Anthropic.OmitSamplingParams && params.Temperature != nil {
		logger.WarnContext(ctx, "anthropic quirks suppressed caller temperature",
			slog.String("provider.type", "anthropic"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	reqBody := buildAnthropicRequest(params, true, q)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Resolve the bearer credential before issuing the HTTP request so a
	// failure in the credential layer is surfaced as a synchronous Stream
	// error rather than a half-built request with a missing header.
	apiKey, err := a.bearer(ctx)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("resolve bearer token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	switch a.authMode {
	case AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	default:
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	// DoWithRetry retries only this pre-stream call (connection errors, or a
	// 429/5xx on the initial response). It is never invoked again once the
	// channel below is returned, so a failure after streaming has begun is
	// never replayed — the same boundary the openai-compatible adapter
	// relies on.
	resp, err := DoWithRetry(ctx, a.httpClient, req, RetryOptions{
		Policy:       a.RetryPolicy,
		Logger:       a.Logger,
		Metrics:      a.Metrics,
		ProviderType: "anthropic",
		Model:        params.Model,
	})
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		// a.baseURL is operator-configurable and may carry a credential in
		// its query string; unwrap the *url.Error so its embedded URL (which
		// Go does not query-redact) never reaches a log or caller (CWE-532).
		return nil, fmt.Errorf("execute request: %w", security.UnwrapURLError(err))
	}

	if a.Tracer != nil {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		a.recordLatency(ctx, start, metricAttrs)
		if len(body) > 0 {
			return nil, fmt.Errorf("anthropic API returned status %d: %s", resp.StatusCode, body)
		}
		return nil, fmt.Errorf("anthropic API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		a.consumeSSE(ctx, resp, ch, start, metricAttrs)
		a.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (a *AnthropicAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if a.Metrics == nil {
		return
	}
	a.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads SSE events from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// streamStart and metricAttrs are forwarded for ProviderTTFB measurement: the
// first non-empty stream event observed marks "time to first byte" for this
// request. TTFB is recorded at most once per stream.
func (a *AnthropicAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// emitEvent sends an event on the output channel and records TTFB on the
	// first non-empty event observed. Closes around ttfbRecorded so each call
	// site does not need to check.
	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && a.Metrics != nil {
			a.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight content blocks by index for tool_use JSON accumulation.
	type blockState struct {
		blockType string
		id        string
		name      string
		jsonBuf   strings.Builder
	}
	blocks := make(map[int]*blockState)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxSSEScannerBuffer)
	var currentEvent string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		switch currentEvent {
		case "content_block_start":
			var cbs sseContentBlockStart
			if err := json.Unmarshal([]byte(data), &cbs); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_start: %w", err)})
				return
			}
			blocks[cbs.Index] = &blockState{
				blockType: cbs.ContentBlock.Type,
				id:        cbs.ContentBlock.ID,
				name:      cbs.ContentBlock.Name,
			}

		case "content_block_delta":
			var cbd sseContentBlockDelta
			if err := json.Unmarshal([]byte(data), &cbd); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_delta: %w", err)})
				return
			}
			bs := blocks[cbd.Index]
			if bs == nil {
				continue
			}
			switch cbd.Delta.Type {
			case "text_delta":
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: cbd.Delta.Text,
				})
			case "input_json_delta":
				if bs.jsonBuf.Len()+len(cbd.Delta.PartialJSON) > maxToolInputSize {
					emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool input exceeds %d byte limit", maxToolInputSize)})
					return
				}
				bs.jsonBuf.WriteString(cbd.Delta.PartialJSON)
			}

		case "content_block_stop":

			var stopData struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(data), &stopData); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_stop: %w", err)})
				return
			}
			bs := blocks[stopData.Index]
			if bs != nil && bs.blockType == "tool_use" {
				var input map[string]any
				raw := bs.jsonBuf.String()
				if raw != "" {
					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool input JSON: %w", err)})
						return
					}
				}
				emitEvent(types.StreamEvent{
					Type:  "tool_call",
					ID:    bs.id,
					Name:  bs.name,
					Input: input,
				})
			}
			delete(blocks, stopData.Index)

		case "message_delta":
			var md sseMessageDelta
			if err := json.Unmarshal([]byte(data), &md); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse message_delta: %w", err)})
				return
			}
			ev := types.StreamEvent{
				Type:       "message_complete",
				StopReason: md.Delta.StopReason,
			}
			if md.Usage != nil {
				ev.OutputTokens = md.Usage.OutputTokens
			}
			emitEvent(ev)

		case "message_stop":
			// Stream is done; the goroutine will exit and close the channel.
			return
		}

		currentEvent = ""
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}
