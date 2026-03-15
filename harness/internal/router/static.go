package router

import "context"

// StaticRouter always returns the same provider and model regardless of context.
type StaticRouter struct {
	provider string
	model    string
}

// NewStaticRouter creates a router that always selects the given provider and model.
func NewStaticRouter(provider, model string) *StaticRouter {
	return &StaticRouter{provider: provider, model: model}
}

// Select returns the fixed provider and model.
func (r *StaticRouter) Select(_ context.Context, _ RouterContext) ModelSelection {
	return ModelSelection{
		Provider: r.provider,
		Model:    r.model,
	}
}
