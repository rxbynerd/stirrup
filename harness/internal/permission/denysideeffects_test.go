package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

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
			must := require.New(t)
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			must.NoError(err)
			must.False(result.Allowed)
			must.NotEmpty(result.Reason)
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
			must := require.New(t)
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			must.NoError(err)
			must.True(result.Allowed, "reason: %q", result.Reason)
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
			must := require.New(t)
			tool := types.ToolDefinition{Name: name}
			result, err := policy.Check(context.Background(), tool, json.RawMessage(`{}`))
			must.NoError(err)
			must.True(result.Allowed, "reason: %q", result.Reason)
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
	must := require.New(t)
	inner := &countingPolicy{result: &PermissionResult{Allowed: true}}
	gate := NewSideEffectGate(map[string]bool{"write_file": true}, inner)

	result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, json.RawMessage(`{}`))
	must.NoError(err)
	must.False(result.Allowed, "write_file should be denied by the gate")
	must.Zero(inner.calls, "inner policy should not be consulted for a mutating tool")
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
			must := require.New(t)
			gate := NewSideEffectGate(map[string]bool{"write_file": true}, tc.inner)
			result, err := gate.Check(context.Background(), types.ToolDefinition{Name: "web_fetch"}, json.RawMessage(`{}`))
			must.Equal(1, tc.inner.calls)
			if tc.wantErr != nil {
				must.ErrorIs(err, tc.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tc.wantAllowed, result.Allowed)
		})
	}
}

// TestSideEffectGate_SurvivesRewrapChain pins that Unwrap sees through
// the gate and RewrapChain keeps it, with its denied set, in front of a
// replacement inner policy.
func TestSideEffectGate_SurvivesRewrapChain(t *testing.T) {
	must := require.New(t)
	original := &countingPolicy{result: &PermissionResult{Allowed: true}}
	gate := NewSideEffectGate(map[string]bool{"write_file": true}, original)
	must.Same(original, Unwrap(gate), "Unwrap should return the inner policy")

	replacement := &countingPolicy{result: &PermissionResult{Allowed: true}}
	rewrapped, ok := RewrapChain(gate, replacement)
	must.True(ok, "RewrapChain should accept a chain containing only the gate")
	must.Same(replacement, Unwrap(rewrapped), "rewrapped chain should end at the replacement policy")

	result, err := rewrapped.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, json.RawMessage(`{}`))
	must.NoError(err)
	must.False(result.Allowed, "rewrapped gate should keep its denied set")
	must.Zero(replacement.calls, "replacement should not be consulted for a mutating tool")
}
