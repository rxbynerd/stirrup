package core

import "github.com/rxbynerd/stirrup/harness/internal/executor"

// noteSandboxLoss reports whether the executor's sandbox has ended for
// good and, when it has, records the executor's description of its
// terminal state as the run's failure. No later tool call can work, so the
// caller ends the run with "sandbox_lost".
func (l *AgenticLoop) noteSandboxLoss() bool {
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

// sandboxLossSymptoms are the outcomes of a setup, verification or post-run
// step that fails because the sandbox it runs in has ended.
var sandboxLossSymptoms = map[string]bool{
	"error":              true,
	"setup_failed":       true,
	"verification_error": true,
	"hook_failed":        true,
	"handoff_failed":     true,
}

// attributeSandboxLoss replaces an outcome that a lost sandbox explains
// with "sandbox_lost". Cancellation, timeout and every outcome the loop
// decided on its own are kept.
func (l *AgenticLoop) attributeSandboxLoss(outcome string) string {
	if sandboxLossSymptoms[outcome] && l.noteSandboxLoss() {
		return "sandbox_lost"
	}
	return outcome
}
