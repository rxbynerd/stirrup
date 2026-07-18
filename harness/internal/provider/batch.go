package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// Event-type discriminators for the batch wire protocol. HarnessEvents
// flow harness → control plane; ControlEvent flows control plane →
// harness. See types/events.go for the wire contract.
const (
	eventBatchSubmission    = "batch_submission"
	eventBatchWaiting       = "batch_waiting"
	eventBatchCancelRequest = "batch_cancel_request"
	eventBatchResult        = "batch_result"
)

// batchWaitingHeartbeatIntervalNs holds the cadence (in nanoseconds) at
// which controlPlaneBatchClient emits batch_waiting HarnessEvents while
// a submission is in flight. Atomic so tests can lower it without
// racing heartbeat goroutines from earlier tests.
var batchWaitingHeartbeatIntervalNs atomic.Int64

func init() {
	batchWaitingHeartbeatIntervalNs.Store(int64(5 * time.Minute))
}

func getBatchWaitingHeartbeatInterval() time.Duration {
	return time.Duration(batchWaitingHeartbeatIntervalNs.Load())
}

func setBatchWaitingHeartbeatInterval(d time.Duration) time.Duration {
	prev := batchWaitingHeartbeatIntervalNs.Swap(int64(d))
	return time.Duration(prev)
}

// BatchClient submits batch entries to a provider and retrieves results.
// The multi-entry shape is required to support OpenAI's file-upload flow;
// the gRPC control-plane client always submits size-1 slices.
type BatchClient interface {
	// Submit dispatches the given entries to the provider's batch
	// endpoint (directly, or via the control plane) and returns an opaque
	// batchID that can be passed to Result. The batchID is the harness's
	// correlation handle; the provider-side batch identifier may differ
	// and is carried inside the eventual BatchResult.
	//
	// Callers must pass exactly one entry; implementations reject
	// len(entries) != 1. The slice shape is reserved for a future
	// multi-entry file-upload path.
	Submit(ctx context.Context, entries []BatchEntry) (batchID string, err error)

	// Result blocks until the batch identified by batchID resolves (or
	// the context / the client's own wall-clock cap fires). The returned
	// map is keyed by BatchEntry.CustomID.
	Result(ctx context.Context, batchID string) (map[string]*BatchResult, error)
}

// BatchEntry is a single submission within a batch.
type BatchEntry struct {
	// CustomID is the per-entry correlation handle echoed back in the
	// batch_result, shaped "stirrup-<runID>-turn-<n>" (see BatchAdapter.Stream).
	CustomID string `json:"custom_id"`

	// Provider names the upstream API shape the Body conforms to.
	// "anthropic" | "openai-compatible" | "openai-responses".
	Provider string `json:"provider"`

	// Body is the marshalled request the streaming adapter would have
	// sent — see build*Request helpers in the sibling adapter files.
	Body json.RawMessage `json:"body"`
}

// BatchResult is the per-entry outcome of a batch submission.
type BatchResult struct {
	// Response is the provider's Messages-API-compatible response JSON on
	// success. Nil when Err is non-nil.
	Response json.RawMessage `json:"response,omitempty"`

	// Err is populated when the provider reported a non-success result
	// for this entry. The streaming wrapper maps this to a single error
	// StreamEvent; a future phase will surface the type as a TurnTrace
	// outcome.
	Err *BatchResultError `json:"err,omitempty"`
}

// BatchResultError categorises a non-success batch outcome.
type BatchResultError struct {
	// Type discriminates the failure category. Recognised values mirror
	// the provider's own taxonomy:
	//   - "invalid_request_error"
	//   - "server_error"
	//   - "batch_expired"
	//   - "batch_cancelled"
	Type string `json:"type"`

	// Message is a human-readable description of the failure.
	Message string `json:"message,omitempty"`
}

