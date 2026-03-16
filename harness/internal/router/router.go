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
