package permission

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// mockTransport implements the Transport interface for testing.
type mockTransport struct {
	mu       sync.Mutex
	emitted  []types.HarnessEvent
	handlers []func(types.ControlEvent)
	emitErr  error
}

func (m *mockTransport) Emit(event types.HarnessEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.emitErr != nil {
		return m.emitErr
	}
	m.emitted = append(m.emitted, event)
	return nil
}

func (m *mockTransport) OnControl(handler func(types.ControlEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

// simulateControlEvent delivers a control event to all registered handlers.
func (m *mockTransport) simulateControlEvent(event types.ControlEvent) {
	m.mu.Lock()
	handlers := make([]func(types.ControlEvent), len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()

	for _, h := range handlers {
		h(event)
	}
}

func (m *mockTransport) lastEmitted() *types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.emitted) == 0 {
		return nil
	}
	e := m.emitted[len(m.emitted)-1]
	return &e
}

func sideEffectingSet() map[string]bool {
	return map[string]bool{
		"write_file":        true,
		"run_shell_command": true,
	}
}

func TestAskUpstream_AutoAllowsReadOnlyTools(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("read-only tool should be auto-allowed")
	}
	if mt.lastEmitted() != nil {
		t.Error("should not emit any event for read-only tools")
	}
}

func TestAskUpstream_EmitsRequestForSideEffectingTool(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path":"/tmp/test","content":"hello"}`)

	// Respond with approval asynchronously.
	go func() {
		// Wait for the event to be emitted.
		for {
			time.Sleep(5 * time.Millisecond)
			e := mt.lastEmitted()
			if e != nil && e.Type == "permission_request" {
				allowed := true
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: e.RequestID,
					Allowed:   &allowed,
				})
				return
			}
		}
	}()

	result, err := policy.Check(context.Background(), tool, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("should be allowed after upstream approval")
	}

	e := mt.lastEmitted()
	if e == nil {
		t.Fatal("expected a permission_request event to be emitted")
	}
	if e.Type != "permission_request" {
		t.Errorf("expected type permission_request, got %q", e.Type)
	}
	if e.ToolName != "write_file" {
		t.Errorf("expected tool name write_file, got %q", e.ToolName)
	}
	if e.RequestID == "" {
		t.Error("expected non-empty request ID")
	}
}

func TestAskUpstream_ApprovalResponse(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "run_shell_command"}

	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			e := mt.lastEmitted()
			if e != nil && e.Type == "permission_request" {
				allowed := true
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: e.RequestID,
					Allowed:   &allowed,
				})
				return
			}
		}
	}()

	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed after approval")
	}
}

func TestAskUpstream_DenialResponse(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "write_file"}

	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			e := mt.lastEmitted()
			if e != nil && e.Type == "permission_request" {
				allowed := false
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: e.RequestID,
					Allowed:   &allowed,
					Reason:    "user denied the operation",
				})
				return
			}
		}
	}()

	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected denied after denial response")
	}
	if result.Reason != "user denied the operation" {
		t.Errorf("expected denial reason, got %q", result.Reason)
	}
}

func TestAskUpstream_ContextCancellation(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "write_file"}

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the request is emitted.
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			if mt.lastEmitted() != nil {
				cancel()
				return
			}
		}
	}()

	result, err := policy.Check(ctx, tool, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if result != nil {
		t.Error("expected nil result on cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in error chain, got: %v", err)
	}

	// Verify that the pending request was cleaned up.
	policy.mu.Lock()
	pendingCount := len(policy.pending)
	policy.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests after cancellation, got %d", pendingCount)
	}
}

func TestAskUpstream_EmitError(t *testing.T) {
	mt := &mockTransport{emitErr: errors.New("transport broken")}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "write_file"}

	result, err := policy.Check(context.Background(), tool, nil)
	if err == nil {
		t.Fatal("expected error when emit fails")
	}
	if result != nil {
		t.Error("expected nil result on emit error")
	}

	// Verify pending map is cleaned up.
	policy.mu.Lock()
	pendingCount := len(policy.pending)
	policy.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("expected 0 pending requests after emit error, got %d", pendingCount)
	}
}

func TestAskUpstream_IgnoresUnrelatedControlEvents(t *testing.T) {
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet())

	tool := types.ToolDefinition{Name: "write_file"}

	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			e := mt.lastEmitted()
			if e != nil && e.Type == "permission_request" {
				// Send an unrelated event first.
				mt.simulateControlEvent(types.ControlEvent{
					Type: "user_response",
				})
				// Send a response with the wrong request ID.
				wrongAllowed := true
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: "wrong-id",
					Allowed:   &wrongAllowed,
				})
				// Now send the correct response.
				allowed := true
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: e.RequestID,
					Allowed:   &allowed,
				})
				return
			}
		}
	}()

	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed after correct response arrives")
	}
}
