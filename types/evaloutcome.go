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
	// (`error`, `tool_failures`, `verification_failed`) or ran to
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
	case "verification_failed", "error", "tool_failures":
		return EvalFailed
	case "max_turns", "budget_exceeded", "timeout", "max_tokens", "stalled",
		"cancelled", "verification_error", "setup_failed", "hook_failed":
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
