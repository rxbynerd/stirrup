package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// fakeBatchClient is a controllable BatchClient for table-driven tests.
type fakeBatchClient struct {
	submitErr error
	submitFn  func(entries []BatchEntry) (string, error)

	resultErr error
	resultFn  func(batchID string) (map[string]*BatchResult, error)

	mu             sync.Mutex
	submitted      [][]BatchEntry
	resultRequests []string
}

func (f *fakeBatchClient) Submit(_ context.Context, entries []BatchEntry) (string, error) {
	f.mu.Lock()
	f.submitted = append(f.submitted, entries)
	f.mu.Unlock()

	if f.submitFn != nil {
		return f.submitFn(entries)
	}
	if f.submitErr != nil {
		return "", f.submitErr
	}
	return "fake-batch-1", nil
}

func (f *fakeBatchClient) Result(_ context.Context, batchID string) (map[string]*BatchResult, error) {
	f.mu.Lock()
	f.resultRequests = append(f.resultRequests, batchID)
	f.mu.Unlock()

	if f.resultFn != nil {
		return f.resultFn(batchID)
	}
	if f.resultErr != nil {
		return nil, f.resultErr
	}
	return nil, errors.New("fakeBatchClient: no result configured")
}

// stubProvider is a streaming ProviderAdapter that replays a fixed sequence
// of StreamEvents. Used to assert FallbackOnTimeout pumps the inner.
type stubProvider struct {
	events []types.StreamEvent
	called atomic.Int64
}

func (s *stubProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	s.called.Add(1)
	ch := make(chan types.StreamEvent, len(s.events))
	go func() {
		defer close(ch)
		for _, ev := range s.events {
			ch <- ev
		}
	}()
	return ch, nil
}

// drain reads everything from ch with a generous wall-clock cap so a hung
// goroutine fails the test rather than wedging the whole suite.
func drain(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var out []types.StreamEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("drain: timed out waiting for channel close (collected %d events)", len(out))
		}
	}
}

// -----------------------------------------------------------------------------
// fabricateStream
// -----------------------------------------------------------------------------

