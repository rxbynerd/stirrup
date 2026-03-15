package context

import (
	"context"

	"github.com/rubynerd/stirrup/types"
)

// minPreservedMessages is the minimum number of recent messages to keep,
// ensuring turn structure is maintained even under heavy truncation.
const minPreservedMessages = 2

// SlidingWindowStrategy drops the oldest messages when the token budget is
// exceeded, always preserving at least the most recent 2 messages.
type SlidingWindowStrategy struct{}

// NewSlidingWindowStrategy creates a new SlidingWindowStrategy.
func NewSlidingWindowStrategy() *SlidingWindowStrategy {
	return &SlidingWindowStrategy{}
}

// Prepare truncates messages from the front of the slice until the estimated
// token count fits within (MaxTokens - ReserveForResponse). Token estimation
// uses a simple characters/4 approximation.
func (s *SlidingWindowStrategy) Prepare(_ context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	available := budget.MaxTokens - budget.ReserveForResponse
	if available <= 0 {
		// No room at all; return the minimum preserved messages.
		if len(messages) <= minPreservedMessages {
			return messages, nil
		}
		return messages[len(messages)-minPreservedMessages:], nil
	}

	if budget.CurrentTokens <= available {
		return messages, nil
	}

	// Calculate per-message token estimates so we can drop from the front.
	estimates := make([]int, len(messages))
	for i, msg := range messages {
		estimates[i] = estimateTokens(msg)
	}

	total := budget.CurrentTokens
	dropUntil := 0

	for dropUntil < len(messages)-minPreservedMessages && total > available {
		total -= estimates[dropUntil]
		dropUntil++
	}

	return messages[dropUntil:], nil
}

// estimateTokens returns a rough token estimate for a message: total
// characters across all content blocks divided by 4.
func estimateTokens(msg types.Message) int {
	chars := 0
	for _, block := range msg.Content {
		chars += len(block.Text)
		chars += len(block.Content)
		chars += len(block.Input)
	}
	est := chars / 4
	if est == 0 {
		est = 1 // every message costs at least 1 token
	}
	return est
}
