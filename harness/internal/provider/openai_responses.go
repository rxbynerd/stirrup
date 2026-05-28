package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
//
// Azure Foundry's "/openai/v1/responses" endpoint is wire-compatible with
// the OpenAI Responses request/response body, SSE event names, tool schema,
// and previous_response_id semantics. It is supported by pointing BaseURL
// at the Azure resource ("https://<resource>.openai.azure.com/openai/v1"),
// setting APIKeyHeader to "api-key" when authenticating with a plain Azure
// OpenAI key (Entra ID bearer tokens still work with the empty default),
// and adding the required api-version through QueryParams (e.g.
// {"api-version": "preview"}). Azure-only Responses extensions such as
// server-side state and content_part lifecycle events ride the same
// forward-compatible "unknown SSE event" path implemented in dispatchEvent.
type OpenAIResponsesAdapter struct {
	bearer       credential.BearerTokenFunc
	httpClient   *http.Client
	baseURL      string
	apiKeyHeader string
	queryParams  map[string]string
	Tracer       oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics      *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
	Logger       *slog.Logger           // optional, set by factory; nil falls back to slog.Default()
	// Registry resolves per-(provider, model) quirks at the top of
	// every Stream call. No rules target openai-responses in v1; the
	// field exists so the integration point is in place when a
	// Responses-specific divergence is added (design §7 Step 4). The
	// constructor defaults it to quirks.DefaultRegistry().
	Registry *quirks.Registry

	// strictSchemas memoises strict-mode schema rewrites within this
	// adapter's lifetime. See OpenAICompatibleAdapter.strictSchemas
	// for the full rationale; the Responses path shares the same cache
	// shape because the underlying schema rewrite is identical.
	strictSchemas *strictSchemaCache
}