func TestFabricateStream_AnthropicTextAndToolUse(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "text", "text": "hello "},
			{"type": "text", "text": "world"},
			{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": {"path": "/etc/hosts"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 42}
	}`)

	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, response, "anthropic")
	close(ch)

	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}

	if len(got) != 4 {
		t.Fatalf("expected 4 events (2 text_delta + 1 tool_call + 1 message_complete), got %d: %+v", len(got), got)
	}
	if got[0].Type != "text_delta" || got[0].Text != "hello " {
		t.Errorf("event 0: got %+v, want text_delta(hello )", got[0])
	}
	if got[1].Type != "text_delta" || got[1].Text != "world" {
		t.Errorf("event 1: got %+v, want text_delta(world)", got[1])
	}
	if got[2].Type != "tool_call" || got[2].ID != "tu_1" || got[2].Name != "read_file" {
		t.Errorf("event 2: got %+v, want tool_call(tu_1, read_file)", got[2])
	}
	if got[2].Input["path"] != "/etc/hosts" {
		t.Errorf("event 2 input: got %+v, want path=/etc/hosts", got[2].Input)
	}
	if got[3].Type != "message_complete" {
		t.Fatalf("event 3: got %+v, want message_complete", got[3])
	}
	if got[3].StopReason != "tool_use" {
		t.Errorf("event 3 stop_reason: got %q, want tool_use", got[3].StopReason)
	}
	if got[3].OutputTokens != 42 {
		t.Errorf("event 3 output_tokens: got %d, want 42", got[3].OutputTokens)
	}
	// message_complete carries the assembled content blocks the
	// streaming consumer would have produced for the same response.
	if len(got[3].Content) != 3 {
		t.Fatalf("event 3 content: got %d blocks, want 3", len(got[3].Content))
	}
}

func TestFabricateStream_OpenAINotImplemented(t *testing.T) {
	for _, provType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(provType, func(t *testing.T) {
			ch := make(chan types.StreamEvent, 1)
			fabricateStream(ch, []byte(`{}`), provType)
			close(ch)

			got := <-ch
			if got.Type != "error" {
				t.Errorf("got type %q, want error", got.Type)
			}
			if got.Error == nil || !strings.Contains(got.Error.Error(), "OpenAI batch fabrication not yet implemented") {
				t.Errorf("expected 'not yet implemented' error, got: %v", got.Error)
			}
		})
	}
}

func TestFabricateStream_UnsupportedProviderEmitsError(t *testing.T) {
	ch := make(chan types.StreamEvent, 1)
	fabricateStream(ch, []byte(`{}`), "bedrock")
	close(ch)
	got := <-ch
	if got.Type != "error" {
		t.Errorf("got type %q, want error", got.Type)
	}
	if got.Error == nil || !strings.Contains(got.Error.Error(), "unsupported provider type") {
		t.Errorf("expected unsupported-provider error, got %v", got.Error)
	}
}

// -----------------------------------------------------------------------------
// BatchAdapter
// -----------------------------------------------------------------------------

func batchAdapter(t *testing.T, client BatchClient, cfg *types.BatchProviderConfig, inner ProviderAdapter) *BatchAdapter {
	t.Helper()
	return NewBatchAdapter(inner, client, cfg, "anthropic", "run-test")
}

func anthropicParams() types.StreamParams {
	return types.StreamParams{
		Model:     "claude-3-5-sonnet-20241022",
		System:    "be brief",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	}
}

func TestBatchAdapter_Stream_HappyPath(t *testing.T) {
	response := json.RawMessage(`{
		"content": [{"type": "text", "text": "hello"}],
		"stop_reason": "end_turn",
		"usage": {"output_tokens": 7}
	}`)
	client := &fakeBatchClient{
		resultFn: func(batchID string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{
				"stirrup-run-test-turn-1": {Response: response},
			}, nil
		},
	}

	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "hello" {
		t.Errorf("event 0: %+v", events[0])
	}
	if events[1].Type != "message_complete" || events[1].StopReason != "end_turn" {
		t.Errorf("event 1: %+v", events[1])
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.submitted) != 1 || len(client.submitted[0]) != 1 {
		t.Fatalf("expected one single-entry submit, got %+v", client.submitted)
	}
	if client.submitted[0][0].CustomID != "stirrup-run-test-turn-1" {
		t.Errorf("custom_id: got %q, want stirrup-run-test-turn-1", client.submitted[0][0].CustomID)
	}
	if client.submitted[0][0].Provider != "anthropic" {
		t.Errorf("provider: got %q, want anthropic", client.submitted[0][0].Provider)
	}
}

func TestBatchAdapter_Stream_TurnCounterIncrements(t *testing.T) {
	emptyResponse := json.RawMessage(`{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}`)
	client := &fakeBatchClient{
		resultFn: func(batchID string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{}, nil // missing -> error, but we only inspect submitted
		},
		submitFn: func(entries []BatchEntry) (string, error) { return "id", nil },
	}
	_ = emptyResponse

	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	for i := 0; i < 3; i++ {
		ch, err := a.Stream(context.Background(), anthropicParams())
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		_ = drain(t, ch)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.submitted) != 3 {
		t.Fatalf("expected 3 submits, got %d", len(client.submitted))
	}
	for i, batch := range client.submitted {
		want := fmt.Sprintf("stirrup-run-test-turn-%d", i+1)
		if batch[0].CustomID != want {
			t.Errorf("submit %d: custom_id %q, want %q", i, batch[0].CustomID, want)
		}
	}
}

func TestBatchAdapter_Stream_SubmitError(t *testing.T) {
	client := &fakeBatchClient{submitErr: errors.New("network unreachable")}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream returned err: %v (should surface via channel)", err)
	}
	events := drain(t, ch)
	if len(events) != 1 {
		t.Fatalf("expected 1 error event, got %d: %+v", len(events), events)
	}
	if events[0].Type != "error" {
		t.Errorf("got type %q, want error", events[0].Type)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "network unreachable") {
		t.Errorf("expected submit error chain, got: %v", events[0].Error)
	}
}

// TestBatchAdapter_LastBatchID_EmptyBeforeSubmit pins the contract
// documented on LastBatchID(): readers (the agentic loop in #138)
// must see an empty string when no batch has been submitted yet, so
// a streaming-only fallback path can detect the absence cleanly.
func TestBatchAdapter_LastBatchID_EmptyBeforeSubmit(t *testing.T) {
	a := batchAdapter(t, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, nil)
	if got := a.LastBatchID(); got != "" {
		t.Errorf("LastBatchID before Submit = %q, want empty", got)
	}
}

// TestBatchAdapter_LastBatchID_PopulatedAfterSubmit confirms the
// adapter surfaces the provider-assigned batch identifier returned
// from Submit so the agentic loop can attach it to the TurnTrace.
// Both the control-plane handle ("batch-N") and the polling client's
// "msgbatch_..." flow through this single accessor unchanged.
func TestBatchAdapter_LastBatchID_PopulatedAfterSubmit(t *testing.T) {
	response := json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	client := &fakeBatchClient{
		submitFn: func(_ []BatchEntry) (string, error) { return "msgbatch_abc123", nil },
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"stirrup-run-test-turn-1": {Response: response}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drain(t, ch)

	if got := a.LastBatchID(); got != "msgbatch_abc123" {
		t.Errorf("LastBatchID after Submit = %q, want %q", got, "msgbatch_abc123")
	}
}

// TestBatchAdapter_LastBatchID_UnchangedOnSubmitError pins the
// rationale called out in the field doc: a failed Submit leaves the
// previous batch identifier in place rather than clobbering with "".
// A loop that reads LastBatchID() on a fallback path then still
// references the most recent successful submission, not a transient
// failure that produced no provider-side batch.
func TestBatchAdapter_LastBatchID_UnchangedOnSubmitError(t *testing.T) {
	response := json.RawMessage(`{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}`)
	calls := 0
	client := &fakeBatchClient{
		submitFn: func(_ []BatchEntry) (string, error) {
			calls++
			if calls == 1 {
				return "msgbatch_first", nil
			}
			return "", errors.New("transient submit failure")
		},
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"stirrup-run-test-turn-1": {Response: response}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drain(t, ch)
	if got := a.LastBatchID(); got != "msgbatch_first" {
		t.Fatalf("LastBatchID after first Submit = %q, want msgbatch_first", got)
	}

	// Second Stream Submit returns an error; LastBatchID must still
	// hold the previous successful value.
	ch2, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream 2: %v", err)
	}
	_ = drain(t, ch2)
	if got := a.LastBatchID(); got != "msgbatch_first" {
		t.Errorf("LastBatchID after failed Submit = %q, want msgbatch_first (unchanged)", got)
	}
}

func TestBatchAdapter_Stream_ResultError(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{
				"stirrup-run-test-turn-1": {Err: &BatchResultError{Type: "batch_cancelled", Message: "user aborted"}},
			}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if msg := events[0].Error.Error(); !strings.Contains(msg, "batch_cancelled") || !strings.Contains(msg, "user aborted") {
		t.Errorf("error chain: %q (want both 'batch_cancelled' and 'user aborted')", msg)
	}
}

func TestBatchAdapter_Stream_MissingEntry(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"other-id": {Response: json.RawMessage(`{}`)}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if !strings.Contains(events[0].Error.Error(), "missing entry") {
		t.Errorf("expected 'missing entry' in error, got %q", events[0].Error)
	}
}

func TestBatchAdapter_Stream_CtxCancel(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, context.Canceled
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := a.Stream(ctx, anthropicParams())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if !strings.Contains(events[0].Error.Error(), "cancelled") {
		t.Errorf("expected 'cancelled' in error, got %q", events[0].Error)
	}
}

func TestBatchAdapter_Stream_TimeoutWithoutFallback(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: false}, nil)
	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if !errors.Is(events[0].Error, errBatchExpired) {
		t.Errorf("expected errBatchExpired in chain, got %v", events[0].Error)
	}
}

func TestBatchAdapter_Stream_TimeoutFallback(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &stubProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "fallback"},
			{Type: "message_complete", StopReason: "end_turn", OutputTokens: 3},
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)

	if inner.called.Load() != 1 {
		t.Errorf("expected inner adapter to be called once, got %d", inner.called.Load())
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events from inner, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "fallback" {
		t.Errorf("event 0: %+v", events[0])
	}
	if events[1].Type != "message_complete" {
		t.Errorf("event 1: %+v", events[1])
	}
}

func TestBatchAdapter_Stream_UnsupportedProviderEmitsError(t *testing.T) {
	client := &fakeBatchClient{}
	a := NewBatchAdapter(nil, client, &types.BatchProviderConfig{Enabled: true}, "bedrock", "run-test")
	ch, err := a.Stream(context.Background(), anthropicParams())
	if err != nil {
		t.Fatalf("Stream returned err: %v (should surface via channel)", err)
	}
	events := drain(t, ch)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if !strings.Contains(events[0].Error.Error(), `unsupported provider type "bedrock"`) {
		t.Errorf("expected unsupported provider error, got: %v", events[0].Error)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.submitted) != 0 {
		t.Errorf("expected no submits for unsupported provider, got %+v", client.submitted)
	}
}

// -----------------------------------------------------------------------------
// controlPlaneBatchClient
// -----------------------------------------------------------------------------

type mockBatchTransport struct {
	mu       sync.Mutex
	emitted  []types.HarnessEvent
	handlers []func(types.ControlEvent)
	// emitErr, when non-nil and emitErrTypes is empty, causes every
	// Emit call to fail with that error. When emitErrTypes is populated,
	// only matching event types fail; others succeed. This lets a test
	// drive submission failures while still observing later cancel
	// emits, etc.
	emitErr      error
	emitErrTypes map[string]bool
}

func (m *mockBatchTransport) Emit(event types.HarnessEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitted = append(m.emitted, event)
	if m.emitErr != nil {
		if len(m.emitErrTypes) == 0 || m.emitErrTypes[event.Type] {
			return m.emitErr
		}
	}
	return nil
}

func (m *mockBatchTransport) OnControl(handler func(types.ControlEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

func (m *mockBatchTransport) Close() error { return nil }

func (m *mockBatchTransport) deliver(event types.ControlEvent) {
	m.mu.Lock()
	hs := make([]func(types.ControlEvent), len(m.handlers))
	copy(hs, m.handlers)
	m.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

func (m *mockBatchTransport) lastEmitted() *types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.emitted) == 0 {
		return nil
	}
	e := m.emitted[len(m.emitted)-1]
	return &e
}

func (m *mockBatchTransport) emittedSnapshot() []types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.HarnessEvent, len(m.emitted))
	copy(out, m.emitted)
	return out
}

func TestControlPlaneBatchClient_SubmitAndResult(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 5*time.Second, false)

	entry := BatchEntry{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{"model":"claude-3-5-sonnet-20241022"}`),
	}

	batchID, err := c.Submit(context.Background(), []BatchEntry{entry})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	em := tr.lastEmitted()
	if em == nil || em.Type != "batch_submission" {
		t.Fatalf("expected batch_submission emitted, got %+v", em)
	}
	if em.RequestID != batchID {
		t.Errorf("requestID echoed: got %q, want %q", em.RequestID, batchID)
	}
	var payload BatchSubmission
	if err := json.Unmarshal(em.Input, &payload); err != nil {
		t.Fatalf("unmarshal submission payload: %v", err)
	}
	if payload.ProviderType != "anthropic" || payload.CustomID != "run-test-turn-1" {
		t.Errorf("payload: %+v", payload)
	}
	if string(payload.Body) != string(entry.Body) {
		t.Errorf("payload body mismatch")
	}

	// Inject the matching batch_result; Result should return a map keyed
	// by the original custom_id.
	go func() {
		// Small delay so Result blocks first.
		time.Sleep(20 * time.Millisecond)
		tr.deliver(types.ControlEvent{
			Type:      "batch_result",
			RequestID: batchID,
			Content:   `{"response":{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}}`,
		})
	}()

	results, err := c.Result(context.Background(), batchID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	got, ok := results["run-test-turn-1"]
	if !ok {
		t.Fatalf("expected result keyed by custom_id, got %+v", results)
	}
	if got.Err != nil {
		t.Errorf("expected success, got err %+v", got.Err)
	}
	if !strings.Contains(string(got.Response), `"text":"hi"`) {
		t.Errorf("unexpected response: %s", got.Response)
	}

	// Cleanup proof (R10): a second Result on the same batchID surfaces
	// "no pending submission", confirming the success path called
	// releasePending and dropped both maps.
	if _, err := c.Result(context.Background(), batchID); err == nil ||
		!strings.Contains(err.Error(), "no pending submission") {
		t.Errorf("expected 'no pending submission' on second Result, got %v", err)
	}
}