// BatchSubmission is the JSON payload carried in the batch_submission
// HarnessEvent's Input field. The control plane uses it to construct the
// provider-side batch entry.
type BatchSubmission struct {
	// SchemaVersion identifies the payload shape so the control plane
	// can route legacy submissions without ambiguity; increment when
	// adding fields consumers must know about.
	SchemaVersion int `json:"schema_version"`

	// ProviderType is the provider shape the Body conforms to.
	// "anthropic" | "openai-compatible" | "openai-responses".
	ProviderType string `json:"provider_type"`

	// CustomID is the entry's correlation handle ("stirrup-<runID>-turn-<n>").
	// The control plane includes it on the provider-side batch entry so
	// the returned batch_result.content can carry it back unchanged.
	CustomID string `json:"custom_id"`

	// Body is the marshalled provider request — what the streaming
	// adapter would have POSTed to /v1/messages or /v1/chat/completions
	// or /v1/responses.
	Body json.RawMessage `json:"body"`
}

// batchSubmissionSchemaVersion is the current BatchSubmission payload
// shape version. See the field doc on BatchSubmission.SchemaVersion.
const batchSubmissionSchemaVersion = 1

// BatchAdapter wraps a streaming ProviderAdapter and fakes the streaming
// channel from a completed batch response. The streaming inner is retained
// only as a fallback for cfg.FallbackOnTimeout.
type BatchAdapter struct {
	inner     ProviderAdapter
	client    BatchClient
	cfg       *types.BatchProviderConfig
	provType  string
	runID     string
	turnCount atomic.Int64
	// Registry resolves per-(provider, model) quirks for the request
	// body marshalling path, so a batch run against a compat-profile
	// provider sees the same wire shape the streaming path would have
	// produced. Nil falls back to quirks.DefaultRegistry(). Exported
	// (not a constructor arg) to avoid churn on every NewBatchAdapter
	// call site in tests.
	Registry *quirks.Registry
	// lastBatchID stores the provider-assigned batch identifier from the
	// most recent successful Submit; the agentic loop reads it via
	// LastBatchID() to populate TurnTrace.BatchID. atomic.Value as
	// defence in depth against concurrent turns.
	lastBatchID atomic.Value // string
}

// NewBatchAdapter constructs a BatchAdapter. cfg.MaxWaitSeconds is read
// on each Stream call; the client is expected to enforce the same cap
// internally (defence in depth).
func NewBatchAdapter(
	inner ProviderAdapter,
	client BatchClient,
	cfg *types.BatchProviderConfig,
	provType string,
	runID string,
) *BatchAdapter {
	return &BatchAdapter{
		inner:    inner,
		client:   client,
		cfg:      cfg,
		provType: provType,
		runID:    runID,
	}
}

// Stream submits the turn as a single-entry batch, awaits the result, and
// emits a fabricated StreamEvent sequence equivalent to what the inner
// streaming adapter would have produced for the same response.
func (a *BatchAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	turn := a.turnCount.Add(1)
	customID := fmt.Sprintf("stirrup-%s-turn-%d", a.runID, turn)

	body, err := a.marshalRequestBody(params)
	if err != nil {
		ch := make(chan types.StreamEvent, 1)
		ch <- types.StreamEvent{Type: "error", Error: err}
		close(ch)
		return ch, nil
	}

	batchID, err := a.client.Submit(ctx, []BatchEntry{{
		CustomID: customID,
		Provider: a.provType,
		Body:     body,
	}})
	if err != nil {
		ch := make(chan types.StreamEvent, 1)
		ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch submit: %w", err)}
		close(ch)
		return ch, nil
	}
	// Stored on Submit success (not after Result) so the loop sees the
	// ID even on a downstream fabrication or fallback path.
	a.lastBatchID.Store(batchID)

	ch := make(chan types.StreamEvent, 64)
	go a.awaitAndFabricate(ctx, ch, params, customID, batchID)
	return ch, nil
}

// LastBatchID returns the provider-assigned identifier of the most
// recent successfully-submitted batch. Empty before the first Submit
// and remains the previous value if a later Submit fails.
func (a *BatchAdapter) LastBatchID() string {
	v := a.lastBatchID.Load()
	if v == nil {
		return ""
	}
	id, _ := v.(string)
	return id
}

