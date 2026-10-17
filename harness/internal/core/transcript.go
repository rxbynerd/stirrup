package core

import (
	"github.com/rxbynerd/stirrup/types"
)

// emitUserMessage reports text the loop just appended to the history as a
// user-role message. Merged appends report only the appended text, so a
// consumer sees each input once.
func (l *AgenticLoop) emitUserMessage(source, text, requestID string) {
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:      "user_message",
		Source:    source,
		Text:      text,
		RequestID: requestID,
	}); err != nil {
		l.logEmitFailure("user_message", err)
	}
}

// emitTurnStarted reports the routed model and provider type of a turn
// about to send its first provider request.
func (l *AgenticLoop) emitTurnStarted(turn int, model, providerType string) {
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:     "turn_started",
		Turn:     &turn,
		Model:    model,
		Provider: providerType,
	}); err != nil {
		l.logEmitFailure("turn_started", err)
	}
}

// committableAssistantBlocks returns the blocks of a turn that ended with
// stopReason that may enter history, and whether the turn leaves an
// assistant message at all. Only a tool_use turn is dispatched, so every
// other stop drops its tool calls. A max_tokens turn is kept only when
// it carries text: thinking alone may have been cut before its
// signature arrived, and providers reject an assistant message with no
// content.
func committableAssistantBlocks(stopReason string, blocks []types.ContentBlock) ([]types.ContentBlock, bool) {
	if stopReason == "tool_use" {
		return blocks, true
	}
	kept := withoutToolUse(blocks)
	if stopReason == "max_tokens" {
		return kept, lastAssistantText(kept) != ""
	}
	return kept, len(kept) > 0
}

// emitTurnCommitted reports a turn whose assistant message, and a result
// for every tool call in it, are in the history.
func (l *AgenticLoop) emitTurnCommitted(turn int, stopReason string, usage types.TokenUsage, toolResults int) {
	ev := types.HarnessEvent{
		Type:            "turn_committed",
		Turn:            &turn,
		StopReason:      stopReason,
		ToolResultCount: toolResults,
	}
	if usage != (types.TokenUsage{}) {
		ev.Usage = &usage
	}
	if err := l.Transport.Emit(ev); err != nil {
		l.logEmitFailure("turn_committed", err)
	}
}
