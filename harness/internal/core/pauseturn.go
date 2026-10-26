package core

import (
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// maxPauseContinuations bounds how many consecutive paused turns the loop
// continues before the pause becomes the run outcome.
const maxPauseContinuations = 5

// canContinuePausedTurn reports whether a turn that stopped on pause_turn,
// after continuations consecutive continuations, is continued rather than
// ending the run. A continuation needs a turn left to run in.
func canContinuePausedTurn(continuations, turn, maxTurns int) bool {
	return continuations < maxPauseContinuations && turn+1 < maxTurns
}

// pauseLimitReason names the limit that stops a paused turn, after
// continuations consecutive continuations, from being continued.
func pauseLimitReason(continuations int) string {
	if continuations < maxPauseContinuations {
		return "on the last turn the run allows"
	}
	return fmt.Sprintf("after %d consecutive continuations", continuations)
}

// reportUncontinuedPause logs and emits a warning for a paused turn the
// loop does not continue, which ends the run with outcome pause_turn.
func (l *AgenticLoop) reportUncontinuedPause(turn int, model string, continuations int, reason string) {
	l.Logger.Warn("paused turn not continued; ending run",
		"turn", turn,
		"model", model,
		"reason", reason,
		"continuations", continuations,
		"maxContinuations", maxPauseContinuations,
	)
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:    "warning",
		Message: fmt.Sprintf("turn %d: the provider paused a long-running server tool turn %s; ending the run with outcome pause_turn", turn, reason),
	})
}
