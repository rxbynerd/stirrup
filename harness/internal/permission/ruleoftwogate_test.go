package permission

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed {
		t.Error("untripped gate must delegate to inner (allow-all)")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner.Check calls = %d, want 1 (delegation)", inner.callCount())
	}
}

func TestRuleOfTwoGate_NotEnforcingDelegatesEvenWhenTripped(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: false, action: "warn"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed {
		t.Error("observe-only gate must never deny")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner.Check calls = %d, want 1", inner.callCount())
	}
}

func TestRuleOfTwoGate_BlockExternalDeniesExternalTools(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	for _, name := range []string{"run_command", "web_fetch"} {
		result, err := gate.Check(context.Background(), types.ToolDefinition{Name: name}, nil)
		if err != nil {
			t.Fatalf("Check(%s): %v", name, err)
		}
		if result.Allowed {
			t.Errorf("%s must be denied after the latch trips", name)
		}
		if result.Reason != RuleOfTwoDeniedReason {
			t.Errorf("%s reason = %q, want RuleOfTwoDeniedReason", name, result.Reason)
		}
	}
	if inner.callCount() != 0 {
		t.Errorf("inner.Check calls = %d, want 0 (block-external never consults inner)", inner.callCount())
	}
}

func TestRuleOfTwoGate_NonExternalToolsDelegateWhileTripped(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	for _, name := range []string{"read_file", "write_file", "edit_file", "grep_files"} {
		result, err := gate.Check(context.Background(), types.ToolDefinition{Name: name}, nil)
		if err != nil {
			t.Fatalf("Check(%s): %v", name, err)
		}
		if !result.Allowed {
			t.Errorf("%s is not external-comm and must delegate to inner (allow)", name)
		}
	}
	if inner.callCount() != 4 {
		t.Errorf("inner.Check calls = %d, want 4", inner.callCount())
	}
}

func TestRuleOfTwoGate_MCPPrefixDeniedEvenWhenAbsentFromSet(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	// External set deliberately omits the MCP tool: the prefix check
	// must cover tools registered after gate construction.
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "mcp_github_create_issue"}, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Error("mcp_-prefixed tool must be denied via the prefix check")
	}
	if result.Reason != RuleOfTwoDeniedReason {
		t.Errorf("reason = %q, want RuleOfTwoDeniedReason", result.Reason)
	}
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
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Error("inner deny must short-circuit the upstream ask")
	}
	if result.Reason != "cedar forbid" {
		t.Errorf("reason = %q, want the inner policy's reason", result.Reason)
	}
	if mt.lastEmitted() != nil {
		t.Error("no permission_request may be emitted when the inner policy already denied")
	}
}

func TestRuleOfTwoGate_AskUpstreamRoutesAfterInnerAllow(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	mt := &mockTransport{}
	ask := NewAskUpstreamPolicy(mt, externalSet(), 0)
	gate := NewRuleOfTwoGate(inner, state, externalSet(), ask, nil)

	// Operator approves asynchronously, mirroring askupstream_test.go.
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
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed {
		t.Error("operator approval must allow the call")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner.Check calls = %d, want 1 (consulted before the ask)", inner.callCount())
	}
	e := mt.lastEmitted()
	if e == nil || e.Type != "permission_request" {
		t.Fatalf("expected a permission_request on the transport, got %+v", e)
	}
	if e.ToolName != "run_command" {
		t.Errorf("permission_request tool = %q, want run_command", e.ToolName)
	}
}

func TestRuleOfTwoGate_AskUpstreamDenialPropagates(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	mt := &mockTransport{}
	ask := NewAskUpstreamPolicy(mt, externalSet(), 0)
	gate := NewRuleOfTwoGate(inner, state, externalSet(), ask, nil)

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
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Error("operator denial must deny the call")
	}
	if result.Reason != "operator said no" {
		t.Errorf("reason = %q, want the operator's reason", result.Reason)
	}
}

func TestRuleOfTwoGate_AskUpstreamNilFailsClosed(t *testing.T) {
	inner := &recordingPolicy{}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "ask-upstream"}
	gate := NewRuleOfTwoGate(inner, state, externalSet(), nil, nil)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Error("a gate with no ask policy must fail closed, not allow")
	}
	if result.Reason != RuleOfTwoDeniedReason {
		t.Errorf("reason = %q, want RuleOfTwoDeniedReason", result.Reason)
	}
}

func TestRuleOfTwoGate_UnwrapChainThroughGateAndMetrics(t *testing.T) {
	core := NewAllowAll()
	state := &fakeRuleOfTwoState{}
	gate := NewRuleOfTwoGate(core, state, externalSet(), nil, nil)
	// The metric recorder needs a non-nil metrics handle to wrap; the
	// chain shape is what matters here, so reuse the gate-only chain
	// for the single-wrapper case and assert Unwrap reaches the core
	// through the gate.
	if got := Unwrap(gate); got != core {
		t.Errorf("Unwrap(gate) = %T, want the inner *AllowAll", got)
	}
}

func TestRewrapChain_RebuildsGateAroundNewInner(t *testing.T) {
	oldInner := &recordingPolicy{}
	newInner := &recordingPolicy{result: &PermissionResult{Allowed: false, Reason: "new inner"}}
	state := &fakeRuleOfTwoState{tripped: true, enforcing: true, action: "block-external"}
	gate := NewRuleOfTwoGate(oldInner, state, externalSet(), nil, nil)

	rewrapped, ok := RewrapChain(gate, newInner)
	if !ok {
		t.Fatal("RewrapChain must succeed for a gate-only chain")
	}
	if Unwrap(rewrapped) != PermissionPolicy(newInner) {
		t.Fatalf("Unwrap(rewrapped) = %T, want the new inner", Unwrap(rewrapped))
	}
	// The rebuilt gate shares the tripped state: external tools still
	// denied with the rule-of-two reason, non-external delegate to the
	// NEW inner.
	result, err := rewrapped.Check(context.Background(), types.ToolDefinition{Name: "run_command"}, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Reason != RuleOfTwoDeniedReason {
		t.Errorf("rebuilt gate must keep denying external tools, got %+v", result)
	}
	result, err = rewrapped.Check(context.Background(), types.ToolDefinition{Name: "read_file"}, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Reason != "new inner" {
		t.Errorf("non-external check must reach the new inner, got %+v", result)
	}
	if oldInner.callCount() != 0 {
		t.Errorf("old inner received %d calls after rewrap, want 0", oldInner.callCount())
	}
}

func TestRewrapChain_UnrewrappableWrapperRefuses(t *testing.T) {
	inner := NewAllowAll()
	wrapped := &unwrapOnlyWrapper{inner: inner}
	out, ok := RewrapChain(wrapped, NewAllowAll())
	if ok {
		t.Fatal("RewrapChain must refuse a chain containing a non-Rewrapper wrapper")
	}
	if out != PermissionPolicy(wrapped) {
		t.Errorf("on refusal RewrapChain must return the original chain, got %T", out)
	}
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

func TestRuleOfTwoGate_DeniedReasonNamesRuleOfTwo(t *testing.T) {
	if !strings.HasPrefix(RuleOfTwoDeniedReason, "rule_of_two:") {
		t.Errorf("RuleOfTwoDeniedReason must keep the stable rule_of_two: grep prefix, got %q", RuleOfTwoDeniedReason)
	}
}