func TestControlPlaneBatchClient_SubmitMultiEntryRejected(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)
	_, err := c.Submit(context.Background(), []BatchEntry{{}, {}})
	if err == nil || !strings.Contains(err.Error(), "expected exactly 1 entry") {
		t.Errorf("expected single-entry contract error, got: %v", err)
	}
}

func TestControlPlaneBatchClient_Result_Timeout(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 50*time.Millisecond, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	_, err = c.Result(context.Background(), batchID)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, errBatchExpired) {
		t.Errorf("expected errBatchExpired in chain, got %v", err)
	}
}

func TestControlPlaneBatchClient_Result_ContextCancel(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = c.Result(ctx, batchID)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in chain, got: %v", err)
	}
}

func TestControlPlaneBatchClient_BatchWaitingHeartbeat(t *testing.T) {
	prev := setBatchWaitingHeartbeatInterval(25 * time.Millisecond)
	t.Cleanup(func() { setBatchWaitingHeartbeatInterval(prev) })

	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Resolve the batch after enough ticks have fired so we can assert
	// at least two heartbeats land alongside the result.
	go func() {
		time.Sleep(90 * time.Millisecond)
		tr.deliver(types.ControlEvent{
			Type:      "batch_result",
			RequestID: batchID,
			Content:   `{"response":{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
		})
	}()

	if _, err := c.Result(context.Background(), batchID); err != nil {
		t.Fatalf("Result: %v", err)
	}

	// Settle: give the heartbeat goroutine one extra tick to notice the
	// pending entry was removed and exit cleanly.
	time.Sleep(40 * time.Millisecond)

	var waiting int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_waiting" && ev.RequestID == batchID {
			waiting++
		}
	}
	if waiting < 2 {
		t.Errorf("expected at least 2 batch_waiting heartbeats, got %d (emitted=%+v)", waiting, tr.emittedSnapshot())
	}
}

func TestControlPlaneBatchClient_IgnoresUnrelatedControlEvents(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		// Unrelated event types and wrong request IDs must not unblock
		// the Result wait.
		tr.deliver(types.ControlEvent{Type: "permission_response", RequestID: batchID})
		tr.deliver(types.ControlEvent{Type: "batch_result", RequestID: "wrong-id"})
		// Empty content surfaces a synthetic invalid_request_error so
		// the BatchAdapter still sees a non-nil entry.
		tr.deliver(types.ControlEvent{Type: "batch_result", RequestID: batchID})
	}()

	results, err := c.Result(context.Background(), batchID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	got, ok := results["run-test-turn-1"]
	if !ok || got == nil {
		t.Fatalf("expected entry, got %+v", results)
	}
	if got.Err == nil || got.Err.Type != "invalid_request_error" {
		t.Errorf("expected synthetic invalid_request_error, got %+v", got.Err)
	}
}

// TestControlPlaneBatchClient_DeliverBeforeResult exercises the B1
// race: handleControl delivers a batch_result before Result is even
// called. With the fix, Result must return the buffered value rather
// than spuriously surfacing "no pending submission".
func TestControlPlaneBatchClient_DeliverBeforeResult(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Deliver the result before Result is called; the buffered channel
	// holds the value until Result drains it.
	tr.deliver(types.ControlEvent{
		Type:      "batch_result",
		RequestID: batchID,
		Content:   `{"response":{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
	})

	results, err := c.Result(context.Background(), batchID)
	if err != nil {
		t.Fatalf("Result after early delivery: %v", err)
	}
	got, ok := results["run-test-turn-1"]
	if !ok || got == nil {
		t.Fatalf("expected entry keyed by custom_id, got %+v", results)
	}
	if got.Err != nil {
		t.Errorf("expected success, got err %+v", got.Err)
	}

	// Cleanup proof: a second Result returns "no pending submission".
	if _, err := c.Result(context.Background(), batchID); err == nil ||
		!strings.Contains(err.Error(), "no pending submission") {
		t.Errorf("expected 'no pending submission' on second Result, got %v", err)
	}
}

// TestControlPlaneBatchClient_CancelBundle_TimeoutEmits asserts B3: a
// timeout exit emits a batch_cancel_request when cancelBundleOnExit=true.
func TestControlPlaneBatchClient_CancelBundle_TimeoutEmits(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 30*time.Millisecond, true)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if _, err := c.Result(context.Background(), batchID); !errors.Is(err, errBatchExpired) {
		t.Fatalf("expected errBatchExpired, got %v", err)
	}

	var cancel int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" && ev.RequestID == batchID {
			cancel++
		}
	}
	if cancel != 1 {
		t.Errorf("expected exactly 1 batch_cancel_request, got %d (emitted=%+v)", cancel, tr.emittedSnapshot())
	}
}

