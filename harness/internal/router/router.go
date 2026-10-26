// Package router defines the ModelRouter interface and implementations for
// selecting which provider and model to use on each turn.
package router

import "context"

// RouterContext provides the information a router needs to make a selection.
type RouterContext struct {
	Mode           string
	Turn           int
	LastStopReason string
	TokenUsage     TokenUsage
}

// TokenUsage mirrors types.TokenUsage to avoid coupling the router interface
// to the types module. Callers convert at the boundary.
type TokenUsage struct {
	Input  int
	Output int
}

// ModelSelection is the provider and model chosen for the current turn.
type ModelSelection struct {
	Provider string
	Model    string
}

// ModelRouter selects a provider and model for each agentic loop turn.
type ModelRouter interface {
	Select(ctx context.Context, rc RouterContext) ModelSelection
}

// selectionLister is implemented by a router whose selection varies with
// the mode or with turn signals, so every selection it can make is
// knowable up front.
type selectionLister interface {
	Selections() []ModelSelection
}

// Candidates returns the distinct selections r can make for a run in mode:
// the selection for the mode, then any others the router can make, such as
// another mode's or one a turn signal switches to.
func Candidates(ctx context.Context, r ModelRouter, mode string) []ModelSelection {
	all := []ModelSelection{r.Select(ctx, RouterContext{Mode: mode})}
	if lister, ok := r.(selectionLister); ok {
		all = append(all, lister.Selections()...)
	}
	seen := make(map[ModelSelection]bool, len(all))
	distinct := all[:0]
	for _, sel := range all {
		if !seen[sel] {
			seen[sel] = true
			distinct = append(distinct, sel)
		}
	}
	return distinct
}
