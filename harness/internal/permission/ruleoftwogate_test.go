package permission

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// fakeRuleOfTwoState is a hand-set RuleOfTwoState for gate tests.
type fakeRuleOfTwoState struct {
	tripped   bool
	enforcing bool
	action    string
}

func (f *fakeRuleOfTwoState) Tripped() bool   { return f.tripped }
func (f *fakeRuleOfTwoState) Enforcing() bool { return f.enforcing }
func (f *fakeRuleOfTwoState) Action() string  { return f.action }

// recordingPolicy counts Check calls and returns a configured result.
type recordingPolicy struct {
	mu     sync.Mutex
	calls  []string
	result *PermissionResult
}

func (r *recordingPolicy) Check(_ context.Context, tool types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, tool.Name)
	if r.result != nil {
		return r.result, nil
	}
	return &PermissionResult{Allowed: true}, nil
}

func (r *recordingPolicy) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func externalSet() map[string]bool {
	return map[string]bool{"run_command": true, "web_fetch": true}
}

func TestRuleOfTwoGate_DelegatesUntilTripped(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: false, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed, "untripped gate must delegate to inner (allow-all)")
	require.Equal(t, 1, inner.callCount(), "inner must be consulted once (delegation)")
}

func TestRuleOfTwoGate_NotEnforcingDelegatesEvenWhenTripped(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: false, action: "warn"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed, "observe-only gate must never deny")
	require.Equal(t, 1, inner.callCount())
}

func TestRuleOfTwoGate_BlockExternalDeniesExternalTools(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	for _, name := range []string{"run_command", "web_fetch"} {
		t.Run(name, func(t *testing.T) {
			result, err := gate.Check(context.Background(), types.ToolDefinition{Name: name}, nil)
			require.NoError(t, err)
			require.False(t, result.Allowed, "external tool must be denied after the latch trips")
			require.Equal(t, RuleOfTwoDeniedReason, result.Reason)
		})
	}
	require.Equal(t, 0, inner.callCount(), "block-external never consults inner")
}

func TestRuleOfTwoGate_NonExternalToolsDelegateWhileTripped(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	for _, name := range []string{"read_file", "write_file", "edit_file", "grep_files"} {
		t.Run(name, func(t *testing.T) {
			result, err := gate.Check(context.Background(), types.ToolDefinition{Name: name}, nil)
			require.NoError(t, err)
			require.True(t, result.Allowed, "not external-comm, must delegate to inner (allow)")
		})
	}
	require.Equal(t, 4, inner.callCount())
}

func TestRuleOfTwoGate_MCPPrefixDeniedEvenWhenAbsentFromSet(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	// External set deliberately omits the MCP tool: the prefix check
	// must cover tools registered after gate construction.
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "mcp_github_create_issue"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "mcp_-prefixed tool must be denied via the prefix check")
	require.Equal(t, RuleOfTwoDeniedReason, result.Reason)
}

func TestRuleOfTwoGate_AskUpstreamConsultsInnerFirst(t *testing.T) {
	// A Cedar forbid (inner deny) must still deny without ever asking
	// upstream: ask-upstream loosens nothing the inner policy decided.
	inner := &recordingPolicy{result: &PermissionResult{Allowed: false, Reason: "cedar forbid"}}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	mt := &mockTransport{}
	ask := NewAskUpstreamPolicy(mt, externalSet(), 0)
	gate := NewRuleOfTwoGate(inner, state, externalSet(), ask, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "inner deny must short-circuit the upstream ask")
	require.Equal(t, "cedar forbid", result.Reason, "reason must be the inner policy's")
	require.Nil(t, mt.lastEmitted(), "no permission_request may be emitted when the inner policy already denied")
}