// TestControlPlaneBatchClient_CancelBundle_CtxCancelEmits asserts B3 on
// the ctx-cancel arm.
func TestControlPlaneBatchClient_CancelBundle_CtxCancelEmits(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, true)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	if _, err := c.Result(ctx, batchID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	var cancelEvents int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" && ev.RequestID == batchID {
			cancelEvents++
		}
	}
	if cancelEvents != 1 {
		t.Errorf("expected exactly 1 batch_cancel_request, got %d", cancelEvents)
	}
}

// TestControlPlaneBatchClient_CancelBundle_DisabledNoEmit asserts B3:
// when cancelBundleOnExit=false, the cancel/timeout arms emit nothing
// beyond the original batch_submission.
func TestControlPlaneBatchClient_CancelBundle_DisabledNoEmit(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Result(ctx, batchID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" {
			t.Errorf("unexpected batch_cancel_request emitted with cancelBundleOnExit=false: %+v", ev)
		}
	}
}

// TestDecodeBatchResult_MalformedJSON (B6) asserts a non-JSON content
// surfaces as a synthetic invalid_request_error rather than panicking.
func TestDecodeBatchResult_MalformedJSON(t *testing.T) {
	got := decodeBatchResult(types.ControlEvent{
		Type:      "batch_result",
		RequestID: "batch-1",
		Content:   "{not-json",
	})
	if got == nil || got.Err == nil {
		t.Fatalf("expected synthetic error, got %+v", got)
	}
	if got.Err.Type != "invalid_request_error" {
		t.Errorf("type: got %q, want invalid_request_error", got.Err.Type)
	}
	if !strings.Contains(got.Err.Message, "decode batch_result") {
		t.Errorf("message: got %q, want substring 'decode batch_result'", got.Err.Message)
	}
}

