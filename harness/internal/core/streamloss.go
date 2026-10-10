package core

import "fmt"

// ControlStreamLost ends the session because the control stream ended
// with cause. An active run is cancelled and ends with outcome
// "control_stream_lost" after its bounded post-run work; an idle loop
// starts no further run, so a follow-up window closes without changing
// the last run's outcome. Safe to call from any goroutine, at most
// once per loss.
func (l *AgenticLoop) ControlStreamLost(cause error) {
	l.ensureControlRouting()
	l.streamLost.Store(true)
	lost := ErrControlStreamLost
	if cause != nil {
		lost = fmt.Errorf("%w: %w", ErrControlStreamLost, cause)
	}
	l.cancelSession(lost, "control stream lost")
}

// logEmitFailure logs a failed transport emit. After the control stream
// is lost every emit fails, so only the first such failure is a warning
// and the rest are logged at debug level.
func (l *AgenticLoop) logEmitFailure(event string, err error) {
	if l.streamLost.Load() && !l.lossEmitLogged.CompareAndSwap(false, true) {
		l.Logger.Debug("transport emit failed", "event", event, "error", err)
		return
	}
	l.Logger.Warn("transport emit failed", "event", event, "error", err)
}
