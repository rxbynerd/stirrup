package context

import "github.com/rxbynerd/stirrup/types"

// A compacted history is the task (messages[0]) followed by whole
// exchanges: an assistant message and the user message answering its tool
// calls. Cutting only at an assistant message keeps every tool_result next
// to its tool_use and keeps roles alternating, as every provider requires.

// lastExchangeStart returns the index of the last assistant message after
// the task, or 0 when the history has no exchange after the task.
func lastExchangeStart(messages []types.Message) int {
	for i := len(messages) - 1; i >= 1; i-- {
		if messages[i].Role == "assistant" {
			return i
		}
	}
	return 0
}

// pairSafeCut returns the smallest exchange start at or after candidate,
// never past the last exchange, so that the most recent complete exchange
// always survives. It returns 0 when there is no exchange to cut at.
func pairSafeCut(messages []types.Message, candidate int) int {
	last := lastExchangeStart(messages)
	if last == 0 {
		return 0
	}
	for i := max(candidate, 1); i < last; i++ {
		if messages[i].Role == "assistant" {
			return i
		}
	}
	return last
}

// pinTask returns the task followed by messages[cut:] in a new slice.
// cut must be a value returned by pairSafeCut greater than 0.
func pinTask(messages []types.Message, cut int) []types.Message {
	result := make([]types.Message, 0, 1+len(messages)-cut)
	result = append(result, messages[0])
	return append(result, messages[cut:]...)
}

// minimalHistory returns the task and the last complete exchange, the
// smallest history a strategy sends when the budget leaves no room. The
// second result is false when nothing could be dropped.
func minimalHistory(messages []types.Message) ([]types.Message, bool) {
	cut := lastExchangeStart(messages)
	if cut <= 1 {
		return messages, false
	}
	return pinTask(messages, cut), true
}
