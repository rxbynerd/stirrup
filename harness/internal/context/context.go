// Package context defines the ContextStrategy interface and implementations
// for managing conversation history within token budgets.
package context

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// TokenBudget describes the token limits for context preparation.
type TokenBudget struct {
	MaxTokens          int
	CurrentTokens      int
	ReserveForResponse int
}

// ContextStrategy prepares a message slice to fit within a token budget.
type ContextStrategy interface {
	Prepare(ctx context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error)
}
