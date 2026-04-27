package tool

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistry_RegisterAndResolve(t *testing.T) {
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
	if resolved == nil {
		t.Fatal("expected to resolve registered tool")
	}
	if resolved.Name != "test_tool" {
		t.Errorf("got name %q, want %q", resolved.Name, "test_tool")
	}
}

func TestRegistry_ResolveUnknown(t *testing.T) {
	r := NewRegistry()
	if r.Resolve("nonexistent") != nil {
		t.Error("expected nil for unregistered tool")
	}
}

func TestRegistry_List(t *testing.T) {
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
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	if defs[0].Name != "alpha" {
		t.Errorf("first tool: got %q, want %q", defs[0].Name, "alpha")
	}
	if defs[1].Name != "beta" {
		t.Errorf("second tool: got %q, want %q", defs[1].Name, "beta")
	}
}

func TestRegistry_ListPreservesOrder(t *testing.T) {
	r := NewRegistry()
	names := []string{"charlie", "alpha", "bravo"}
	for _, n := range names {
		r.Register(&Tool{
			Name:        n,
			Description: n,
			InputSchema: json.RawMessage(`{"type": "object"}`),
		})
	}

	defs := r.List()
	for i, def := range defs {
		if def.Name != names[i] {
			t.Errorf("position %d: got %q, want %q", i, def.Name, names[i])
		}
	}
}

func TestRegistry_ReplaceExisting(t *testing.T) {
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
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition after replacement, got %d", len(defs))
	}
	if defs[0].Description != "replaced" {
		t.Errorf("got description %q, want %q", defs[0].Description, "replaced")
	}
}

func TestTool_Definition(t *testing.T) {
	tool := &Tool{
		Name:              "my_tool",
		Description:       "My tool description",
		InputSchema:       json.RawMessage(`{"type": "object", "properties": {}}`),
		WorkspaceMutating: true,
		RequiresApproval:  true,
	}

	def := tool.Definition()
	if def.Name != "my_tool" {
		t.Errorf("name: got %q, want %q", def.Name, "my_tool")
	}
	if def.Description != "My tool description" {
		t.Errorf("description mismatch")
	}
	if string(def.InputSchema) != `{"type": "object", "properties": {}}` {
		t.Errorf("schema mismatch")
	}
}