// NewOpenAIResponsesAdapter creates an adapter for the OpenAI Responses API.
// The baseURL should be the API root (e.g. "https://api.openai.com/v1");
// the /responses path is appended automatically. Pass an empty string for
// the default OpenAI URL — kept consistent with NewOpenAICompatibleAdapter
// so callers can switch the provider type without re-deriving the URL. The
// auth argument carries optional header-name and query-parameter overrides;
// pass a zero value for OpenAI-default behaviour.
//
// bearer is invoked on every Stream call to fetch the current API key.
// See NewOpenAICompatibleAdapter for the closure contract; both adapters
// share the same auth shape so swapping provider.type does not require
// reconfiguring credentials.
func NewOpenAIResponsesAdapter(bearer credential.BearerTokenFunc, baseURL string, auth OpenAIAuthConfig) *OpenAIResponsesAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIResponsesAdapter{
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
		Registry:      quirks.DefaultRegistry(),
		strictSchemas: newStrictSchemaCache(),
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
	// Temperature is *float64 with omitempty: a nil pointer omits the
	// key entirely (the Responses API rejects "temperature" outright on
	// reasoning models — the same class-wide rejection that motivated
	// #200 on the Chat Completions adapter). A non-nil pointer transmits
	// the dereferenced value verbatim, including an explicit 0.0 for
	// greedy decoding. This mirrors the upstream StreamParams.Temperature
	// pointer type so the unset-vs-explicit-zero distinction survives
	// marshalling.
	Temperature *float64 `json:"temperature,omitempty"`
	// Stream carries omitempty so that buildResponsesRequest, which leaves
	// the field at its zero value, produces a wire body with no "stream"
	// key at all. The streaming caller sets reqBody.Stream = true after
	// the builder returns, which serialises "stream":true. A future batch
	// caller can marshal the helper output directly and be sure the field
	// is absent — the Anthropic Messages Batches API explicitly rejects
	// the field; the Responses batch endpoint's contract is unverified
	// but omission is the safer default until that verification lands.
	Stream bool `json:"stream,omitempty"`
	Store  bool `json:"store"`
	// ParallelToolCalls carries the top-level parallel_tool_calls bool (#222),
	// shared with the Chat Completions API. A nil pointer omits the key so the
	// body is byte-identical to the pre-#222 shape; buildResponsesRequest sets
	// it only when the caller requested the control and the resolved capability
	// advertises support.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// responsesInput is one item in the Responses API input array. The Type
// field selects which other fields are populated; this matches the
// discriminated-union shape OpenAI publishes for typed input items.
//
// The struct keeps an ergonomic flat shape so construction-site code
// (translateMessagesResponses and friends) can build items without
// branching on type. MarshalJSON below switches on Type and emits only
// the keys valid for that variant, via per-type wire structs. This is
// the structural fix for #199: stricter validators (Azure OpenAI's
// Responses endpoint) reject "output":"" on message / function_call
// items even though upstream OpenAI tolerates it.
//
// The per-variant wire shapes preserve the #172 invariant: the
// function_call_output wire struct's Output field has no omitempty, so
// the "output" key is always present on function_call_output items
// even when the value is the empty string.
type responsesInput struct {
	Type      string                  `json:"-"` // "message" | "function_call" | "function_call_output"
	Role      string                  `json:"-"` // for "message"
	Content   []responsesContentBlock `json:"-"` // for "message"
	Name      string                  `json:"-"` // for "function_call"
	CallID    string                  `json:"-"` // for "function_call" / "function_call_output"
	Arguments string                  `json:"-"` // for "function_call" — JSON string
	Output    string                  `json:"-"` // for "function_call_output" — required even when empty
}

// MarshalJSON emits only the wire fields valid for the input item's Type
// discriminant. Each Type maps to a dedicated wire struct so a future
// edit cannot accidentally leak a field across variants — the original
// shared-struct shape silently emitted "output":"" on every variant,
// which #199 surfaced as an Azure OpenAI HTTP 400.
//
// function_call_output is the variant that requires the "output" key
// even when its value is the empty string (see #172). Its wire struct's
// Output field therefore has no omitempty.
func (r responsesInput) MarshalJSON() ([]byte, error) {
	switch r.Type {
	case "message":
		return json.Marshal(responsesMessageInputWire{
			Type:    r.Type,
			Role:    r.Role,
			Content: r.Content,
		})
	case "function_call":
		return json.Marshal(responsesFunctionCallInputWire{
			Type:      r.Type,
			CallID:    r.CallID,
			Name:      r.Name,
			Arguments: r.Arguments,
		})
	case "function_call_output":
		return json.Marshal(responsesFunctionCallOutputInputWire{
			Type:   r.Type,
			CallID: r.CallID,
			Output: r.Output,
		})
	default:
		// Unknown variants would previously have been serialised as a
		// pile of empty-string keys. Surfacing the type explicitly makes
		// the failure mode debuggable rather than a silent wire-format
		// mismatch.
		return nil, fmt.Errorf("responsesInput: unknown type %q", r.Type)
	}
}

// UnmarshalJSON is the inverse of MarshalJSON: it accepts any of the
// per-type wire shapes and populates the ergonomic flat struct so test
// roundtrips and any future caller-side parsing continues to work. The
// adapter itself never decodes request bodies — this exists for symmetry
// and to keep tests that send-then-receive a request able to inspect the
// shape through the same struct that built it.
func (r *responsesInput) UnmarshalJSON(data []byte) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	r.Type = head.Type
	switch head.Type {
	case "message":
		var w responsesMessageInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.Role = w.Role
		r.Content = w.Content
	case "function_call":
		var w responsesFunctionCallInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.CallID = w.CallID
		r.Name = w.Name
		r.Arguments = w.Arguments
	case "function_call_output":
		var w responsesFunctionCallOutputInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.CallID = w.CallID
		r.Output = w.Output
	default:
		return fmt.Errorf("responsesInput: unknown type %q", head.Type)
	}
	return nil
}

// responsesMessageInputWire is the wire shape for type=="message".
// Only type/role/content are valid for this variant.
type responsesMessageInputWire struct {
	Type    string                  `json:"type"`
	Role    string                  `json:"role,omitempty"`
	Content []responsesContentBlock `json:"content,omitempty"`
}

