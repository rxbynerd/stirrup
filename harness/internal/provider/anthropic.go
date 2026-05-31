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
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
	maxToolInputSize    = 10 * 1024 * 1024 // 10 MB cap on streamed tool input JSON
)

// AuthMode selects the authentication header sent on every /v1/messages
// request. Anthropic's API accepts two header shapes that are NOT
// interchangeable:
//
//   - x-api-key: <token>            for static API keys (sk-ant-api03-...).
//   - Authorization: Bearer <token> for OAuth access tokens issued by the
//     WIF token-exchange flow (sk-ant-oat01-...).
//
// Sending a WIF access token via x-api-key returns a 401 from Anthropic;
// the discriminator is therefore load-bearing for issue #117 — the
// credential source returns a Bearer token either way, but the adapter
// must know which header to set.
type AuthMode int

const (
	// AuthModeAPIKey sends the credential in the x-api-key header.
	// Use for static API keys (sk-ant-api03-...). This is the default
	// to preserve compatibility with the static-key code path.
	AuthModeAPIKey AuthMode = iota
	// AuthModeBearer sends the credential as Authorization: Bearer.
	// Use for WIF OAuth access tokens (sk-ant-oat01-...) returned by
	// the AnthropicWIFSource credential-exchange flow.
	AuthModeBearer
)

// AnthropicAdapter implements ProviderAdapter for the Anthropic Messages API.
type AnthropicAdapter struct {
	bearer     credential.BearerTokenFunc
	authMode   AuthMode
	httpClient *http.Client
	baseURL    string                 // overridable for testing
	Tracer     oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics    *observability.Metrics // optional, set by factory for metric recording (nil means no recording)

	// Registry resolves per-(provider, model) quirks at the top of every
	// Stream call. The constructor seeds it with quirks.DefaultRegistry()
	// so callers that ignore the field still get the built-in rule set;
	// the nil-Registry guard in Stream tolerates direct construction. The
	// Anthropic adapter only reads the cross-provider ToolChoice
	// capability today (Anthropic has no wire-shape or sampling-param
	// divergences StreamParams cannot already express), but resolving
	// through the registry keeps the tool-choice gating consistent with
	// the OpenAI and Gemini adapters.
	Registry *quirks.Registry
}

// NewAnthropicAdapter creates an adapter for the Anthropic Messages API.
// The HTTP client is configured with explicit timeouts to prevent unbounded
// connections. The overall timeout is generous (120s) because streaming
// responses can be long-lived; transport-level timeouts are tighter.
//
// bearer is invoked on every Stream call to fetch the current API key —
// this lets refresh-aware credential sources (e.g. AnthropicWIFSource)
// rotate the token without rebuilding the adapter. Static sources return
// a captured value with no IO, so the per-request call is effectively
// free.
//
// authMode selects which HTTP header carries the credential. The factory
// passes AuthModeBearer when credential.type=anthropic-wif (the credential
// source returns short-lived OAuth access tokens), and AuthModeAPIKey for
// every other code path (static API key from secret://).
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

// AuthMode returns the configured authentication header mode. Exported
// for tests in adjacent packages (e.g. core/factory_test.go) that need
// to assert the factory wires the correct mode for WIF vs static credentials.
func (a *AnthropicAdapter) AuthMode() AuthMode {
	return a.authMode
}