// TestDecodeBatchResult_SizeCap (B6) asserts a Content payload above
// maxBatchResponseBytes surfaces as a synthetic invalid_request_error
// without attempting to decode the oversized blob.
func TestDecodeBatchResult_SizeCap(t *testing.T) {
	oversized := strings.Repeat("a", maxBatchResponseBytes+1)
	got := decodeBatchResult(types.ControlEvent{
		Type:      "batch_result",
		RequestID: "batch-1",
		Content:   oversized,
	})
	if got == nil || got.Err == nil {
		t.Fatalf("expected synthetic error, got %+v", got)
	}
	if got.Err.Type != "invalid_request_error" {
		t.Errorf("type: got %q, want invalid_request_error", got.Err.Type)
	}
	if !strings.Contains(got.Err.Message, "exceeds") {
		t.Errorf("message: got %q, want substring 'exceeds'", got.Err.Message)
	}
}

// TestControlPlaneBatchClient_SubmitEmitFailureCleansUp (B7) drives an
// Emit failure on batch_submission and asserts the pending entry was
// dropped (no leak; subsequent Result reports "no pending submission").
func TestControlPlaneBatchClient_SubmitEmitFailureCleansUp(t *testing.T) {
	tr := &mockBatchTransport{
		emitErr:      errors.New("simulated emit failure"),
		emitErrTypes: map[string]bool{"batch_submission": true},
	}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err == nil {
		t.Fatal("expected emit error from Submit")
	}
	if !strings.Contains(err.Error(), "simulated emit failure") {
		t.Errorf("expected wrapped emit error, got %v", err)
	}
	if batchID != "" {
		t.Errorf("Submit returned non-empty batchID on failure: %q", batchID)
	}
	emitted := tr.emittedSnapshot()
	if len(emitted) != 1 || emitted[0].Type != "batch_submission" {
		t.Fatalf("expected single batch_submission emit, got %+v", emitted)
	}
	requestID := emitted[0].RequestID

	if _, err := c.Result(context.Background(), requestID); err == nil ||
		!strings.Contains(err.Error(), "no pending submission") {
		t.Errorf("expected 'no pending submission' after Submit failure, got %v", err)
	}
}