// responsesFunctionCallInputWire is the wire shape for type=="function_call".
// Only type/call_id/name/arguments are valid for this variant.
type responsesFunctionCallInputWire struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// responsesFunctionCallOutputInputWire is the wire shape for
// type=="function_call_output". Output deliberately lacks omitempty: the
// Responses API rejects function_call_output items missing the "output"
// key with HTTP 400 (Missing required parameter: 'input[N].output'),
// even when the value is the empty string. See #172.
type responsesFunctionCallOutputInputWire struct {
	Type   string `json:"type"`
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output"`
}

// responsesContentBlock is one part inside a message item.
// OpenAI uses "input_text" for user/system messages and "output_text" for
// assistant messages — the asymmetry is part of their wire format.
//
// Text deliberately lacks omitempty: the Responses API requires the "text"
// key on input_text / output_text content parts, even when the value is
// the empty string. Both content-block variants today carry the same set
// of fields (type, text), so a single struct expresses the wire shape
// without the cross-variant leakage that motivated splitting
// responsesInput. If a future variant introduces non-shared fields, this
// struct should be split along the same lines.
type responsesContentBlock struct {
	Type string `json:"type"` // "input_text" | "output_text"
	Text string `json:"text"`
}

// responsesTool describes a tool in the Responses API's flatter format.
// (Compare with Chat Completions, which nests under a "function" object.)
//
// Strict is a *bool so the zero-value body omits the field; a quirks
// rule that pins strict mode causes the adapter to emit an explicit
// `"strict": true` on every tool entry. See openaiToolDefinition for
// the equivalent on the Chat Completions side.
type responsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
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
			// User message emission order is deliberate and contract-pinned:
			// function_call_output items are emitted first in document order
			// as they appear in msg.Content, and any text blocks are batched
			// into a single trailing input_text message item. This ordering
			// is documented (rather than fixed to strict document order)
			// because the harness's own message construction never produces
			// mixed user messages — text-then-tool_result-then-text inputs
			// only arise from external callers, and reordering text after
			// tool results matches the Responses API's preference for
			// function_call_output items to precede the next user turn's
			// instructions. See TestTranslateMessagesResponses_UserTextAndToolResultOrder
			// for the pinned behaviour.
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
//
// strict / model / cache behave the same way as translateTools on the
// Chat Completions side: when strict is true, every tool's schema is
// rewritten by NormalizeStrictSchema and the wire entry carries
// strict=true; the cache memoises rewrites within the adapter's
// lifetime. A schema that fails the lint surfaces as an error here,
// and the caller MUST NOT send a request.
func translateToolsResponses(tools []types.ToolDefinition, strict, examples bool, model string, cache *strictSchemaCache) ([]responsesTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]responsesTool, len(tools))
	for i, t := range tools {
		entry := responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
		if strict {
			normalised, err := normalizeStrictWithCache(cache, model, t.Name, t.InputSchema)
			if err != nil {
				return nil, err
			}
			entry.Parameters = normalised
			truthy := true
			entry.Strict = &truthy
		} else if examples {
			// Fold worked examples (#222) into the schema, but only for
			// non-strict tools: the structured-outputs subset rejects the
			// `examples` keyword, identical to the Chat Completions side.
			merged, err := mergeSchemaExamples(entry.Parameters, toolInputExamples(t))
			if err != nil {
				return nil, fmt.Errorf("tool %q: merge examples: %w", t.Name, err)
			}
			entry.Parameters = merged
		}
		out[i] = entry
	}
	return out, nil
}

