package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rubynerd/stirrup/types"
)

func TestAllowAll_AlwaysAllows(t *testing.T) {
	policy := NewAllowAll()
	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path": "/tmp/test"}`)

	result, err := policy.Check(context.Background(), tool, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("AllowAll should allow all calls")
	}
}

func TestDenySideEffects_DeniesKnownTools(t *testing.T) {
	sideEffecting := map[string]bool{
		"write_file":        true,
		"run_shell_command": true,
	}
	policy := NewDenySideEffects(sideEffecting)

	tool := types.ToolDefinition{Name: "write_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("should deny side-effecting tool")
	}
	if result.Reason != "side effects not permitted in this mode" {
		t.Errorf("unexpected reason: %q", result.Reason)
	}
}

func TestDenySideEffects_AllowsReadOnlyTools(t *testing.T) {
	sideEffecting := map[string]bool{
		"write_file": true,
	}
	policy := NewDenySideEffects(sideEffecting)

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("should allow non-side-effecting tool")
	}
}

func TestDenySideEffects_EmptySet(t *testing.T) {
	policy := NewDenySideEffects(map[string]bool{})

	tool := types.ToolDefinition{Name: "anything"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("should allow all tools when set is empty")
	}
}
