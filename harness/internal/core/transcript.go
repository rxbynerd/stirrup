package core

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/rxbynerd/stirrup/types"
)

// setupWarningHold buffers warnings while a run is in setup, so each
// arrives after the run_started that attributes it to its run.
type setupWarningHold struct {
	mu      sync.Mutex
	holding bool
	pending []types.HarnessEvent
}

// holdSetupWarnings starts buffering warnings for a run entering setup.
func (l *AgenticLoop) holdSetupWarnings() {
	l.setupWarnings.mu.Lock()
	defer l.setupWarnings.mu.Unlock()
	l.setupWarnings.holding = true
	l.setupWarnings.pending = nil
}

// releaseSetupWarnings emits the buffered warnings in order and stops
// buffering. It is called once run_started is sent, or before the error
// of a run that failed setup and so sends no run_started.
func (l *AgenticLoop) releaseSetupWarnings() {
	l.setupWarnings.mu.Lock()
	pending := l.setupWarnings.pending
	l.setupWarnings.holding = false
	l.setupWarnings.pending = nil
	l.setupWarnings.mu.Unlock()
	for _, ev := range pending {
		l.emitWarning(ev)
	}
}

// emitRunWarning sends a "warning" raised by the run, buffered while the
// run is in setup.
func (l *AgenticLoop) emitRunWarning(ev types.HarnessEvent) {
	ev.Type = "warning"
	l.setupWarnings.mu.Lock()
	if l.setupWarnings.holding {
		l.setupWarnings.pending = append(l.setupWarnings.pending, ev)
		l.setupWarnings.mu.Unlock()
		return
	}
	l.setupWarnings.mu.Unlock()
	l.emitWarning(ev)
}

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
// cut off before its signature cannot replay. A tool_use stop without tool
// calls, or a response without a stop reason, is never kept: the run errors
// on it, and a follow-up continuing the history must not replay it.
func committableAssistantBlocks(stopReason string, blocks []types.ContentBlock) ([]types.ContentBlock, bool) {
	switch stopReason {
	case "tool_use":
		return blocks, len(withoutToolUse(blocks)) < len(blocks)
	case "":
		return nil, false
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

// emitToolsLoaded reports deferred tools a search loaded, by internal ID.
// source is one of the ToolsLoadedSource* values, search names the search
// tool and input is its input; turn is the open turn, omitted when none
// is open.
func (l *AgenticLoop) emitToolsLoaded(source, search string, input json.RawMessage, tools []string) {
	ev := types.HarnessEvent{
		Type:   "tools_loaded",
		Source: source,
		Name:   search,
		Input:  bytes.Clone(input),
		Tools:  tools,
	}
	if l.openTurn != nil {
		turn := *l.openTurn
		ev.Turn = &turn
	}
	if err := l.Transport.Emit(ev); err != nil {
		l.logEmitFailure("tools_loaded", err)
	}
}
