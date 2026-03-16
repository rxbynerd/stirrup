package router

import "context"

// PerModeRouter maps each run mode to a specific provider+model pair, falling
// back to a default selection for modes not explicitly configured.
type PerModeRouter struct {
	defaultSelection ModelSelection
	modeMap          map[string]ModelSelection
}

// NewPerModeRouter creates a router that selects a provider and model based on
// the run mode. Modes not present in modeMap use defaultSelection.
func NewPerModeRouter(defaultSelection ModelSelection, modeMap map[string]ModelSelection) *PerModeRouter {
	// Copy the map to avoid aliasing the caller's slice.
	m := make(map[string]ModelSelection, len(modeMap))
	for k, v := range modeMap {
		m[k] = v
	}
	return &PerModeRouter{
		defaultSelection: defaultSelection,
		modeMap:          m,
	}
}

// Select returns the model selection for the given mode, or the default if the
// mode is not explicitly configured.
func (r *PerModeRouter) Select(_ context.Context, rc RouterContext) ModelSelection {
	if sel, ok := r.modeMap[rc.Mode]; ok {
		return sel
	}
	return r.defaultSelection
}