// TestControlPlaneBatchClient_HeartbeatExitsOnCtxCancel (R3) asserts the
// heartbeat goroutine terminates promptly when its context is cancelled
// before any tick has fired.
func TestControlPlaneBatchClient_HeartbeatExitsOnCtxCancel(t *testing.T) {
	// Use a long interval so the ticker cannot fire during the test
	// window; the goroutine should exit via the ctx.Done() arm.
	prev := setBatchWaitingHeartbeatInterval(time.Hour)
	t.Cleanup(func() { setBatchWaitingHeartbeatInterval(prev) })

	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	ctx, cancel := context.WithCancel(context.Background())
	batchID, err := c.Submit(ctx, []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cancel()
	c.releasePending(batchID)

	// Settle. With the long interval, no batch_waiting events should
	// have been emitted; only the original batch_submission is on the
	// wire.
	time.Sleep(20 * time.Millisecond)
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_waiting" {
			t.Errorf("unexpected batch_waiting emitted before ticker fired: %+v", ev)
		}
	}
}

// TestBatchAdapter_Stream_TimeoutFallback_InnerStreamError (R5) asserts
// that when FallbackOnTimeout fires and the inner adapter itself
// returns an error, the BatchAdapter surfaces a single error event.
func TestBatchAdapter_Stream_TimeoutFallback_InnerStreamError(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &erroringProvider{err: errors.New("upstream provider down")}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)

	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("expected single error event, got %+v", events)
	}
	if !strings.Contains(events[0].Error.Error(), "batch fallback to streaming failed") {
		t.Errorf("expected fallback-failure error, got %v", events[0].Error)
	}
}