// buildResponsesRequest projects a StreamParams into the Responses API wire
// body. The Stream field is set by the streaming caller after this returns;
// the builder leaves it false so batch callers get an omitted field (relies
// on omitempty on the responsesRequest.Stream struct tag). Phase-0 refactor
// for issue #133.
//
// q carries the resolved quirks for the (provider, model) pair; the
// OpenAI strict-mode flag (if set by a future openai-responses rule)
// drives strict-mode schema normalisation through cache. Errors from
// the lint surface here so the caller can fail-closed before any HTTP
// request is issued.
//
// TODO(batch): consider returning json.RawMessage if endpoint-contract drift
// becomes a maintenance burden.
func buildResponsesRequest(params types.StreamParams, q quirks.ProviderQuirks, strictCache *strictSchemaCache) (responsesRequest, error) {
	tools, err := translateToolsResponses(params.Tools, q.BehaviourFlags.OpenAI.StrictMode, q.ToolExamples.Supported, params.Model, strictCache)
	if err != nil {
		return responsesRequest{}, err
	}
	return responsesRequest{
		Model:             params.Model,
		Instructions:      params.System,
		Input:             translateMessagesResponses(params.Messages),
		Tools:             tools,
		MaxOutputTokens:   params.MaxTokens,
		Temperature:       params.Temperature,
		Store:             false,
		ParallelToolCalls: openAIParallelFromParams(params, q.ParallelToolCalls),
	}, nil
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

	// Resolve quirks for this (provider, model) pair. No rule
	// targets openai-responses in v1, but the resolution is wired
	// here so a future rule (e.g. a Responses-specific sampling-param
	// omission) lands without re-shaping the Stream method.
	registry := o.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("openai-responses", params.Model)

	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Debug-level log mirrors the chat adapter so an operator gets the
	// same trace surface regardless of which OpenAI endpoint is in
	// use. Empty rules list today (no openai-responses rule); the line
	// fires anyway so a future rule landing here is immediately
	// visible in debug output.
	logger.DebugContext(ctx, "openai-responses quirks resolved",
		slog.String("provider.type", "openai-responses"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	// The Responses request body carries no tool_choice field, so a
	// non-auto ToolChoice requested against this adapter is silently
	// downgraded to auto. Warn once per Stream call so the downgrade is
	// observable (#343). Only the static mode integer and the adapter /
	// model identifiers are logged — never message content or any
	// secret-derived value. q.ToolChoice.Supported is always false today
	// (no openai-responses tool-choice rule), but the flag is checked so a
	// future rule that adds native support suppresses the warning.
	//
	// TODO(#343): add a suppression test asserting this warning does NOT
	// fire when q.ToolChoice.Supported is true, once the first
	// openai-responses native tool-choice quirk rule lands. The
	// !q.ToolChoice.Supported branch is unreachable until then, so no test
	// exercises the suppressed path today.
	if params.ToolChoice != types.ToolChoiceAuto && !q.ToolChoice.Supported {
		logger.WarnContext(ctx, "openai-responses tool-choice downgraded to auto: adapter does not support tool-choice",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
			slog.Int("tool_choice", int(params.ToolChoice)),
		)
	}

	if q.BehaviourFlags.OpenAI.StrictMode {
		// Dormant in v1: no built-in rule currently sets
		// StrictMode=true for any openai-responses model, so this
		// branch is forward-compat scaffolding. It runs in tests that
		// inject a synthetic registry (see
		// TestResponsesStrictMode_WireBodyShape in
		// openai_responses_builder_test.go) and would activate the
		// moment a builtin rule targets openai-responses.
		logger.DebugContext(ctx, "openai-responses strict mode applied",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	reqBody, err := buildResponsesRequest(params, q, o.strictSchemas)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}
	reqBody.Stream = true

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

	requestURL, err := composeOpenAIURL(o.baseURL, "/responses", o.queryParams)
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
	req.Header.Set("Accept", "text/event-stream")
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

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
		// Record latency on a background context: the caller's `ctx` may
		// already have been cancelled by the time the stream completes
		// (the agentic loop has moved on), and some OTel exporters drop
		// measurements on cancelled contexts. Synchronous error paths
		// above use the live `ctx` because the caller is still waiting.
		o.recordLatency(context.Background(), start, metricAttrs)
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
	// emitEvent forwards an event to the consumer, recording TTFB on the
	// first substantive event. Returns false if the consumer has gone
	// away (context cancelled) so the caller can unwind without leaking
	// the goroutine on the open HTTP body.
	emitEvent := func(ev types.StreamEvent) bool {
		// TTFB is meant to capture time-to-first-substantive-output; gate
		// it on event types that represent actual model inference output
		// so error-path zero latencies do not pollute the histogram.
		if !ttfbRecorded && o.Metrics != nil && (ev.Type == "text_delta" || ev.Type == "tool_call") {
			o.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
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
	var dataLen int // aggregate byte length of dataParts; capped to prevent OOM

	flushRecord := func() bool {
		// Reset event-record state on return; defer-style guard so any
		// early return below still leaves a clean slate. We do this
		// via explicit assignment because we need the captured values
		// inside the dispatch.
		eventName := currentEvent
		data := strings.Join(dataParts, "\n")
		currentEvent = ""
		dataParts = dataParts[:0]
		dataLen = 0

		if eventName == "" || data == "" {
			return true
		}
		return o.dispatchEvent(ctx, eventName, data, calls, emitEvent)
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

		// appendData stages a data payload chunk, enforcing the aggregate
		// size cap before allocating. Returns false if the cap was hit
		// (caller should stop reading the stream).
		appendData := func(chunk string) bool {
			if dataLen+len(chunk) > openaiMaxToolInputSize {
				emitEvent(types.StreamEvent{
					Type:  "error",
					Error: fmt.Errorf("SSE record data exceeds %d byte limit", openaiMaxToolInputSize),
				})
				return false
			}
			dataLen += len(chunk)
			dataParts = append(dataParts, chunk)
			return true
		}

		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimPrefix(line, "event:")
		case strings.HasPrefix(line, "data: "):
			if !appendData(strings.TrimPrefix(line, "data: ")) {
				return
			}
		case strings.HasPrefix(line, "data:"):
			if !appendData(strings.TrimPrefix(line, "data:")) {
				return
			}
		}
	}

	// Flush any trailing record without a terminating blank line. A
	// well-behaved server will not do this, but tolerating it avoids
	// dropped final events on premature EOF.
	if currentEvent != "" || len(dataParts) > 0 {
		if !flushRecord() {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}

// dispatchEvent handles a single completed SSE record. It returns false to
// signal the caller to stop reading (terminal event or consumer cancelled),
// true to continue.
//
// `emit` returns false when the consumer has gone away (context cancelled);
// every emit call site propagates that to abandon the stream rather than
// pretending to keep going.
func (o *OpenAIResponsesAdapter) dispatchEvent(ctx context.Context, name, data string, calls map[string]*responsesCallState, emit func(types.StreamEvent) bool) bool {
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
			if !emit(types.StreamEvent{Type: "text_delta", Text: payload.Delta}) {
				return false
			}
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
		// Terminal event: signal caller to stop reading regardless of
		// whether the consumer accepted the message_complete event.
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
		emit(types.StreamEvent{Type: "error", Error: errors.New(msg)})
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
		emit(types.StreamEvent{Type: "error", Error: errors.New(msg)})
		return false

	default:
		// Forward-compatible: unknown events (e.g. reasoning summaries,
		// content_part lifecycle, partial-image deltas) are ignored. We
		// add a span event so production operators can spot a flood of
		// new event types from a future API revision instead of silently
		// dropping content.
		if span := oteltrace.SpanFromContext(ctx); span != nil && span.IsRecording() {
			span.AddEvent("openai_responses.unknown_sse_event",
				oteltrace.WithAttributes(attribute.String("event.type", name)),
			)
		}
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
// its accumulated arguments JSON. Returns false on a fatal parse error or
// when the consumer has gone away (caller should stop reading the stream).
// Marks the call as emitted so a duplicate .done event does not fire it
// twice.
func flushOneCall(st *responsesCallState, emit func(types.StreamEvent) bool) bool {
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
	return emit(types.StreamEvent{
		Type:  "tool_call",
		ID:    st.callID,
		Name:  st.name,
		Input: input,
	})
}

// flushPendingCalls emits any tool calls that were left in flight when the
// terminal response.completed / response.incomplete event arrived. Order is
// stable by output_index so multi-call responses are deterministic.
func flushPendingCalls(calls map[string]*responsesCallState, emit func(types.StreamEvent) bool) bool {
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

// deriveStopReason adapts the streaming Responses response shape to
// the shared deriveResponsesStopReason helper. Computes the
// hasTool / incomplete-reason inputs from resp and dispatches; the
// branch logic lives in batch.go so a new status arm is applied to
// the batch fabrication path simultaneously.
func deriveStopReason(resp responsesResponse) string {
	hasTool := false
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			hasTool = true
			break
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	return deriveResponsesStopReason(resp.Status, incompleteReason, hasTool)
}
