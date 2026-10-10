package core

import (
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// maxOutputLimitRecoveries bounds how many max_tokens stops one run
// continues past before the stop becomes the run outcome.
const maxOutputLimitRecoveries = 2

const (
	outputLimitContinueText = "Your previous response was cut off at the output limit. Continue from where you stopped."
	outputLimitToolCallText = "Your previous response was cut off at the output limit while writing a `%s` call, so no tool was run. Write large content in smaller pieces and re-issue the call."
	outputLimitNoTextText   = "Your previous response was cut off at the output limit before it produced any text. Keep your reasoning brief, then answer or make the next tool call."
)

// canRecoverFromOutputLimit reports whether a turn that ended with
// stopReason continues the run rather than ending it. A recovery needs
// a turn left to run in, so the last turn's max_tokens stop is terminal.
func (l *AgenticLoop) canRecoverFromOutputLimit(stopReason string, turn, maxTurns int) bool {
	return stopReason == "max_tokens" &&
		l.outputLimitRecoveries < maxOutputLimitRecoveries &&
		turn+1 < maxTurns
}

// withoutToolUse returns blocks minus every tool_use block. A turn cut
// off at the output limit is never dispatched, so none of its tool calls
// may reach history without a paired tool result.
func withoutToolUse(blocks []types.ContentBlock) []types.ContentBlock {
	kept := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "tool_use" {
			kept = append(kept, b)
		}
	}
	return kept
}

// recoverFromOutputLimit records a max_tokens turn in history without
// its tool calls and appends a synthetic instruction for the next turn.
// The assistant message is kept only when it carries text: a turn of
// thinking alone may have been cut before its signature arrived, and
// providers reject an assistant message with no content.
func (l *AgenticLoop) recoverFromOutputLimit(turn int, model string, outputLimit int, sr *streamResult, messages []types.Message) []types.Message {
	l.outputLimitRecoveries++

	kept := withoutToolUse(sr.Blocks)
	hasText := lastAssistantText(kept) != ""
	if hasText {
		messages = appendAssistantContent(messages, kept, sr.ReplayFields)
	}

	dropped := 0
	var lastToolName string
	for _, b := range sr.Blocks {
		if b.Type == "tool_use" {
			dropped++
			lastToolName = b.Name
		}
	}
	var instruction, action string
	switch {
	case dropped > 0:
		instruction = fmt.Sprintf(outputLimitToolCallText, lastToolName)
		action = fmt.Sprintf("dropped %d unrun tool call(s)", dropped)
	case hasText:
		instruction = outputLimitContinueText
		action = "asked the model to continue"
	default:
		instruction = outputLimitNoTextText
		action = "discarded a turn with no text"
	}
	messages = injectSyntheticUserText(messages, instruction)

	l.Logger.Warn("output limit reached; continuing run",
		"turn", turn,
		"model", model,
		"outputLimit", outputLimit,
		"droppedToolCalls", dropped,
		"recovery", l.outputLimitRecoveries,
		"maxRecoveries", maxOutputLimitRecoveries,
	)
	_ = l.Transport.Emit(types.HarnessEvent{
		Type: "warning",
		Message: fmt.Sprintf(
			"turn %d stopped at the output limit of %d tokens; %s and continued the run (output-limit recovery %d of %d)",
			turn, outputLimit, action, l.outputLimitRecoveries, maxOutputLimitRecoveries,
		),
	})
	return messages
}
