package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// batchWaitingHeartbeatIntervalNs holds the cadence (in nanoseconds) at
// which the controlPlaneBatchClient emits batch_waiting HarnessEvents
// while a batch submission is in flight. Stored as an atomic so tests
// can lower it without racing the heartbeat goroutines that earlier
// tests may still be running. Use getBatchWaitingHeartbeatInterval /
// setBatchWaitingHeartbeatInterval rather than touching this directly.
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
	Submit(ctx context.Context, entries []BatchEntry) (batchID string, err error)

	// Result blocks until the batch identified by batchID resolves (or
	// the context / the client's own wall-clock cap fires). The returned
	// map is keyed by BatchEntry.CustomID.
	Result(ctx context.Context, batchID string) (map[string]*BatchResult, error)
}

// BatchEntry is a single submission within a batch.
type BatchEntry struct {
	// CustomID is the per-entry correlation handle echoed back in the
	// batch_result. The BatchAdapter uses the shape "<runID>-turn-<n>".
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
	// ProviderType is the provider shape the Body conforms to.
	// "anthropic" | "openai-compatible" | "openai-responses".
	ProviderType string `json:"provider_type"`

	// CustomID is the entry's correlation handle ("<runID>-turn-<n>").
	// The control plane includes it on the provider-side batch entry so
	// the returned batch_result.content can carry it back unchanged.
	CustomID string `json:"custom_id"`

	// Body is the marshalled provider request — what the streaming
	// adapter would have POSTed to /v1/messages or /v1/chat/completions
	// or /v1/responses.
	Body json.RawMessage `json:"body"`
}

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
	customID := fmt.Sprintf("%s-turn-%d", a.runID, turn)

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

	ch := make(chan types.StreamEvent, 64)
	go a.awaitAndFabricate(ctx, ch, params, customID, batchID)
	return ch, nil
}

// marshalRequestBody projects params into the wire body for the
// configured provider type. Unsupported provider types surface as a
// marshal-time error so the caller can emit a single error StreamEvent.
func (a *BatchAdapter) marshalRequestBody(params types.StreamParams) (json.RawMessage, error) {
	switch a.provType {
	case "anthropic":
		return json.Marshal(buildAnthropicRequest(params, false))
	case "openai-compatible":
		return json.Marshal(buildOpenAIRequest(params, false))
	case "openai-responses":
		return json.Marshal(buildResponsesRequest(params))
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
		// Prefix the error type so reviewers (and the eventual outcome
		// mapper in #138) can distinguish batch-side failure categories
		// from inner provider errors without parsing the wrapped chain.
		ch <- types.StreamEvent{
			Type:  "error",
			Error: fmt.Errorf("[%s] %s", entry.Err.Type, entry.Err.Message),
		}
		return
	}

	if err := fabricateStream(ch, entry.Response, a.provType); err != nil {
		ch <- types.StreamEvent{Type: "error", Error: err}
	}
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

// isBatchTimeout reports whether err is a wall-clock batch_expired
// timeout from the BatchClient. The client surfaces this by returning an
// error whose message contains "batch_expired".
func isBatchTimeout(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "batch_expired")
}