// anthropicRequest is the JSON body sent to the Anthropic Messages API.
//
// Temperature is *float64 with omitempty: a nil pointer omits the key
// entirely (Anthropic treats an explicit "temperature":0 as a request for
// greedy decoding rather than "use the service default", so a caller who
// never set a temperature must not be pinned to greedy decoding via the
// wire shape). A non-nil pointer transmits the dereferenced value
// verbatim, including an explicit 0.0 for greedy decoding. This mirrors
// the upstream StreamParams.Temperature pointer type so the
// unset-vs-explicit-zero distinction survives marshalling.
//
// Messages is typed as []anthropicMessage, not []types.Message, to
// enforce a cross-provider confidentiality invariant: each adapter owns
// its egress wire type, so no field that another provider populates on
// types.ContentBlock can be transmitted to Anthropic by accident.
// types.ContentBlock is a shared carrier — fields accumulate on it as
// providers need round-trip state — which means the egress shape must
// be enforced at the adapter, not the carrier. #194 (Vertex's
// thought_signature leaking via the model router) was the motivating
// incident; the pattern generalises to any provider-private state added
// later, and matches what every other adapter already does.
type anthropicRequest struct {
	Model       string                 `json:"model"`
	System      string                 `json:"system,omitempty"`
	Messages    []anthropicMessage     `json:"messages"`
	Tools       []types.ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature *float64               `json:"temperature,omitempty"`
	// ToolChoice is the Anthropic tool_choice object. A nil pointer omits
	// the field entirely so a request that does not pin tool choice is
	// byte-identical to the pre-#230 shape; this is the zero-value
	// ToolChoiceAuto path. Populated only when the resolved quirks
	// advertise native support for the requested mode.
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Stream     bool                 `json:"stream"`
}

