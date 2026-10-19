package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestControlPlaneTool_DefinitionAndDispatch(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
	ct := ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:             "search_memory",
		Description:      "Search long-term memory.",
		InputSchema:      schema,
		TimeoutSeconds:   15,
		RequiresApproval: true,
	})

	def := ct.Definition()
	require.Equal(t, "search_memory", def.Name, "definition = %+v", def)
	require.Equal(t, "Search long-term memory.", def.Description, "definition = %+v", def)
	require.Equal(t, string(schema), string(def.InputSchema), "input schema not carried verbatim")
	require.False(t, ct.WorkspaceMutating, "control-plane tools must never be WorkspaceMutating")
	require.True(t, ct.RequiresApproval, "RequiresApproval not propagated")
	require.Nil(t, ct.Handler, "control-plane tool must be async-only")
	require.Nil(t, ct.StructuredHandler, "control-plane tool must be async-only")
	require.NotNil(t, ct.AsyncHandler, "control-plane tool must be async-only")

	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{"query":"x"}`))
	require.NoError(t, err, "preflight")
	require.Equal(t, 15*time.Second, dispatch.Timeout)
}

func TestControlPlaneTool_ZeroTimeoutSelectsLoopDefault(t *testing.T) {
	ct := ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:        "save_memory",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err, "preflight")
	require.Zero(t, dispatch.Timeout, "zero timeout should select the loop default")
	require.False(t, ct.RequiresApproval, "RequiresApproval should default to false")
}
