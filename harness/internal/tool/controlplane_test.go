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
	must := require.New(t)
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
	ct := ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:             "search_memory",
		Description:      "Search long-term memory.",
		InputSchema:      schema,
		TimeoutSeconds:   15,
		RequiresApproval: true,
	})

	def := ct.Definition()
	must.Equal("search_memory", def.Name, "definition = %+v", def)
	must.Equal("Search long-term memory.", def.Description, "definition = %+v", def)
	must.Equal(string(schema), string(def.InputSchema), "input schema not carried verbatim")
	must.False(ct.WorkspaceMutating, "control-plane tools must never be WorkspaceMutating")
	must.True(ct.RequiresApproval, "RequiresApproval not propagated")
	must.Nil(ct.Handler, "control-plane tool must be async-only")
	must.Nil(ct.StructuredHandler, "control-plane tool must be async-only")
	must.NotNil(ct.AsyncHandler, "control-plane tool must be async-only")

	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{"query":"x"}`))
	must.NoError(err, "preflight")
	must.Equal(15*time.Second, dispatch.Timeout)
}

func TestControlPlaneTool_ZeroTimeoutSelectsLoopDefault(t *testing.T) {
	must := require.New(t)
	ct := ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:        "save_memory",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{}`))
	must.NoError(err, "preflight")
	must.Zero(dispatch.Timeout, "zero timeout should select the loop default")
	must.False(ct.RequiresApproval, "RequiresApproval should default to false")
}
