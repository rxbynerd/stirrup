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

// CompactionEvent records what happened during context compaction.
// Nil means no compaction was needed (messages fit within budget).
type CompactionEvent struct {
	Strategy       string // "sliding-window", "summarise", "offload-to-file"
	MessagesBefore int
	MessagesAfter  int
	TokensBefore   int
	TokensAfter    int
}

// ContextStrategy prepares a message slice to fit within a token budget.
//
// Concurrency contract: implementations must update LastCompaction()
// before Prepare returns. The metric-recording wrapper (and any other
// caller pairing Prepare with LastCompaction) reads the value on the
// same goroutine immediately after Prepare returns, so deferring the
// update to a background goroutine would race the read. Setting
// LastCompaction inline at the end of Prepare keeps the pairing
// race-free without locking.
type ContextStrategy interface {
	Prepare(ctx context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error)

	// LastCompaction returns details of the most recent compaction performed
	// by Prepare, or nil if no compaction was needed. Implementations must
	// have set this value before the most recent Prepare call returns.
	LastCompaction() *CompactionEvent
}
