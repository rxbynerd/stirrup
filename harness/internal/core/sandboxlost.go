package core

import "github.com/rxbynerd/stirrup/harness/internal/executor"

// sandboxLost reports whether the executor's sandbox has ended for good,
// recording the executor's description of its terminal state as the run's
// failure. No later tool call can work, so the caller ends the run.
func (l *AgenticLoop) sandboxLost() bool {
	reporter, ok := l.Executor.(executor.SandboxLossReporter)
	if !ok {
		return false
	}
	lost := reporter.SandboxLost()
	if lost == nil {
		return false
	}
	l.noteFailure("sandbox_lost", lost.Error())
	return true
}
