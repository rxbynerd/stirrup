package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
	if def.Name != "search_memory" || def.Description != "Search long-term memory." {
		t.Fatalf("definition = %+v", def)
	}
	if string(def.InputSchema) != string(schema) {
		t.Fatalf("input schema not carried verbatim: %s", def.InputSchema)
	}
	if ct.WorkspaceMutating {
		t.Fatal("control-plane tools must never be WorkspaceMutating")
	}
	if !ct.RequiresApproval {
		t.Fatal("RequiresApproval not propagated")
	}
	if ct.Handler != nil || ct.StructuredHandler != nil || ct.AsyncHandler == nil {
		t.Fatal("control-plane tool must be async-only")
	}

	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if dispatch.Timeout != 15*time.Second {
		t.Fatalf("timeout = %v, want 15s", dispatch.Timeout)
	}
}

func TestControlPlaneTool_ZeroTimeoutSelectsLoopDefault(t *testing.T) {
	ct := ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:        "save_memory",
		Description: "d",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	dispatch, err := ct.AsyncHandler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if dispatch.Timeout != 0 {
		t.Fatalf("timeout = %v, want 0 (loop default)", dispatch.Timeout)
	}
	if ct.RequiresApproval {
		t.Fatal("RequiresApproval should default to false")
	}
}
