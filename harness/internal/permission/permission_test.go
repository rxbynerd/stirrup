package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
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
	mutating := map[string]bool{
		"write_file":        true,
		"run_shell_command": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "write_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("should deny workspace-mutating tool")
	}
	if result.Reason == "" {
		t.Error("expected non-empty denial reason")
	}
}

func TestDenySideEffects_AllowsReadOnlyTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("should allow non-mutating tool")
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