// erroringProvider is a ProviderAdapter that always fails on Stream.
type erroringProvider struct{ err error }

func (e *erroringProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	return nil, e.err
}

// TestBatchAdapter_Stream_TimeoutFallback_CtxCancelDuringRelay (R5)
// asserts the channel closes cleanly when ctx is cancelled mid-relay.
func TestBatchAdapter_Stream_TimeoutFallback_CtxCancelDuringRelay(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &blockingProvider{first: types.StreamEvent{Type: "text_delta", Text: "first"}, release: make(chan struct{})}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := a.Stream(ctx, anthropicParams())

	// Read the first event from the relay, then cancel.
	deadline := time.After(2 * time.Second)
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before first event")
		}
		if ev.Type != "text_delta" || ev.Text != "first" {
			t.Fatalf("unexpected first event: %+v", ev)
		}
	case <-deadline:
		t.Fatal("timed out waiting for first event from inner")
	}

	cancel()
	close(inner.release)

	// Channel must close cleanly (no goroutine leak). The relay races
	// the second send against ctx.Done() per event — Go's select is
	// non-deterministic so the second event may or may not land before
	// pumpInner notices the cancellation. Either way, the channel must
	// close without wedging.
	deadline = time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // success: closed cleanly
			}
		case <-deadline:
			t.Fatal("channel did not close after ctx cancel")
		}
	}
}

// blockingProvider emits `first`, then waits for release to close
// before emitting a second event and closing the channel. Used to drive
// the ctx-cancel-during-relay path.
type blockingProvider struct {
	first   types.StreamEvent
	release chan struct{}
}

