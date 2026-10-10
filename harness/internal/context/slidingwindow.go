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

const (
	charsPerToken         = 4
	messageOverheadTokens = 4
	blockOverheadTokens   = 3
)

// EstimateHistoryTokens returns the estimated size, in tokens, of messages
// as sent to a provider. The agentic loop builds TokenBudget.CurrentTokens
// from it, so a strategy that subtracts its own estimate of the history
// from CurrentTokens is left with the system prompt and tool definitions.
func EstimateHistoryTokens(messages []types.Message) int {
	return estimateHistory(messages)
}

// estimateTokens returns a token estimate for a message at charsPerToken
// characters per token, counting each field separately, plus the
// structural overhead of the message and each of its blocks.
func estimateTokens(msg types.Message) int {
	total := messageOverheadTokens
	for _, block := range msg.Content {
		total += estimateBlockTokens(block)
	}
	// Replay state is resent with the message, so it counts too.
	replay := 0
	for _, v := range msg.ReplayFields {
		replay += len(v)
	}
	return total + replay/charsPerToken
}

// estimateBlockTokens returns the estimated tokens of one content block,
// including the wire view of its structured envelope, which is sent
// alongside a tool result's text. A thinking block's signature is not
// counted.
func estimateBlockTokens(block types.ContentBlock) int {
	total := blockOverheadTokens
	for _, field := range []int{
		len(block.Text), len(block.Content), len(block.Input), len(block.ID),
		len(block.Name), len(block.ToolUseID), len(types.ToolResultWireEnvelope(block)),
	} {
		total += field / charsPerToken
	}
	return total
}
