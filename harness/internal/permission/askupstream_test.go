package permission

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// respondToNextRequest answers the next permission_request the policy emits
// with the given decision. It runs on a background goroutine and must not
// call require.
func respondToNextRequest(mt *mockTransport, allowed bool, reason string) {
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			e := mt.lastEmitted()
			if e != nil && e.Type == "permission_request" {
				mt.simulateControlEvent(types.ControlEvent{
					Type:      "permission_response",
					RequestID: e.RequestID,
					Allowed:   &allowed,
					Reason:    reason,
				})
				return
			}
		}
	}()
}

func TestAskUpstream_AutoAllowsReadOnlyTools(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	check.True(result.Allowed, "read-only tool should be auto-allowed")
	check.Nil(mt.lastEmitted(), "should not emit any event for read-only tools")
}

// TestAskUpstream_MCPPrefixRequiresApprovalWhenAbsentFromSnapshot: an
// mcp_-prefixed tool absent from the construction-time approvalTools
// snapshot must still be forwarded for approval, matching
// ruleOfTwoGate.isExternal. Without the prefix fallback the call would
// auto-allow, bypassing the upstream approval the rule-of-two gate routes
// it here to obtain.
func TestAskUpstream_MCPPrefixRequiresApprovalWhenAbsentFromSnapshot(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	// Snapshot deliberately omits the MCP tool.
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "mcp_newserver_tool"}

	// If the prefix fallback is missing, Check returns Allowed immediately
	// without emitting a request, and the approval below is dropped (caught
	// by the lastEmitted check).
	respondToNextRequest(mt, true, "")

	result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
	must.NoError(err)
	check.True(result.Allowed, "expected allow after upstream approval")

	e := mt.lastEmitted()
	must.NotNil(e, "mcp_-prefixed tool must emit a permission_request, not auto-allow")
	must.Equal("permission_request", e.Type, "mcp_-prefixed tool must emit a permission_request, not auto-allow")
	check.Equal("mcp_newserver_tool", e.ToolName)
}

func TestAskUpstream_EmitsRequestForSideEffectingTool(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path":"/tmp/test","content":"hello"}`)

	respondToNextRequest(mt, true, "")

	result, err := policy.Check(context.Background(), tool, input)
	must.NoError(err)
	check.True(result.Allowed, "should be allowed after upstream approval")

	e := mt.lastEmitted()
	must.NotNil(e, "expected a permission_request event to be emitted")
	check.Equal("permission_request", e.Type)
	check.Equal("write_file", e.ToolName)
	check.NotEmpty(e.RequestID, "expected non-empty request ID")
}

func TestAskUpstream_ApprovalResponseAllowsTool(t *testing.T) {
	must := require.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "run_shell_command"}

	respondToNextRequest(mt, true, "")

	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	must.True(result.Allowed)
}

func TestAskUpstream_DenialResponseDeniesTool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "write_file"}

	respondToNextRequest(mt, false, "user denied the operation")

	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	check.False(result.Allowed)
	check.Equal("user denied the operation", result.Reason)
}

func TestAskUpstream_ContextCancelReturnsCanceledError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

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
	must.ErrorIs(err, context.Canceled)
	check.Nil(result)

	// The pending request must be removed once Check returns.
	check.Zero(policy.correlator.PendingCount(), "pending requests after cancellation")
}

func TestAskUpstream_EmitFailureReturnsError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{emitErr: errors.New("transport broken")}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	tool := types.ToolDefinition{Name: "write_file"}

	result, err := policy.Check(context.Background(), tool, nil)
	must.Error(err)
	check.Nil(result)

	// The pending map must be cleaned up.
	check.Zero(policy.correlator.PendingCount(), "pending requests after emit error")
}

func TestAskUpstream_IgnoresUnrelatedControlEvents(t *testing.T) {
	must := require.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

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
	must.NoError(err)
	must.True(result.Allowed, "expected allowed after correct response arrives")
}

func TestAskUpstream_TimeoutReturnsError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	// Use a very short timeout so the test completes quickly.
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 50*time.Millisecond)

	tool := types.ToolDefinition{Name: "write_file"}

	// No response is sent, so the request times out.
	result, err := policy.Check(context.Background(), tool, nil)
	must.ErrorContains(err, "timed out")
	check.Nil(result)

	// The pending request must be removed once Check returns.
	check.Zero(policy.correlator.PendingCount(), "pending requests after timeout")
}

func TestAskUpstream_ZeroTimeoutUsesDefault(t *testing.T) {
	must := require.New(t)
	mt := &mockTransport{}
	// Pass 0 to get the default timeout.
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	must.Equal(DefaultAskUpstreamTimeout, policy.Timeout)
}