func (b *blockingProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	go func() {
		defer close(ch)
		ch <- b.first
		<-b.release
		ch <- types.StreamEvent{Type: "text_delta", Text: "second"}
	}()
	return ch, nil
}

// TestBatchAdapter_Stream_OpenAIVariantsEmitNotImplemented (R6) pins
// the phase-6 OpenAI integration point: both openai-compatible and
// openai-responses provType values surface a single
// "not yet implemented" error event from the marshal/fabricate path.
func TestBatchAdapter_Stream_OpenAIVariantsEmitNotImplemented(t *testing.T) {
	for _, provType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(provType, func(t *testing.T) {
			response := json.RawMessage(`{}`)
			client := &fakeBatchClient{
				resultFn: func(_ string) (map[string]*BatchResult, error) {
					return map[string]*BatchResult{
						"stirrup-run-test-turn-1": {Response: response},
					}, nil
				},
			}
			a := NewBatchAdapter(nil, client, &types.BatchProviderConfig{Enabled: true}, provType, "run-test")
			ch, err := a.Stream(context.Background(), anthropicParams())
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			events := drain(t, ch)
			if len(events) != 1 || events[0].Type != "error" {
				t.Fatalf("expected single error event, got %+v", events)
			}
			if !strings.Contains(events[0].Error.Error(), "OpenAI batch fabrication not yet implemented") {
				t.Errorf("expected 'not yet implemented' error, got %v", events[0].Error)
			}
		})
	}
}

// TestFabricateStream_AnthropicParityWithReference (R8) compares the
// fabricator's output against a hand-rolled reference sequence for a
// single text + tool_use fixture. Guards against drift between the SSE
// consumer (consumeSSE in anthropic.go) and the batch fabricator.
func TestFabricateStream_AnthropicParityWithReference(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "text", "text": "hi"},
			{"type": "tool_use", "id": "tu_a", "name": "read_file", "input": {"path": "/etc/hosts"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 5}
	}`)

	ch := make(chan types.StreamEvent, 8)
	if err := fabricateAnthropicStream(ch, response); err != nil {
		t.Fatalf("fabricateAnthropicStream: %v", err)
	}
	close(ch)

	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}

	// Reference sequence: one text_delta per text block, one tool_call
	// per tool_use block, then a single message_complete carrying the
	// assembled content blocks.
	want := []types.StreamEvent{
		{Type: "text_delta", Text: "hi"},
		{Type: "tool_call", ID: "tu_a", Name: "read_file", Input: map[string]any{"path": "/etc/hosts"}},
		{Type: "message_complete", StopReason: "tool_use", OutputTokens: 5, Content: []types.ContentBlock{
			{Type: "text", Text: "hi"},
			{Type: "tool_use", ID: "tu_a", Name: "read_file", Input: json.RawMessage(`{"path": "/etc/hosts"}`)},
		}},
	}
	if len(got) != len(want) {
		t.Fatalf("event count: got %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("event %d: type %q, want %q", i, got[i].Type, want[i].Type)
		}
		if got[i].Text != want[i].Text {
			t.Errorf("event %d: text %q, want %q", i, got[i].Text, want[i].Text)
		}
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name {
			t.Errorf("event %d: id/name %q/%q, want %q/%q", i, got[i].ID, got[i].Name, want[i].ID, want[i].Name)
		}
		if want[i].StopReason != "" && got[i].StopReason != want[i].StopReason {
			t.Errorf("event %d: stop_reason %q, want %q", i, got[i].StopReason, want[i].StopReason)
		}
		if want[i].OutputTokens != 0 && got[i].OutputTokens != want[i].OutputTokens {
			t.Errorf("event %d: output_tokens %d, want %d", i, got[i].OutputTokens, want[i].OutputTokens)
		}
		// Tool input check (event index 1).
		if want[i].Type == "tool_call" {
			if got[i].Input["path"] != want[i].Input["path"] {
				t.Errorf("event %d: tool input path %v, want %v", i, got[i].Input, want[i].Input)
			}
		}
	}
}
