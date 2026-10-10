package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestDenySideEffects_DeniesMutatingTools confirms that the policy
// rejects exactly the tools the factory marks as workspace-mutating
// (write_file, run_command, edit_file).
func TestDenySideEffects_DeniesMutatingTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file":  true,
		"run_command": true,
		"edit_file":   true,
	}
	policy := NewDenySideEffects(mutating)

	for _, name := range []string{"write_file", "run_command", "edit_file"} {
		t.Run(name, func(t *testing.T) {
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Allowed {
				t.Fatalf("expected %s to be denied", name)
			}
			if result.Reason == "" {
				t.Errorf("expected non-empty reason on denial of %s", name)
			}
		})
	}
}

// TestDenySideEffects_AllowsApprovalRequiredButNonMutating: web_fetch
// and spawn_agent are read-only as far as the workspace is concerned,
// so deny-side-effects must let them through. They are still gated by
// ask-upstream when configured.
func TestDenySideEffects_AllowsApprovalRequiredButNonMutating(t *testing.T) {
	mutating := map[string]bool{
		"write_file":  true,
		"run_command": true,
	}
	policy := NewDenySideEffects(mutating)

	for _, name := range []string{"web_fetch", "spawn_agent"} {
		t.Run(name, func(t *testing.T) {
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.Allowed {
				t.Fatalf("expected %s to be allowed (it is approval-required but does not mutate the workspace), got reason: %q", name, result.Reason)
			}
		})
	}
}

// TestDenySideEffects_AllowsPureReadTools confirms the policy never
// denies read_file, list_directory, grep_files, or find_files.
func TestDenySideEffects_AllowsPureReadTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file":  true,
		"run_command": true,
	}
	policy := NewDenySideEffects(mutating)

	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files"} {
		t.Run(name, func(t *testing.T) {
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.Allowed {
				t.Fatalf("expected %s to be allowed, got reason: %q", name, result.Reason)
			}
		})
	}
}

// countingPolicy returns a fixed decision and counts how often it was
// consulted.
type countingPolicy struct {
	result *PermissionResult
	err    error
	calls  int
}

func (c *countingPolicy) Check(context.Context, types.ToolDefinition, json.RawMessage) (*PermissionResult, error) {
	c.calls++
	return c.result, c.err
}

// TestSideEffectGate_DeniesMutatingWithoutConsultingInner pins that a
// mutating call is denied by the gate alone, so an inner ask-upstream
// policy is never asked to approve it.
func TestSideEffectGate_DeniesMutatingWithoutConsultingInner(t *testing.T) {
	inner := &countingPolicy{result: &PermissionResult{Allowed: true}}
	gate := NewSideEffectGate(map[string]bool{"write_file": true}, inner)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Fatal("write_file should be denied by the gate")
	}
	if inner.calls != 0 {
		t.Errorf("inner policy consulted %d times for a mutating tool, want 0", inner.calls)
	}
}

// TestSideEffectGate_DelegatesNonMutating pins that the inner policy
// decides every call the gate lets through, including denials and
// errors.
func TestSideEffectGate_DelegatesNonMutating(t *testing.T) {
	innerErr := errors.New("inner failed")
	cases := []struct {
		name        string
		inner       *countingPolicy
		wantAllowed bool
		wantErr     error
	}{
		{name: "inner allows", inner: &countingPolicy{result: &PermissionResult{Allowed: true}}, wantAllowed: true},
		{name: "inner denies", inner: &countingPolicy{result: &PermissionResult{Allowed: false, Reason: "no"}}},
		{name: "inner errors", inner: &countingPolicy{err: innerErr}, wantErr: innerErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewSideEffectGate(map[string]bool{"write_file": true}, tc.inner)
			result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "web_fetch"}, json.RawMessage(`{}`))
			if tc.inner.calls != 1 {
				t.Fatalf("inner policy consulted %d times, want 1", tc.inner.calls)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if result.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tc.wantAllowed)
			}
		})
	}
}

// TestSideEffectGate_SurvivesRewrapChain pins that Unwrap sees through
// the gate and RewrapChain keeps it, with its denied set, in front of a
// replacement inner policy.
func TestSideEffectGate_SurvivesRewrapChain(t *testing.T) {
	original := &countingPolicy{result: &PermissionResult{Allowed: true}}
	gate := NewSideEffectGate(map[string]bool{"write_file": true}, original)
	if Unwrap(gate) != PermissionPolicy(original) {
		t.Fatalf("Unwrap(gate) = %T, want the inner policy", Unwrap(gate))
	}

	replacement := &countingPolicy{result: &PermissionResult{Allowed: true}}
	rewrapped, ok := RewrapChain(gate, replacement)
	if !ok {
		t.Fatal("RewrapChain refused a chain containing only the gate")
	}
	if Unwrap(rewrapped) != PermissionPolicy(replacement) {
		t.Fatalf("rewrapped chain does not end at the replacement policy")
	}

	result, err := rewrapped.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Error("rewrapped gate lost its denied set: write_file allowed")
	}
	if replacement.calls != 0 {
		t.Errorf("replacement consulted %d times for a mutating tool, want 0", replacement.calls)
	}
}
