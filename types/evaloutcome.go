package types

// EvalOutcome describes the *quality* of a harness run, derived
// consumer-side from RunTrace.Outcome (the loop's stop reason) and
// RunTrace.VerificationResults (the verifier's verdict).
//
// RunTrace.Outcome tells you *why the loop stopped* — `success`,
// `error`, `max_turns`, `verification_failed`, and so on. By itself it
// conflates two very different states in execution mode: "the harness
// made the correct change" vs. "the loop exited cleanly with zero
// useful changes." Every metric derived from `Outcome == "success"`
// therefore lies about quality.
//
// EvalOutcome collapses the (Outcome, VerificationResults) pair onto
// passed / failed / inconclusive so eval aggregates and mining
// decisions can rest on a falsifiable signal. The derivation rules
// live in EvalOutcomeFor.
//
// The type is consumer-side: traces continue to store the loop's
// Outcome verbatim; EvalOutcome is computed on read at aggregation
// time. No proto change, no wire shape mutation, no historical
// trace re-write. Backward compatibility for existing baselines is
// preserved by trusting Outcome=="success" without a verifier as
// passed; the trade-off and a future `evalOutcomeQuality` label
// are tracked in #273.
type EvalOutcome string

const (
	// EvalPassed indicates the run completed successfully and any
	// verifier that ran agreed. Without a verifier, EvalPassed is
	// inherited from RunTrace.Outcome=="success" verbatim per the
	// v0.1 backward-compatibility decision (#273).
	EvalPassed EvalOutcome = "passed"

	// EvalFailed indicates the run produced a wrong-direction result.
	// Either the loop terminated in an error class (`error`,
	// `tool_failures`, `verification_failed`) or it ran to `success`
	// but a verifier disagreed.
	EvalFailed EvalOutcome = "failed"

	// EvalInconclusive indicates the run terminated without enough
	// signal to call it. Limit-hit terminations (`max_turns`,
	// `budget_exceeded`, `timeout`, `max_tokens`, `stalled`),
	// non-result terminations (`cancelled`, `verification_error`), and
	// lifecycle-hook infra failures (`setup_failed`, `hook_failed`,
	// issue #461) all produce inconclusive: the harness ran out of
	// room, was interrupted, or never got a fair shot at the task, so
	// the run is neither a quality win nor a quality loss.
	EvalInconclusive EvalOutcome = "inconclusive"
)

// EvalOutcomeFor maps a RunTrace's termination outcome and verifier
// verdict onto an EvalOutcome. The full table:
//
//	Outcome              | Verifier ran? | Verdict   | EvalOutcome
//	---------------------|---------------|-----------|---------------
//	success              | yes           | all pass  | passed
//	success              | yes           | any fail  | failed
//	success              | no            | n/a       | passed (compat)
//	verification_failed  | yes (impl.)   | failed    | failed
//	error                | any           | any       | failed
//	tool_failures        | any           | any       | failed
//	max_turns            | any           | any       | inconclusive
//	budget_exceeded      | any           | any       | inconclusive
//	timeout              | any           | any       | inconclusive
//	max_tokens           | any           | any       | inconclusive
//	stalled              | any           | any       | inconclusive
//	cancelled            | any           | any       | inconclusive
//	verification_error   | any           | any       | inconclusive
//	setup_failed         | any           | any       | inconclusive
//	hook_failed          | any           | any       | inconclusive
//	(empty / unknown)    | any           | any       | inconclusive
//
// The success-without-verifier branch is the load-bearing call here.
// V0.1 trusts it as passed to preserve existing baselines; the
// stricter "demote to inconclusive when no verifier ran" alternative
// is tracked in #273 as a follow-up behind an opt-in
// `evalOutcomeQuality` label.
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
		// Unknown / empty outcomes default to inconclusive so a future
		// outcome added upstream cannot silently land in the wrong
		// bucket. The default is conservative: an unknown outcome must
		// not be treated as a quality pass.
		return EvalInconclusive
	}
}

// verifierFailed reports whether any verifier in the result set
// returned Passed=false. A nil or empty slice means "no verifier
// ran" and returns false — i.e. EvalOutcomeFor falls back to the
// success-without-verifier branch. The compact name keeps the
// switch arm above readable.
func verifierFailed(results []VerificationResult) bool {
	for _, r := range results {
		if !r.Passed {
			return true
		}
	}
	return false
}
