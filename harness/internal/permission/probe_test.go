package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestPolicyEnginePolicy_Probe_OK(t *testing.T) {
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource
	);`
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewDenySideEffects(map[string]bool{"write_file": true}),
	})
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
}

func TestPolicyEnginePolicy_Probe_DoesNotTriggerFallback(t *testing.T) {
	// The synthetic probe tool matches no policy, so a Check() would
	// delegate to the fallback. Probe must NOT do that — it evaluates the
	// Cedar set directly. Use a fallback that records whether it was
	// consulted to pin the no-side-effect guarantee.
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource
	);`
	fb := &recordingFallback{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  fb,
	})
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if fb.called {
		t.Error("Probe consulted the fallback policy; it must evaluate Cedar directly to avoid a live ask-upstream prompt")
	}
}

func TestPolicyEnginePolicy_Probe_NilSet(t *testing.T) {
	var p *PolicyEnginePolicy
	if err := p.Probe(context.Background()); err == nil {
		t.Fatal("Probe on nil policy should error")
	}
}

// recordingFallback records whether Check was called, standing in for a
// fallback policy whose evaluation would be an observable side effect.
type recordingFallback struct{ called bool }

func (r *recordingFallback) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	r.called = true
	return &PermissionResult{Allowed: true}, nil
}