// marshalRequestBody projects params into the wire body for the
// configured provider type. Unsupported provider types surface as a
// marshal-time error so the caller can emit a single error StreamEvent.
func (a *BatchAdapter) marshalRequestBody(params types.StreamParams) (json.RawMessage, error) {
	switch a.provType {
	case "anthropic":
		registry := a.Registry
		if registry == nil {
			registry = quirks.DefaultRegistry()
		}
		q := registry.Resolve("anthropic", params.Model)
		return json.Marshal(buildAnthropicRequest(params, false, q))
	case "openai-compatible":
		registry := a.Registry
		if registry == nil {
			registry = quirks.DefaultRegistry()
		}
		q := registry.Resolve("openai-compatible", params.Model)
		// Pass nil cache: a batch submission is rare enough that the
		// per-tool normaliser walk cost is negligible.
		req, err := buildOpenAIRequest(params, false, q, nil)
		if err != nil {
			return nil, err
		}
		return json.Marshal(req)
	case "openai-responses":
		registry := a.Registry
		if registry == nil {
			registry = quirks.DefaultRegistry()
		}
		q := registry.Resolve("openai-responses", params.Model)
		req, err := buildResponsesRequest(params, q, nil)
		if err != nil {
			return nil, err
		}
		return json.Marshal(req)
	default:
		return nil, fmt.Errorf("batch: unsupported provider type %q", a.provType)
	}
}

// awaitAndFabricate runs in a goroutine, blocks on the batch result, and
// drains either the fabricated stream, a fallback inner stream, or a
// single error event onto ch before closing it.
func (a *BatchAdapter) awaitAndFabricate(
	ctx context.Context,
	ch chan<- types.StreamEvent,
	params types.StreamParams,
	customID string,
	batchID string,
) {
	defer close(ch)

	results, err := a.client.Result(ctx, batchID)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch wait cancelled: %w", err)}
		case isBatchTimeout(err):
			if a.cfg != nil && a.cfg.FallbackOnTimeout && a.inner != nil {
				a.pumpInner(ctx, ch, params)
				return
			}
			ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch wait timed out: %w", err)}
		default:
			ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch result: %w", err)}
		}
		return
	}

	entry, ok := results[customID]
	if !ok || entry == nil {
		ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch result missing entry for custom_id %q", customID)}
		return
	}

	if entry.Err != nil {
		// Prefix the error type to distinguish batch-side failure
		// categories from inner provider errors. Scrub at this single
		// fan-in point so a provider returning a credential-shaped
		// string in its error body cannot propagate verbatim.
		ch <- types.StreamEvent{
			Type:  "error",
			Error: fmt.Errorf("[%s] %s", entry.Err.Type, security.Scrub(entry.Err.Message)),
		}
		return
	}

	fabricateStream(ch, entry.Response, a.provType)
}

// pumpInner relays events from the streaming fallback into the
// BatchAdapter's outbound channel. Used only when FallbackOnTimeout is
// true and the batch wait fired its wall-clock cap.
func (a *BatchAdapter) pumpInner(ctx context.Context, ch chan<- types.StreamEvent, params types.StreamParams) {
	innerCh, err := a.inner.Stream(ctx, params)
	if err != nil {
		ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("batch fallback to streaming failed: %w", err)}
		return
	}
	for ev := range innerCh {
		select {
		case <-ctx.Done():
			return
		case ch <- ev:
		}
	}
}

// errBatchExpired is the sentinel returned by BatchClient.Result when the
// harness-side wall-clock cap fires before a batch_result arrives.
// BatchAdapter checks for it via isBatchTimeout to decide between an
// error event and the FallbackOnTimeout streaming path. The wire-level
// vocabulary (BatchResultError.Type == "batch_expired") is independent
// of this Go-side sentinel.
var errBatchExpired = errors.New("batch expired")

// isBatchTimeout reports whether err wraps errBatchExpired — i.e. the
// BatchClient's wall-clock cap fired before the batch resolved. Both
// control-plane and polling clients wrap the same sentinel so the
// timeout-fallback branch is provider-independent.
func isBatchTimeout(err error) bool {
	return errors.Is(err, errBatchExpired)
}

