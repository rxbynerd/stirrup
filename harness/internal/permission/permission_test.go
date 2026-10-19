package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestAllowAll_AlwaysAllows(t *testing.T) {
	policy := NewAllowAll()
	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path": "/tmp/test"}`)

	result, err := policy.Check(context.Background(), tool, input)
	require.NoError(t, err)
	require.True(t, result.Allowed)
}

func TestDenySideEffects_DeniesKnownTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file":        true,
		"run_shell_command": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "write_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.NotEmpty(t, result.Reason, "denial reason")
}

func TestDenySideEffects_AllowsReadOnlyTools(t *testing.T) {
	mutating := map[string]bool{
		"write_file": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed)
}

func TestDenySideEffects_EmptySetAllowsAllTools(t *testing.T) {
	policy := NewDenySideEffects(map[string]bool{})

	tool := types.ToolDefinition{Name: "anything"}
	result, err := policy.Check(context.Background(), tool, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed)
}
