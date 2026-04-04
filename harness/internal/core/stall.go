package core

import "encoding/json"

const (
	// maxRepeatedToolCalls is the number of identical consecutive tool calls
	// (same name + same input) before the loop is considered stalled.
	maxRepeatedToolCalls = 3

	// maxConsecutiveFailures is the number of consecutive tool call failures
	// before the loop is terminated with a tool_failures outcome.
	maxConsecutiveFailures = 5
)

// stallDetector tracks repeated tool calls and consecutive failures to detect
// when the agentic loop is making no progress.
type stallDetector struct {
	lastToolCall     string // "name:input" key of the most recent call
	repeatCount      int
	consecutiveFails int
}

// recordToolCall records a tool call and returns a non-empty outcome string
// if a stall condition is detected: "stalled" for repeated identical calls,
// "tool_failures" for too many consecutive failures.
func (s *stallDetector) recordToolCall(name string, input json.RawMessage, success bool) string {
	key := name + ":" + string(input)

	if key == s.lastToolCall {
		s.repeatCount++
	} else {
		s.lastToolCall = key
		s.repeatCount = 1
	}

	if !success {
		s.consecutiveFails++
	} else {
		s.consecutiveFails = 0
	}

	if s.repeatCount >= maxRepeatedToolCalls {
		return "stalled"
	}
	if s.consecutiveFails >= maxConsecutiveFailures {
		return "tool_failures"
	}
	return ""
}
