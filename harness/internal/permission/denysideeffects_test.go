package permission

import (
	"context"
	"encoding/json"
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

// TestDenySideEffects_AllowsApprovalRequiredButNonMutating is the WP1
// regression: web_fetch and spawn_agent are read-only as far as the
// workspace is concerned, so deny-side-effects must let them through.
// They will still be gated by ask-upstream when configured.
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
// denies read_file, list_directory or search_files.
func TestDenySideEffects_AllowsPureReadTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file":  true,
		"run_command": true,
	}
	policy := NewDenySideEffects(mutating)

	for _, name := range []string{"read_file", "list_directory", "search_files"} {
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
