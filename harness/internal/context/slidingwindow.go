package context

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// SlidingWindowStrategy drops the oldest exchanges when the token budget
// is exceeded. It always keeps the task (the first message) and the most
// recent complete exchange.
type SlidingWindowStrategy struct {
	lastCompaction *CompactionEvent
}

// NewSlidingWindowStrategy creates a new SlidingWindowStrategy.
func NewSlidingWindowStrategy() *SlidingWindowStrategy {
	return &SlidingWindowStrategy{}
}

// Prepare drops whole exchanges after the task, oldest first, until the
// estimated token count fits within FitLimit(budget). The task counts
// toward the budget but is never dropped. Token estimation uses a simple
// characters/4 approximation.
func (s *SlidingWindowStrategy) Prepare(_ context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	s.lastCompaction = nil

	available := FitLimit(budget)
	if available <= 0 {
		result, compacted := minimalHistory(messages)
		if compacted {
			s.lastCompaction = &CompactionEvent{
				Strategy:       "sliding-window",
				MessagesBefore: len(messages),
				MessagesAfter:  len(result),
				TokensBefore:   budget.CurrentTokens,
			}
		}
		return result, nil
	}

	if budget.CurrentTokens <= available {
		return messages, nil
	}

	last := lastExchangeStart(messages)
	total := budget.CurrentTokens
	candidate := 1
	for candidate < last && total > available {
		total -= estimateTokens(messages[candidate])
		candidate++
	}
	cut := pairSafeCut(messages, candidate)
	if cut <= 1 {
		return messages, nil
	}
	for i := candidate; i < cut; i++ {
		total -= estimateTokens(messages[i])
	}

	result := pinTask(messages, cut)
	s.lastCompaction = &CompactionEvent{
		Strategy:       "sliding-window",
		MessagesBefore: len(messages),
		MessagesAfter:  len(result),
		TokensBefore:   budget.CurrentTokens,
		TokensAfter:    total,
	}
	return result, nil
}

// LastCompaction returns details of the most recent compaction, or nil if
// no compaction was needed.
func (s *SlidingWindowStrategy) LastCompaction() *CompactionEvent {
	return s.lastCompaction
}

// estimateTokens returns a rough token estimate for a message: total
// characters across all content blocks and replay state divided by 4.
func estimateTokens(msg types.Message) int {
	chars := 0
	for _, block := range msg.Content {
		chars += len(block.Text)
		chars += len(block.Content)
		chars += len(block.Input)
	}
	// Replay state is resent with the message, so it counts too.
	for _, v := range msg.ReplayFields {
		chars += len(v)
	}
	est := chars / 4
	if est == 0 {
		est = 1 // every message costs at least 1 token
	}
	return est
}