// fabricateStream decodes a batch response and emits the StreamEvent
// sequence the streaming adapter would have produced for the same body.
// Only Anthropic is implemented in phase 2; OpenAI variants emit a single
// error event so the OpenAI batch path lands cleanly in phase 6 (#139).
func fabricateStream(ch chan<- types.StreamEvent, response json.RawMessage, provType string) error {
	switch provType {
	case "anthropic":
		return fabricateAnthropicStream(ch, response)
	case "openai-compatible", "openai-responses":
		ch <- types.StreamEvent{
			Type:  "error",
			Error: errors.New("OpenAI batch fabrication not yet implemented"),
		}
		return nil
	default:
		return fmt.Errorf("fabricateStream: unsupported provider type %q", provType)
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
// produces in anthropic.go: one text_delta per text content block, one
// tool_call per tool_use block, then a single message_complete carrying
// the assembled content blocks plus stop_reason / output_tokens.
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
// The Submit/Result split (vs. emit-then-await in one call, as the
// AskUpstreamPolicy does) is dictated by the BatchClient contract: the
// BatchAdapter must hand the streaming goroutine a non-blocking Submit
// so it can run the heartbeat alongside Result. We therefore reproduce
// the correlator's pending-map pattern locally rather than reusing
// transport.Correlator (which exposes only the emit-and-await shape).
type controlPlaneBatchClient struct {
	transport transport.Transport
	maxWait   time.Duration

	mu       sync.Mutex
	nextID   int
	pending  map[string]chan *BatchResult
	customID map[string]string // requestID -> originating entry CustomID
}

// NewControlPlaneBatchClient constructs a batch client that delegates the
// provider-side batch lifecycle to the control plane via the gRPC
// transport. maxWait is the wall-clock cap on Result; the BatchAdapter
// also applies this via cfg.MaxWaitSeconds (defence in depth).
func NewControlPlaneBatchClient(t transport.Transport, maxWait time.Duration) *controlPlaneBatchClient {
	c := &controlPlaneBatchClient{
		transport: t,
		maxWait:   maxWait,
		pending:   make(map[string]chan *BatchResult),
		customID:  make(map[string]string),
	}
	t.OnControl(c.handleControl)
	return c
}

// handleControl routes batch_result ControlEvents to the pending Result
// caller. Mirrors transport.Correlator.deliver, but specialised for the
// BatchResult payload so we can keep the channel typed.
func (c *controlPlaneBatchClient) handleControl(event types.ControlEvent) {
	if event.Type != "batch_result" || event.RequestID == "" {
		return
	}
	result := decodeBatchResult(event)

	c.mu.Lock()
	ch, ok := c.pending[event.RequestID]
	if ok {
		delete(c.pending, event.RequestID)
	}
	c.mu.Unlock()

	if !ok {
		return
	}
	// Channel has capacity 1 and we just removed it from the pending map
	// under the lock, so the send cannot block.
	ch <- result
}

// decodeBatchResult turns a batch_result ControlEvent's content into a
// *BatchResult. An empty content or malformed JSON surfaces as a
// BatchResult.Err so the BatchAdapter sees a non-nil entry even when the
// control plane mis-frames the event.
func decodeBatchResult(event types.ControlEvent) *BatchResult {
	if event.Content == "" {
		return &BatchResult{
			Err: &BatchResultError{Type: "invalid_request_error", Message: "batch_result missing content"},
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
		// The control-plane wire contract is one batch_submission per
		// harness turn. Multi-entry submission is reserved for the
		// future stdio polling client (phase 4).
		return "", fmt.Errorf("controlPlaneBatchClient: expected exactly 1 entry, got %d", len(entries))
	}
	entry := entries[0]

	payload := BatchSubmission{
		ProviderType: entry.Provider,
		CustomID:     entry.CustomID,
		Body:         entry.Body,
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
		Type:      "batch_submission",
		RequestID: requestID,
		Input:     payloadBytes,
	}); err != nil {
		c.releasePending(requestID)
		return "", fmt.Errorf("emit batch_submission: %w", err)
	}

	// The heartbeat goroutine watches the pending map for self-cleanup;
	// it exits when the entry is no longer present (resolved or timed
	// out) or when ctx fires.
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
		timeout = transport.DefaultCorrelatorTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-ch:
		// handleControl already removed the entry from pending; just
		// drop the customID side-table now that the wait is done.
		c.mu.Lock()
		delete(c.customID, batchID)
		c.mu.Unlock()
		return map[string]*BatchResult{customID: result}, nil
	case <-timer.C:
		c.releasePending(batchID)
		return nil, fmt.Errorf("controlPlaneBatchClient: batch_expired: timed out after %s waiting for batch_result (batchID=%s)", timeout, batchID)
	case <-ctx.Done():
		c.releasePending(batchID)
		return nil, fmt.Errorf("controlPlaneBatchClient: cancelled: %w", ctx.Err())
	}
}

// releasePending removes a pending entry. Safe to call when the entry has
// already been removed (e.g. because handleControl resolved it
// concurrently with a timeout firing).
func (c *controlPlaneBatchClient) releasePending(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	delete(c.customID, requestID)
	c.mu.Unlock()
}

// nextRequestID issues a monotonically increasing request ID. The
// "batch-<n>" shape mirrors the askupstream "perm-<n>" convention from
// transport.Correlator.
func (c *controlPlaneBatchClient) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return fmt.Sprintf("batch-%d", c.nextID)
}

// heartbeat emits a batch_waiting HarnessEvent at the configured cadence
// until ctx fires or the pending entry is removed. Errors from Emit are
// ignored — a transport that breaks mid-wait will surface the same break
// to Result via the underlying RPC, which is a more reliable signal than
// a heartbeat error.
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
				Type:      "batch_waiting",
				RequestID: requestID,
			})
		}
	}
}
