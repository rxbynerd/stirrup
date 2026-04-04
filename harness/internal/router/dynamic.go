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
	// Later turns tend to require deeper reasoning.
	ExpensiveTurnThreshold int

	// ExpensiveTokenThreshold: if cumulative output tokens >= this value,
	// use the expensive model. High output suggests complex reasoning chains.
	ExpensiveTokenThreshold int

	// CheapStopReasons: if LastStopReason is in this set, use the cheap model.
	// For example, "tool_use" means the model just invoked tools and the next
	// turn only needs to process tool results — genuinely cheap work.
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

// Select picks a model based on the current turn's complexity signals.
//
// Priority order:
//  1. Cheap stop reason match → cheap model (tool result processing is genuinely cheap)
//  2. Turn >= expensive turn threshold → expensive model
//  3. Cumulative output tokens >= expensive token threshold → expensive model
//  4. Otherwise → default model
func (r *DynamicRouter) Select(_ context.Context, rc RouterContext) ModelSelection {
	// 1. Cheap stop reason takes priority: if the previous turn ended with
	// a tool call, the next turn just processes results.
	if rc.LastStopReason != "" && r.cheapStopReasons[rc.LastStopReason] {
		return r.cheapSel
	}

	// 2. Late turns are harder — the model may need to reason about
	// accumulated context and recover from earlier mistakes.
	if r.expensiveTurnThreshold > 0 && rc.Turn >= r.expensiveTurnThreshold {
		return r.expensiveSel
	}

	// 3. High cumulative output suggests the model is doing complex work.
	if r.expensiveTokenThreshold > 0 && rc.TokenUsage.Output >= r.expensiveTokenThreshold {
		return r.expensiveSel
	}

	return r.defaultSel
}
