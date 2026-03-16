package tool

import "github.com/rxbynerd/stirrup/types"

// Registry is a concrete implementation of ToolRegistry backed by a map.
type Registry struct {
	tools map[string]*Tool
	order []string // preserves registration order for List()
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]*Tool),
	}
}

// Register adds a tool to the registry. If a tool with the same name already
// exists, it is replaced.
func (r *Registry) Register(t *Tool) {
	if _, exists := r.tools[t.Name]; !exists {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// List returns ToolDefinitions for all registered tools in registration order.
func (r *Registry) List() []types.ToolDefinition {
	defs := make([]types.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// Resolve looks up a tool by name. Returns nil if not found.
func (r *Registry) Resolve(name string) *Tool {
	return r.tools[name]
}
