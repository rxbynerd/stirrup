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
// about to send its first provider request, and opens the turn until
// emitTurnCommitted or emitTurnAbandoned closes it.
func (l *AgenticLoop) emitTurnStarted(turn int, model, providerType string) {
	l.openTurn = &turn
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:     "turn_started",
		Turn:     &turn,
		Model:    model,
		Provider: providerType,
	}); err != nil {
		l.logEmitFailure("turn_started", err)
	}
}

// committableAssistantBlocks returns the blocks of a turn ending with
// stopReason that may enter history, and whether any remain. Only tool_use
// turns are dispatched, so other stops drop their tool calls and empty
// text, which providers reject; a max_tokens turn needs text, as thinking
// cut off before its signature cannot replay.
func committableAssistantBlocks(stopReason string, blocks []types.ContentBlock) ([]types.ContentBlock, bool) {
	if stopReason == "tool_use" {
		return blocks, true
	}
	kept := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range withoutToolUse(blocks) {
		if b.Type != "text" || b.Text != "" {
			kept = append(kept, b)
		}
	}
	if stopReason == "max_tokens" {
		return kept, lastAssistantText(kept) != ""
	}
	return kept, len(kept) > 0
}

// emitTurnCommitted reports a turn whose assistant message, and a result
// for every tool call in it, are in the history.
func (l *AgenticLoop) emitTurnCommitted(turn int, stopReason string, usage types.TokenUsage, toolResults int) {
	l.openTurn = nil
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

// emitTurnAbandoned closes the open turn with reason, one of the
// types.TurnAbandoned* values. It is a no-op when no turn is open, so a
// deferred call only reports turns no other path closed.
func (l *AgenticLoop) emitTurnAbandoned(reason string) {
	if l.openTurn == nil {
		return
	}
	turn := *l.openTurn
	l.openTurn = nil
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:          "turn_abandoned",
		Turn:          &turn,
		AbandonReason: reason,
	}); err != nil {
		l.logEmitFailure("turn_abandoned", err)
	}
}