func TestRuleOfTwoGate_AskUpstreamRoutesAfterInnerAllow(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	mt := &mockTransport{}
	ask := NewAskUpstreamPolicy(mt, externalSet(), 0)
	gate := NewRuleOfTwoGate(inner, state, externalSet(), ask, nil)

	// The operator approves asynchronously.
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

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed, "operator approval must allow the call")
	require.Equal(t, 1, inner.callCount(), "inner must be consulted before the ask")

	e := mt.lastEmitted()
	require.NotNil(t, e, "expected a permission_request on the transport")
	require.Equal(t, "permission_request", e.Type)
	require.Equal(t, "run_command", e.ToolName)
}

func TestRuleOfTwoGate_AskUpstreamDenialPropagates(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	mt := &mockTransport{}
	ask := NewAskUpstreamPolicy(mt, externalSet(), 0)
	gate := NewRuleOfTwoGate(inner, state, externalSet(), ask, nil)

	// The operator denies asynchronously.
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
					Reason:    "operator said no",
				})
				return
			}
		}
	}()

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "web_fetch"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "operator denial must deny the call")
	require.Equal(t, "operator said no", result.Reason, "reason must be the operator's")
}

func TestRuleOfTwoGate_AskUpstreamNilFailsClosed(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "a gate with no ask policy must fail closed, not allow")
	require.Equal(t, RuleOfTwoDeniedReason, result.Reason)
}

func TestRuleOfTwoGate_UnwrapReachesInnerPolicy(t *testing.T) {
	core := NewAllowAll()
	state := &fakeRuleOfTwoState{}
	gate := NewRuleOfTwoGate(core, state, externalSet(), nil, nil)

	require.Same(t, core, Unwrap(gate), "Unwrap must walk through the gate to the inner policy")
}

func TestRewrapChain_RebuildsGateAroundNewInner(t *testing.T) {
	oldInner := &recordingPolicy{}
	newInner := &recordingPolicy{result: &PermissionResult{Allowed: false, Reason: "new inner"}}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(oldInner, state, externalSet(), nil, nil)

	rewrapped, ok := RewrapChain(gate, newInner)
	require.True(t, ok, "RewrapChain must succeed for a gate-only chain")
	require.Same(t, newInner, Unwrap(rewrapped), "rewrapped chain must wrap the new inner")

	// The rebuilt gate shares the tripped state: external tools still
	// denied with the rule-of-two reason, non-external delegate to the
	// NEW inner.
	result, err := rewrapped.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "rebuilt gate must keep denying external tools")
	require.Equal(t, RuleOfTwoDeniedReason, result.Reason)

	result, err = rewrapped.Check(context.Background(), types.ToolDefinition{Name: "read_file"}, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed, "non-external check must reach the new inner")
	require.Equal(t, "new inner", result.Reason)

	require.Equal(t, 0, oldInner.callCount(), "old inner must receive no calls after rewrap")
}

func TestRewrapChain_RefusesNonRewrapperWrapper(t *testing.T) {
	inner := NewAllowAll()
	wrapped := &unwrapOnlyWrapper{inner: inner}
	out, ok := RewrapChain(wrapped, NewAllowAll())
	require.False(t, ok, "RewrapChain must refuse a chain containing a non-Rewrapper wrapper")
	require.Same(t, wrapped, out, "on refusal RewrapChain must return the original chain")
}

// unwrapOnlyWrapper implements Unwrap but not Rewrap, standing in for a
// hypothetical future wrapper that predates the Rewrapper contract.
type unwrapOnlyWrapper struct {
	inner PermissionPolicy
}

func (w *unwrapOnlyWrapper) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	return w.inner.Check(ctx, tool, input)
}

func (w *unwrapOnlyWrapper) Unwrap() PermissionPolicy { return w.inner }

func TestRuleOfTwoGate_DeniedReasonKeepsGrepPrefix(t *testing.T) {
	require.True(t, strings.HasPrefix(RuleOfTwoDeniedReason, "rule_of_two:"),
		"RuleOfTwoDeniedReason must keep the stable rule_of_two: grep prefix, got %q", RuleOfTwoDeniedReason)
}
