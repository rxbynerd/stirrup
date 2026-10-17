package core

import (
	"encoding/json"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// verificationTextMaxBytes bounds each string recorded from a verifier
// attempt (feedback, error, each Details string). A truncated value
// keeps this many bytes and gains types.CapFinalAssistantText's marker.
const verificationTextMaxBytes = 4 << 10

// verifierRecorded reports whether the run's verifier attempts belong on
// RunTrace.VerificationResults. The "none" verifier always passes, so
// recording it would make RunResult.verifierVerdict claim a check ran.
func verifierRecorded(cfg types.VerifierConfig) bool {
	return cfg.Type != "" && cfg.Type != "none"
}

// recordVerification hands r to the trace emitter when it records
// verifier attempts and the run's verifier is not "none".
func (l *AgenticLoop) recordVerification(config *types.RunConfig, r types.VerificationResult) {
	if !verifierRecorded(config.Verifier) {
		return
	}
	if recorder, ok := l.Trace.(trace.VerificationRecorder); ok {
		recorder.RecordVerification(r)
	}
}

// boundedVerificationResult returns the trace record of a verifier
// verdict: every string scrubbed of secret-shaped content, then capped.
func boundedVerificationResult(attempt int, vr *types.VerificationResult) types.VerificationResult {
	return types.VerificationResult{
		Attempt:  attempt,
		Passed:   vr.Passed,
		Feedback: boundVerificationText(vr.Feedback),
		Error:    boundVerificationText(vr.Error),
		Details:  boundVerificationDetails(vr.Details),
	}
}

// boundVerificationText scrubs s before capping it, so a secret that
// straddles the cut is still redacted.
func boundVerificationText(s string) string {
	capped, _ := types.CapFinalAssistantText(security.Scrub(s), verificationTextMaxBytes)
	return capped
}

func boundVerificationDetails(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		out[k] = boundVerificationValue(v)
	}
	return out
}

// boundVerificationValue bounds one Details value. Values outside the
// JSON-native shapes are normalised through a JSON round trip first so a
// struct or typed slice cannot carry an unscrubbed string into the trace.
func boundVerificationValue(v any) any {
	switch val := v.(type) {
	case nil, bool, int, int64, float64:
		return val
	case string:
		return boundVerificationText(val)
	case map[string]any:
		return boundVerificationDetails(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = boundVerificationValue(item)
		}
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("[unrecordable %T]", v)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Sprintf("[unrecordable %T]", v)
	}
	return boundVerificationValue(decoded)
}

// reportVerifierError surfaces a verifier that failed to produce a
// verdict on the log, the transport and the trace, so an operator
// without OpenTelemetry can tell a broken verifier from failed work.
func (l *AgenticLoop) reportVerifierError(config *types.RunConfig, attempt int, err error) {
	msg := boundVerificationText(err.Error())
	l.Logger.Warn("verifier error", "attempt", attempt, "error", msg)
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:    "warning",
		Message: fmt.Sprintf("verifier error (attempt %d): %s", attempt, msg),
	}); emitErr != nil {
		l.Logger.Warn("transport emit failed", "event", "warning", "error", emitErr)
	}
	l.recordVerification(config, types.VerificationResult{
		Attempt: attempt,
		Passed:  false,
		Error:   msg,
	})
}
