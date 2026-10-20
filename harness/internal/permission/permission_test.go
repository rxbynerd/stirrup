package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestAllowAll_AlwaysAllows(t *testing.T) {
	must := require.New(t)
	policy := NewAllowAll()
	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path": "/tmp/test"}`)

	result, err := policy.Check(context.Background(), tool, input)
	must.NoError(err)
	must.True(result.Allowed)
}

func TestDenySideEffects_DeniesKnownTools(t *testing.T) {
	must := require.New(t)
	mutating := map[string]bool{
		"write_file":        true,
		"run_shell_command": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "write_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	must.False(result.Allowed)
	must.NotEmpty(result.Reason, "denial reason")
}

func TestDenySideEffects_AllowsReadOnlyTools(t *testing.T) {
	must := require.New(t)
	mutating := map[string]bool{
		"write_file": true,
	}
	policy := NewDenySideEffects(mutating)

	tool := types.ToolDefinition{Name: "read_file"}
	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	must.True(result.Allowed)
}

func TestDenySideEffects_EmptySetAllowsAllTools(t *testing.T) {
	must := require.New(t)
	policy := NewDenySideEffects(map[string]bool{})

	tool := types.ToolDefinition{Name: "anything"}
	result, err := policy.Check(context.Background(), tool, nil)
	must.NoError(err)
	must.True(result.Allowed)
}
