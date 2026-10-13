package types

// EvalOutcome describes the *quality* of a harness run, collapsing
// RunTrace.Outcome and RunTrace.VerificationResults onto
// passed / failed / inconclusive so eval aggregates and mining
// decisions can rest on a falsifiable signal. Computed on read at
// aggregation time; see docs/eval.md for the full derivation table.
type EvalOutcome string

const (
	// EvalPassed indicates the run completed successfully and any
	// verifier that ran agreed. Without a verifier, EvalPassed is
	// inherited from RunTrace.Outcome=="success" (v0.1 back-compat).
	EvalPassed EvalOutcome = "passed"

	// EvalFailed indicates the loop terminated in an error class
	// (`error`, `tool_failures`, `verification_failed`,
	// `structured_output_failed`) or ran to
	// `success` but a verifier disagreed.
	EvalFailed EvalOutcome = "failed"

	// EvalInconclusive indicates the run terminated without enough
	// signal to call it: limit-hit, non-result, or hook-infra
	// terminations.
	EvalInconclusive EvalOutcome = "inconclusive"
)

// EvalOutcomeFor maps a RunTrace's termination outcome and verifier
// verdict onto an EvalOutcome. See docs/eval.md for the full table.
func EvalOutcomeFor(t RunTrace) EvalOutcome {
	switch t.Outcome {
	case "success":
		if verifierFailed(t.VerificationResults) {
			return EvalFailed
		}
		return EvalPassed
	case "verification_failed", "error", "tool_failures", "structured_output_failed":
		return EvalFailed
	case "max_turns", "budget_exceeded", "timeout", "max_tokens", "stalled",
		"cancelled", "verification_error", "setup_failed", "hook_failed",
		"handoff_failed", "control_stream_lost", "context_exhausted":
		return EvalInconclusive
	default:
		// Conservative default: an unknown/future outcome must not be
		// treated as a quality pass.
		return EvalInconclusive
	}
}

// verifierFailed reports whether any verifier in the result set
// returned Passed=false. A nil or empty slice returns false.
func verifierFailed(results []VerificationResult) bool {
	for _, r := range results {
		if !r.Passed {
			return true
		}
	}
	return false
}

// RunHealth describes how the harness run itself finished, independent of
// any judge verdict or verifier opinion about the work.
type RunHealth string

const (
	// RunHealthy means the harness exited zero and the trace outcome is
	// "success".
	RunHealthy RunHealth = "healthy"

	// RunDegraded means the run stopped on a limit, an interruption, or an
	// outcome this version does not recognise.
	RunDegraded RunHealth = "degraded"

	// RunFailed means the harness exited non-zero, left no trace, or ended
	// in an error class.
	RunFailed RunHealth = "failed"
)

// RunHealthFor classifies a run from its trace and whether the harness
// process exited non-zero. A non-zero exit is the operator-visible
// contract, so it forces RunFailed even when the trace says "success".
// A nil trace is RunFailed. A verifier that disagreed with a "success"
// trace does not change health; see LastVerification.
func RunHealthFor(t *RunTrace, harnessFailed bool) RunHealth {
	if t == nil || harnessFailed {
		return RunFailed
	}
	switch t.Outcome {
	case "success":
		return RunHealthy
	case "error", "tool_failures", "verification_failed", "verification_error",
		"setup_failed", "hook_failed", "structured_output_failed":
		return RunFailed
	default:
		// Limit hits, interruptions, and unknown or future outcomes.
		return RunDegraded
	}
}

// VerificationStatus is the verifier verdict of a run: the last
// VerificationResult recorded in its trace.
type VerificationStatus string

const (
	// VerificationNone means no verifier result was recorded.
	VerificationNone VerificationStatus = "none"

	// VerificationPassed means the last verifier result passed.
	VerificationPassed VerificationStatus = "passed"

	// VerificationFailed means the last verifier result failed.
	VerificationFailed VerificationStatus = "failed"
)

// LastVerification reports the verdict of the final result in the trace.
// A run that failed verification and then passed on a retry is
// VerificationPassed, whereas EvalOutcomeFor treats any failed result as a
// failed run.
func LastVerification(t *RunTrace) VerificationStatus {
	if t == nil || len(t.VerificationResults) == 0 {
		return VerificationNone
	}
	if t.VerificationResults[len(t.VerificationResults)-1].Passed {
		return VerificationPassed
	}
	return VerificationFailed
}
