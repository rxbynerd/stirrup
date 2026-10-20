package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistry_RegisterAndResolve(t *testing.T) {
	must := require.New(t)
	r := NewRegistry()
	tool := &Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type": "object"}`),
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	r.Register(tool)

	resolved := r.Resolve("test_tool")
	must.NotNil(resolved, "registered tool should resolve")
	must.Equal("test_tool", resolved.Name)
}

func TestRegistry_ResolveUnknown_ReturnsNil(t *testing.T) {
	must := require.New(t)
	r := NewRegistry()
	must.Nil(r.Resolve("nonexistent"), "unregistered tool should not resolve")
}

func TestRegistry_List_ReturnsRegisteredDefinitions(t *testing.T) {
	must := require.New(t)
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "alpha",
		Description: "First",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	})
	r.Register(&Tool{
		Name:        "beta",
		Description: "Second",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	})

	defs := r.List()
	must.Len(defs, 2)
	must.Equal("alpha", defs[0].Name)
	must.Equal("beta", defs[1].Name)
}

func TestRegistry_List_PreservesRegistrationOrder(t *testing.T) {
	must := require.New(t)
	r := NewRegistry()
	names := []string{"charlie", "alpha", "bravo"}
	for _, n := range names {
		r.Register(&Tool{
			Name:        n,
			Description: n,
			InputSchema: json.RawMessage(`{"type": "object"}`),
		})
	}

	got := make([]string, 0, len(names))
	for _, def := range r.List() {
		got = append(got, def.Name)
	}
	must.Equal(names, got)
}

func TestRegistry_RegisterReplacesExistingName(t *testing.T) {
	must := require.New(t)
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "tool",
		Description: "original",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	})
	r.Register(&Tool{
		Name:        "tool",
		Description: "replaced",
		InputSchema: json.RawMessage(`{"type": "object"}`),
	})

	defs := r.List()
	must.Len(defs, 1)
	must.Equal("replaced", defs[0].Description)
}

func TestTool_Definition_CopiesNameDescriptionAndSchema(t *testing.T) {
	must := require.New(t)
	tool := &Tool{
		Name:              "my_tool",
		Description:       "My tool description",
		InputSchema:       json.RawMessage(`{"type": "object", "properties": {}}`),
		WorkspaceMutating: true,
		RequiresApproval:  true,
	}

	def := tool.Definition()
	must.Equal("my_tool", def.Name)
	must.Equal("My tool description", def.Description)
	must.JSONEq(`{"type": "object", "properties": {}}`, string(def.InputSchema))
}