// anthropicToolChoice is the Anthropic Messages API tool_choice object.
// Type is one of "auto", "any", or "tool"; Name is set only for "tool".
// Anthropic has no native "none" — a no-tools turn is expressed by
// omitting the tools array — so ToolChoiceNone never produces this
// struct.
//
// DisableParallelToolUse carries the #222 parallel-tool-call control.
// Anthropic has no top-level parallel field: forbidding parallel tool calls
// is expressed only as this flag on the tool_choice object. A nil pointer
// omits the key (parallel is Anthropic's default); applyAnthropicParallel sets
// it true when the caller requested no-parallel, synthesising an auto
// tool_choice when none was otherwise produced.
type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// applyAnthropicParallel folds a requested parallel-disable (#222) onto the
// tool_choice object. Only the disable direction is expressible — Anthropic's
// default is parallel-enabled — so a nil or =true StreamParams.ParallelToolCalls
// is a no-op. When disable is requested and the capability supports it, the
// flag rides on the tool_choice object, synthesising an auto object if the
// tool-choice projection produced none. The structural "none" mode is left
// untouched: there are no tool calls to parallelise and synthesising auto
// would contradict the caller.
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
// each tool's worked examples (#222) folded into its input_schema when the
// resolved capability supports it. It replaces a bare slices.Clone: the
// Anthropic adapter serialises types.ToolDefinition onto the wire (Presentation
// carries json:"-"), so examples must be merged into the schema explicitly.
// The fresh slice preserves the no-aliasing guarantee buildAnthropicRequest
// relies on. Anthropic has no strict-mode subset restriction, so examples fold
// whenever the capability is on. Examples are advisory: a merge that cannot
// marshal (not reachable for a map just unmarshalled) leaves the schema as-is
// rather than failing the request — the description still carries the example.
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
		// Defense-in-depth at the wire boundary (#230 B3): A2 will feed
		// model-influenced names through ToolChoiceName, so reject any
		// name outside the providers' shared grammar and degrade to auto.
		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("anthropic", params.Model, len(params.ToolChoiceName))
			return nil
		}
		return &anthropicToolChoice{Type: "tool", Name: params.ToolChoiceName}
	default:
		// ToolChoiceAuto (zero value) and ToolChoiceNone both emit no
		// tool_choice field: auto is the wire default, and Anthropic has
		// no native none.
		//
		// Degradation of ToolChoiceNone: this adapter does not also strip
		// the tools array, so a "none" turn still ships the tool
		// definitions with no tool_choice field. Anthropic therefore
		// treats it as auto and the model may emit a tool_use block the
		// caller asked to suppress. Honouring none strictly would require
		// dropping tools from the request body (a larger change in
		// buildAnthropicRequest); until then "none" is best-effort.
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
// Anthropic Messages API accepts. types.ContentBlock is the shared carrier
// across providers and may grow new fields over time to hold opaque
// per-provider state (e.g. ThoughtSignature, added for Vertex in #194);
// any such field stays absent here by construction. The rule for
// extending this struct is: add a field only if Anthropic's API
// documents support for it. Provider-private state added to
// types.ContentBlock by some other adapter is, by default, dropped on
// egress to Anthropic.
// Content is a json.RawMessage rather than a string because the Messages
// API accepts a tool_result block's `content` as either a JSON string or an
// array of content blocks. The string form is the default (a JSON-encoded
// string literal); the array form is emitted only when the resolved
// StructuredToolResults capability is on and the result carries a structured
// envelope (issue #231 B2). A nil Content omits the key (omitempty), so a
// text block or tool_use block — which never set Content — serialises
// byte-identically to the pre-#231 shape.
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
// text part. There is no native JSON content type for tool_result, so this
// is the faithful representation of "structured data the model can read".
type anthropicToolResultPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// translateMessagesAnthropic copies a slice of types.Message into the
// adapter-local anthropicMessage shape. It is the structural guard that
// enforces the cross-provider confidentiality invariant: any field on
// types.ContentBlock that is not mirrored onto anthropicContentBlock is
// dropped here, rather than relying on call sites to scrub egress.
// #194 was the motivating incident (Vertex's thought_signature would
// otherwise have been forwarded to api.anthropic.com via the model
// router); the same mechanism covers any provider-private field added
// to the shared carrier in the future.
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
	// ({"type":"text","text":""}) is meaningless and diverges from the
	// pre-#231 shape for empty-content results (which omitted the content key
	// entirely). When Content is empty, fall through to the nil-return below
	// regardless of the structured payload.
	if cap.Supported && cap.ContentBlockArray && len(b.Structured) > 0 && b.Content != "" {
		parts := []anthropicToolResultPart{
			{Type: "text", Text: b.Content},
			{Type: "text", Text: string(b.Structured)},
		}
		if raw, err := json.Marshal(parts); err == nil {
			return raw
		}
	}
	// Preserve the pre-#231 omitempty behaviour: an empty tool-result
	// content omitted the `content` key entirely. Returning nil keeps that
	// byte-for-byte. A non-empty string is emitted as a JSON string literal,
	// identical to what the old `Content string` field produced.
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
// non-streaming caller (batch submission, phase 2 of issue #133) can reuse
// the same projection without duplicating field-by-field copying.
//
// q carries the resolved per-(provider, model) quirks. Two cross-provider
// capabilities are load-bearing on the Anthropic build path: ToolChoice gates
// whether StreamParams.ToolChoice is projected onto the tool_choice object,
// and StructuredToolResults gates whether a tool_result block carrying a
// structured envelope is serialised as the content-block array form rather
// than the plain string. A zero-value q (both Supported=false) emits no
// tool_choice field and string-only tool results, so callers that do not
// route through the registry produce the pre-#230/#231 wire shape.
//
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. thinking_config), change the return type to
// (json.RawMessage, error) and apply a batch-specific projection here.
func buildAnthropicRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks) anthropicRequest {
	return anthropicRequest{
		Model:    params.Model,
		System:   params.System,
		Messages: translateMessagesAnthropic(params.Messages, q.StructuredToolResults),
		// translateToolsAnthropic allocates a fresh slice (so a caller mutating
		// params.Tools cannot race the returned struct, as the phase-2 batch
		// caller requires) and folds #222 worked examples into each tool's
		// input_schema when the resolved capability supports it.
		Tools:       translateToolsAnthropic(params.Tools, q.ToolExamples.Supported),
		MaxTokens:   params.MaxTokens,
		Temperature: params.Temperature,
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
	q := registry.Resolve("anthropic", params.Model)

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
	// Header selection per AuthMode (issue #117 BLOCKING B2). Anthropic's
	// /v1/messages accepts x-api-key for static API keys but requires
	// Authorization: Bearer for WIF OAuth access tokens; sending a WIF
	// token via x-api-key returns 401. Both modes pin the same
	// anthropic-version header.
	switch a.authMode {
	case AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	default:
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// Record HTTP-level metadata on the span from context (the provider.stream
	// span created by the loop), when OTel instrumentation is enabled.
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
			// Parse the index from the data to find which block stopped.
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