// fabricateStream decodes a batch response and emits the StreamEvent
// sequence the streaming adapter would have produced for the same body.
// Unsupported provider types emit a single error event rather than a
// separate error return, mirroring ProviderAdapter.Stream's convention
// of surfacing all failures as in-channel error events.
func fabricateStream(ch chan<- types.StreamEvent, response json.RawMessage, provType string) {
	switch provType {
	case "anthropic":
		if err := fabricateAnthropicStream(ch, response); err != nil {
			ch <- types.StreamEvent{Type: "error", Error: err}
		}
	case "openai-compatible":
		if err := fabricateOpenAIChatStream(ch, response); err != nil {
			ch <- types.StreamEvent{Type: "error", Error: err}
		}
	case "openai-responses":
		if err := fabricateOpenAIResponsesStream(ch, response); err != nil {
			ch <- types.StreamEvent{Type: "error", Error: err}
		}
	default:
		ch <- types.StreamEvent{
			Type:  "error",
			Error: fmt.Errorf("fabricateStream: unsupported provider type %q", provType),
		}
	}
}

// anthropicBatchResponse mirrors the Anthropic Messages API response
// shape the batch endpoint returns for a successful entry. Only the
// fields the fabrication path consumes are decoded.
type anthropicBatchResponse struct {
	Content    []anthropicBatchContentBlock `json:"content"`
	StopReason string                       `json:"stop_reason"`
	Usage      struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicBatchContentBlock struct {
	Type  string          `json:"type"` // "text" | "tool_use"
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// fabricateAnthropicStream mirrors the SSE event sequence consumeSSE
// produces in anthropic.go: one text_delta per text content block (not
// per token), one tool_call per tool_use block, then a single
// message_complete carrying the assembled content blocks plus
// stop_reason / output_tokens — observationally indistinguishable from
// the live SSE path for the agentic loop.
func fabricateAnthropicStream(ch chan<- types.StreamEvent, response json.RawMessage) error {
	var resp anthropicBatchResponse
	if err := json.Unmarshal(response, &resp); err != nil {
		return fmt.Errorf("fabricate anthropic stream: decode response: %w", err)
	}

	blocks := make([]types.ContentBlock, 0, len(resp.Content))
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			ch <- types.StreamEvent{Type: "text_delta", Text: block.Text}
			blocks = append(blocks, types.ContentBlock{
				Type: "text",
				Text: block.Text,
			})
		case "tool_use":
			var input map[string]any
			if len(block.Input) > 0 {
				if err := json.Unmarshal(block.Input, &input); err != nil {
					return fmt.Errorf("fabricate anthropic stream: decode tool input: %w", err)
				}
			}
			ch <- types.StreamEvent{
				Type:  "tool_call",
				ID:    block.ID,
				Name:  block.Name,
				Input: input,
			}
			blocks = append(blocks, types.ContentBlock{
				Type:  "tool_use",
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}

	ch <- types.StreamEvent{
		Type:         "message_complete",
		StopReason:   resp.StopReason,
		OutputTokens: resp.Usage.OutputTokens,
		Content:      blocks,
	}
	return nil
}

// controlPlaneBatchClient implements BatchClient by emitting a
// batch_submission HarnessEvent and awaiting a matching batch_result
// ControlEvent on the transport. The control plane owns the provider
// batch lifecycle (polling, webhooks); the harness only sees the
// request/result boundary.
//
// The Submit/Result split (rather than emit-then-await in one call) is
// dictated by the BatchClient contract: the BatchAdapter needs a
// non-blocking Submit so it can run the heartbeat alongside Result.
// This reproduces the correlator's pending-map pattern locally rather
// than reusing transport.Correlator, which only exposes emit-and-await.
type controlPlaneBatchClient struct {
	transport          transport.Transport
	maxWait            time.Duration
	cancelBundleOnExit bool

	mu     sync.Mutex
	nextID int
	// pending and customID are keyed by requestID. The client always
	// submits size-1 batches (see Submit); the map[customID]→*BatchResult
	// shape returned by Result is for forward compatibility with future
	// multi-entry batches.
	pending  map[string]chan *BatchResult
	customID map[string]string // requestID -> originating entry CustomID
}

// NewControlPlaneBatchClient constructs a batch client that delegates the
// provider-side batch lifecycle to the control plane via the gRPC
// transport. maxWait is the wall-clock cap on Result (BatchAdapter also
// applies cfg.MaxWaitSeconds as defence in depth). cancelBundleOnExit,
// when true, emits a batch_cancel_request HarnessEvent on ctx-cancel or
// wall-clock-cap exit from Result (Provider.Batch.CancelBundleOnRunCancel).
func NewControlPlaneBatchClient(t transport.Transport, maxWait time.Duration, cancelBundleOnExit bool) *controlPlaneBatchClient {
	c := &controlPlaneBatchClient{
		transport:          t,
		maxWait:            maxWait,
		cancelBundleOnExit: cancelBundleOnExit,
		pending:            make(map[string]chan *BatchResult),
		customID:           make(map[string]string),
	}
	t.OnControl(c.handleControl)
	return c
}

// handleControl routes batch_result ControlEvents to the pending Result
// caller. Result owns deletion from both c.pending and c.customID on
// every exit path; handleControl never deletes. The non-blocking send
// covers the case where Result has already abandoned the entry.
func (c *controlPlaneBatchClient) handleControl(event types.ControlEvent) {
	if event.Type != eventBatchResult || event.RequestID == "" {
		return
	}
	result := decodeBatchResult(event)

	c.mu.Lock()
	ch, ok := c.pending[event.RequestID]
	c.mu.Unlock()

	if !ok {
		return
	}
	select {
	case ch <- result:
	default:
		// Result already abandoned this entry.
	}
}

// maxBatchResponseBytes caps the size of a batch_result Content payload
// the harness will decode, defending against a misbehaving control
// plane allocating unbounded harness-side memory (CWE-400).
const maxBatchResponseBytes = 4 * 1024 * 1024

// decodeBatchResult turns a batch_result ControlEvent's content into a
// *BatchResult. An empty content, oversize payload, or malformed JSON
// surfaces as a BatchResult.Err rather than a nil entry.
func decodeBatchResult(event types.ControlEvent) *BatchResult {
	if event.Content == "" {
		return &BatchResult{
			Err: &BatchResultError{Type: "invalid_request_error", Message: "batch_result missing content"},
		}
	}
	if len(event.Content) > maxBatchResponseBytes {
		return &BatchResult{
			Err: &BatchResultError{
				Type:    "invalid_request_error",
				Message: fmt.Sprintf("batch_result content exceeds %d-byte limit", maxBatchResponseBytes),
			},
		}
	}
	var result BatchResult
	if err := json.Unmarshal([]byte(event.Content), &result); err != nil {
		return &BatchResult{
			Err: &BatchResultError{Type: "invalid_request_error", Message: fmt.Sprintf("decode batch_result: %v", err)},
		}
	}
	return &result
}

// Submit emits a single-entry batch_submission HarnessEvent and returns
// the correlator-assigned request ID. Result blocks on the matching
// batch_result ControlEvent later, on the same client.
func (c *controlPlaneBatchClient) Submit(ctx context.Context, entries []BatchEntry) (string, error) {
	if len(entries) != 1 {

		return "", fmt.Errorf("controlPlaneBatchClient: expected exactly 1 entry, got %d", len(entries))
	}
	entry := entries[0]

	payload := BatchSubmission{
		SchemaVersion: batchSubmissionSchemaVersion,
		ProviderType:  entry.Provider,
		CustomID:      entry.CustomID,
		Body:          entry.Body,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal batch submission: %w", err)
	}

	requestID := c.nextRequestID()
	ch := make(chan *BatchResult, 1)

	c.mu.Lock()
	c.pending[requestID] = ch
	c.customID[requestID] = entry.CustomID
	c.mu.Unlock()

	if err := c.transport.Emit(types.HarnessEvent{
		Type:      eventBatchSubmission,
		RequestID: requestID,
		Input:     payloadBytes,
	}); err != nil {
		c.releasePending(requestID)
		return "", fmt.Errorf("emit batch_submission: %w", err)
	}

	go c.heartbeat(ctx, requestID)

	return requestID, nil
}

// Result blocks until the batch_result for batchID arrives, the maxWait
// fires (returning a batch_expired error), or the context is cancelled.
func (c *controlPlaneBatchClient) Result(ctx context.Context, batchID string) (map[string]*BatchResult, error) {
	c.mu.Lock()
	ch, ok := c.pending[batchID]
	customID := c.customID[batchID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("controlPlaneBatchClient: no pending submission for batchID %q", batchID)
	}

	timeout := c.maxWait
	if timeout <= 0 {
		// Must not fall back to transport.DefaultCorrelatorTimeout: that
		// default is far shorter than DefaultBatchMaxWaitSeconds and
		// would silently expire long batches early.
		timeout = time.Duration(types.DefaultBatchMaxWaitSeconds) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-ch:

		c.releasePending(batchID)
		return map[string]*BatchResult{customID: result}, nil
	case <-timer.C:
		c.releasePending(batchID)
		c.maybeEmitCancelRequest(batchID)
		return nil, fmt.Errorf("%w: timed out after %s (batchID=%s)", errBatchExpired, timeout, batchID)
	case <-ctx.Done():
		c.releasePending(batchID)
		c.maybeEmitCancelRequest(batchID)
		return nil, fmt.Errorf("controlPlaneBatchClient: cancelled: %w", ctx.Err())
	}
}

// maybeEmitCancelRequest emits a batch_cancel_request HarnessEvent for
// the given submission when cancelBundleOnExit=true. Emit errors are
// ignored so they don't obscure the primary timeout/cancel error
// returned by Result.
func (c *controlPlaneBatchClient) maybeEmitCancelRequest(requestID string) {
	if !c.cancelBundleOnExit {
		return
	}
	_ = c.transport.Emit(types.HarnessEvent{
		Type:      eventBatchCancelRequest,
		RequestID: requestID,
	})
}

// releasePending removes a pending entry; safe to call when it has
// already been removed by a concurrent resolution.
func (c *controlPlaneBatchClient) releasePending(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	delete(c.customID, requestID)
	c.mu.Unlock()
}

// nextRequestID issues a monotonically increasing "batch-<n>" request ID.
func (c *controlPlaneBatchClient) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return fmt.Sprintf("batch-%d", c.nextID)
}

// heartbeat emits a batch_waiting HarnessEvent at the configured cadence
// until ctx fires or the pending entry is removed. Emit errors are
// ignored: a broken transport surfaces via Result's underlying RPC.
func (c *controlPlaneBatchClient) heartbeat(ctx context.Context, requestID string) {
	ticker := time.NewTicker(getBatchWaitingHeartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			_, stillPending := c.pending[requestID]
			c.mu.Unlock()
			if !stillPending {
				return
			}
			_ = c.transport.Emit(types.HarnessEvent{
				Type:      eventBatchWaiting,
				RequestID: requestID,
			})
		}
	}
}

// openaiChatBatchResponse mirrors the OpenAI Chat Completions response
// body returned inside an /v1/files batch output line's "response.body"
// field. Only the fields the fabrication path consumes are decoded.
type openaiChatBatchResponse struct {
	Choices []openaiChatBatchChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens,omitempty"`
		CompletionTokens int `json:"completion_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

type openaiChatBatchChoice struct {
	Index        int                    `json:"index"`
	Message      openaiChatBatchMessage `json:"message"`
	FinishReason string                 `json:"finish_reason"`
}

type openaiChatBatchMessage struct {
	Role      string                    `json:"role,omitempty"`
	Content   *string                   `json:"content,omitempty"`
	ToolCalls []openaiChatBatchToolCall `json:"tool_calls,omitempty"`
}

type openaiChatBatchToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// fabricateOpenAIChatStream mirrors the SSE event sequence consumeSSE
// produces in openai.go: one text_delta for any non-empty assistant
// content, one tool_call per tool_calls entry (in upstream order), then
// a single message_complete carrying the mapped finish_reason and the
// usage.completion_tokens count. Content is left nil on message_complete
// to match consumeSSE, which does not populate it either.
func fabricateOpenAIChatStream(ch chan<- types.StreamEvent, response json.RawMessage) error {
	var resp openaiChatBatchResponse
	if err := json.Unmarshal(response, &resp); err != nil {
		return fmt.Errorf("fabricate openai chat stream: decode response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("fabricate openai chat stream: response has no choices")
	}
	choice := resp.Choices[0]

	if choice.Message.Content != nil && *choice.Message.Content != "" {
		ch <- types.StreamEvent{Type: "text_delta", Text: *choice.Message.Content}
	}
	for _, tc := range choice.Message.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				return fmt.Errorf("fabricate openai chat stream: decode tool arguments: %w", err)
			}
		}
		ch <- types.StreamEvent{
			Type:  "tool_call",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		}
	}

	ev := types.StreamEvent{
		Type:       "message_complete",
		StopReason: mapFinishReason(choice.FinishReason),
	}
	if resp.Usage != nil {
		ev.OutputTokens = resp.Usage.CompletionTokens
	}
	ch <- ev
	return nil
}

// openaiResponsesBatchResponse mirrors the Responses-API response body
// returned inside an /v1/files batch output line for the Responses
// endpoint. The shape matches the response.completed envelope the
// streaming SSE adapter consumes via response.output[] items + usage.
type openaiResponsesBatchResponse struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Output []openaiResponsesBatchOutputItem `json:"output,omitempty"`
	Usage  *struct {
		InputTokens  int `json:"input_tokens,omitempty"`
		OutputTokens int `json:"output_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

// openaiResponsesBatchOutputItem is one item in response.output. Type
// discriminates: "message" carries assistant text inside content[];
// "function_call" carries a single call's id/name/arguments flat.
type openaiResponsesBatchOutputItem struct {
	Type      string                             `json:"type"`
	ID        string                             `json:"id,omitempty"`
	CallID    string                             `json:"call_id,omitempty"`
	Name      string                             `json:"name,omitempty"`
	Arguments string                             `json:"arguments,omitempty"`
	Content   []openaiResponsesBatchContentBlock `json:"content,omitempty"`
}

type openaiResponsesBatchContentBlock struct {
	Type string `json:"type"` // "output_text" | "refusal" | ...
	Text string `json:"text,omitempty"`
}

// fabricateOpenAIResponsesStream mirrors the SSE event sequence the
// Responses adapter's consumeSSE produces on a completed response: one
// text_delta per assistant output_text content block, one tool_call per
// function_call output item (in upstream order — the JSON array
// preserves document order), then a single message_complete carrying
// the derived stop reason and usage.output_tokens.
func fabricateOpenAIResponsesStream(ch chan<- types.StreamEvent, response json.RawMessage) error {
	var resp openaiResponsesBatchResponse
	if err := json.Unmarshal(response, &resp); err != nil {
		return fmt.Errorf("fabricate openai responses stream: decode response: %w", err)
	}

	hasTool := false
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, block := range item.Content {
				if block.Type == "output_text" && block.Text != "" {
					ch <- types.StreamEvent{Type: "text_delta", Text: block.Text}
				}
			}
		case "function_call":
			hasTool = true
			var input map[string]any
			if item.Arguments != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
					return fmt.Errorf("fabricate openai responses stream: decode tool arguments: %w", err)
				}
			}
			ch <- types.StreamEvent{
				Type:  "tool_call",
				ID:    item.CallID,
				Name:  item.Name,
				Input: input,
			}
		}
	}

	stop := deriveOpenAIResponsesStopReason(resp, hasTool)
	ev := types.StreamEvent{
		Type:       "message_complete",
		StopReason: stop,
	}
	if resp.Usage != nil {
		ev.OutputTokens = resp.Usage.OutputTokens
	}
	ch <- ev
	return nil
}

// deriveOpenAIResponsesStopReason adapts the batch response shape to
// the shared deriveResponsesStopReason helper.
func deriveOpenAIResponsesStopReason(resp openaiResponsesBatchResponse, hasTool bool) string {
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	return deriveResponsesStopReason(resp.Status, incompleteReason, hasTool)
}

// deriveResponsesStopReason maps an OpenAI Responses API status /
// incomplete_details.reason / tool-presence tuple to stirrup's stop
// reason vocabulary, shared between the streaming and batch fabrication
// paths so a new status arm only has to be applied once. Tool calls
// take precedence over plain end_turn so the agentic loop dispatches
// tools before treating the turn as final.
func deriveResponsesStopReason(status, incompleteReason string, hasTool bool) string {
	switch status {
	case "completed":
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	case "incomplete":
		if incompleteReason == "max_output_tokens" || incompleteReason == "max_tokens" {
			return "max_tokens"
		}
		if incompleteReason != "" {
			return incompleteReason
		}
		return "incomplete"
	default:
		if status != "" {
			return status
		}
		if hasTool {
			return "tool_use"
		}
		return "end_turn"
	}
}
