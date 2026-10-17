package core

import (
	"github.com/rxbynerd/stirrup/harness/internal/router"
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

// emitTurnStarted reports the routed provider and model of a turn about to
// send its first provider request.
func (l *AgenticLoop) emitTurnStarted(turn int, selection router.ModelSelection) {
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:     "turn_started",
		Turn:     &turn,
		Model:    selection.Model,
		Provider: selection.Provider,
	}); err != nil {
		l.logEmitFailure("turn_started", err)
	}
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
