package router

import "context"

// DynamicRouterConfig controls the complexity-based model selection logic.
// The router picks between cheap, default, and expensive model selections
// based on turn signals like stop reason, turn number, and cumulative token usage.
type DynamicRouterConfig struct {
	DefaultSelection   ModelSelection
	CheapSelection     ModelSelection
	ExpensiveSelection ModelSelection

	// ExpensiveTurnThreshold: if Turn >= this value, use the expensive model.
	ExpensiveTurnThreshold int

	// ExpensiveTokenThreshold: if cumulative output tokens >= this value, use the expensive model.
	ExpensiveTokenThreshold int

	// CheapStopReasons: if LastStopReason is in this set, use the cheap model.
	CheapStopReasons []string
}

// DynamicRouter selects models based on turn complexity signals, routing
// simple turns to a cheaper model and complex turns to a more capable one.
type DynamicRouter struct {
	defaultSel   ModelSelection
	cheapSel     ModelSelection
	expensiveSel ModelSelection

	expensiveTurnThreshold  int
	expensiveTokenThreshold int
	cheapStopReasons        map[string]bool
}

// NewDynamicRouter creates a complexity-based router from the given config.
func NewDynamicRouter(cfg DynamicRouterConfig) *DynamicRouter {
	reasons := make(map[string]bool, len(cfg.CheapStopReasons))
	for _, r := range cfg.CheapStopReasons {
		reasons[r] = true
	}
	return &DynamicRouter{
		defaultSel:              cfg.DefaultSelection,
		cheapSel:                cfg.CheapSelection,
		expensiveSel:            cfg.ExpensiveSelection,
		expensiveTurnThreshold:  cfg.ExpensiveTurnThreshold,
		expensiveTokenThreshold: cfg.ExpensiveTokenThreshold,
		cheapStopReasons:        reasons,
	}
}

// Select picks a model based on the current turn's complexity signals, in
// priority order: cheap stop reason, then turn threshold, then token
// threshold, falling back to the default selection.
func (r *DynamicRouter) Select(_ context.Context, rc RouterContext) ModelSelection {
	if rc.LastStopReason != "" && r.cheapStopReasons[rc.LastStopReason] {
		return r.cheapSel
	}

	if r.expensiveTurnThreshold > 0 && rc.Turn >= r.expensiveTurnThreshold {
		return r.expensiveSel
	}

	if r.expensiveTokenThreshold > 0 && rc.TokenUsage.Output >= r.expensiveTokenThreshold {
		return r.expensiveSel
	}

	return r.defaultSel
}
