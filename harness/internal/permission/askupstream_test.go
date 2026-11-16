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

// emittedRequests returns the permission_request events emitted so far.
func (m *mockTransport) emittedRequests() []types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.HarnessEvent
	for _, e := range m.emitted {
		if e.Type == "permission_request" {
			out = append(out, e)
		}
	}
	return out
}

type policyCheckResult struct {
	result *PermissionResult
	err    error
}

// TestAskUpstream_TwoPoliciesOnOneTransport_ResponseResolvesOnlyItsRequest
// pins request-ID uniqueness across every ask-upstream policy sharing a
// transport: the base policy and the rule-of-two gate's ask policy both
// attach to the run's transport, and an approval for one tool must never
// resolve a pending request for another.
func TestAskUpstream_TwoPoliciesOnOneTransport_ResponseResolvesOnlyItsRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	base := NewAskUpstreamPolicy(mt, map[string]bool{"write_file": true}, 5*time.Second)
	gateAsk := NewAskUpstreamPolicy(mt, map[string]bool{"web_fetch": true}, 5*time.Second)

	baseDone := make(chan policyCheckResult, 1)
	go func() {
		r, err := base.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, nil)
		baseDone <- policyCheckResult{r, err}
	}()
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 1 }, time.Second, time.Millisecond)

	gateCtx, cancelGate := context.WithCancel(context.Background())
	defer cancelGate()
	gateDone := make(chan policyCheckResult, 1)
	go func() {
		r, err := gateAsk.Check(gateCtx, types.ToolDefinition{Name: "web_fetch"}, nil)
		gateDone <- policyCheckResult{r, err}
	}()
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 2 }, time.Second, time.Millisecond)

	reqs := mt.emittedRequests()
	must.Equal("write_file", reqs[0].ToolName)
	must.Equal("web_fetch", reqs[1].ToolName)
	must.NotEqual(reqs[0].RequestID, reqs[1].RequestID, "two policies on one transport issued the same request ID")

	allowed := true
	mt.simulateControlEvent(types.ControlEvent{
		Type:      "permission_response",
		RequestID: reqs[0].RequestID,
		Allowed:   &allowed,
	})

	got := <-baseDone
	must.NoError(got.err)
	check.True(got.result.Allowed)

	select {
	case leaked := <-gateDone:
		must.Failf("web_fetch request resolved by the write_file approval", "%+v", leaked)
	case <-time.After(50 * time.Millisecond):
	}
	check.Equal(1, gateAsk.correlator.PendingCount())

	cancelGate()
	gated := <-gateDone
	must.ErrorIs(gated.err, context.Canceled)
	check.Nil(gated.result)
}

// TestRuleOfTwoGate_AskUpstreamEscalation_NotReleasedByUnrelatedApproval
// builds the base ask-upstream policy and the gate's own ask policy on one
// transport, as the factory does. With an escalation pending in the gate,
// approving a different call's base request must leave the escalation
// pending.
func TestRuleOfTwoGate_AskUpstreamEscalation_NotReleasedByUnrelatedApproval(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	base := NewAskUpstreamPolicy(mt, map[string]bool{"write_file": true, "web_fetch": true}, 5*time.Second)
	ext := map[string]bool{"web_fetch": true}
	gateAsk := NewAskUpstreamPolicy(mt, ext, 5*time.Second)
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	gate := NewRuleOfTwoGate(base, state, ext, gateAsk, nil)
	allowed := true

	writeDone := make(chan policyCheckResult, 1)
	go func() {
		r, err := gate.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, nil)
		writeDone <- policyCheckResult{r, err}
	}()
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 1 }, time.Second, time.Millisecond)
	writeID := mt.emittedRequests()[0].RequestID

	fetchCtx, cancelFetch := context.WithCancel(context.Background())
	defer cancelFetch()
	fetchDone := make(chan policyCheckResult, 1)
	go func() {
		r, err := gate.Check(fetchCtx, types.ToolDefinition{Name: "web_fetch"}, nil)
		fetchDone <- policyCheckResult{r, err}
	}()
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 2 }, time.Second, time.Millisecond)
	mt.simulateControlEvent(types.ControlEvent{
		Type:      "permission_response",
		RequestID: mt.emittedRequests()[1].RequestID,
		Allowed:   &allowed,
	})
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 3 }, time.Second, time.Millisecond)
	escalation := mt.emittedRequests()[2]
	must.Equal("web_fetch", escalation.ToolName)
	must.NotEqual(writeID, escalation.RequestID)

	mt.simulateControlEvent(types.ControlEvent{
		Type:      "permission_response",
		RequestID: writeID,
		Allowed:   &allowed,
	})
	wrote := <-writeDone
	must.NoError(wrote.err)
	check.True(wrote.result.Allowed)

	select {
	case leaked := <-fetchDone:
		must.Failf("rule-of-two escalation released by the write_file approval", "%+v", leaked)
	case <-time.After(50 * time.Millisecond):
	}
	check.Equal(1, gateAsk.correlator.PendingCount())

	cancelFetch()
	fetched := <-fetchDone
	must.ErrorIs(fetched.err, context.Canceled)
}

// TestAskUpstream_ReplayedResponseDoesNotResolveLaterRequest: a response
// for an already-resolved request ID is ignored, so the next request
// stays pending and fails closed on timeout instead of inheriting the
// earlier approval.
func TestAskUpstream_ReplayedResponseDoesNotResolveLaterRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 500*time.Millisecond)
	tool := types.ToolDefinition{Name: "write_file"}

	respondToNextRequest(mt, true, "")
	first, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	must.True(first.Allowed)
	firstID := mt.emittedRequests()[0].RequestID

	done := make(chan policyCheckResult, 1)
	go func() {
		r, err := policy.Check(context.Background(), tool, nil)
		done <- policyCheckResult{r, err}
	}()
	must.Eventually(func() bool { return len(mt.emittedRequests()) == 2 }, time.Second, time.Millisecond)
	check.NotEqual(firstID, mt.emittedRequests()[1].RequestID)

	allowed := true
	mt.simulateControlEvent(types.ControlEvent{
		Type:      "permission_response",
		RequestID: firstID,
		Allowed:   &allowed,
	})
	must.Equal(1, policy.correlator.PendingCount(), "the replayed response resolved the pending request")

	second := <-done
	must.ErrorContains(second.err, "timed out")
	check.Nil(second.result)
}
