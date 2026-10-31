package types

import (
	"maps"
	"slices"
)

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
	// `structured_output_failed`) or ran to `success` but the last
	// verifier attempt disagreed.
	EvalFailed EvalOutcome = "failed"

	// EvalInconclusive indicates the run terminated without enough
	// signal to call it: limit-hit, non-result, or hook-infra
	// terminations.
	EvalInconclusive EvalOutcome = "inconclusive"
)

// outcomeClass is how one RunTrace.Outcome value is judged.
type outcomeClass struct {
	eval   EvalOutcome
	health RunHealth
}

// runOutcomes classifies every outcome the harness sets on
// RunTrace.Outcome, plus "max_tokens", the provider stop reason the
// harness relies on. Other provider stop reasons pass through verbatim
// and fall to the unknown-outcome defaults. A new outcome is added here
// and nowhere else.
var runOutcomes = map[string]outcomeClass{
	"success":                       {EvalPassed, RunHealthy},
	"error":                         {EvalFailed, RunFailed},
	"tool_failures":                 {EvalFailed, RunFailed},
	"verification_failed":           {EvalFailed, RunFailed},
	"structured_output_failed":      {EvalFailed, RunFailed},
	"verification_error":            {EvalInconclusive, RunFailed},
	"setup_failed":                  {EvalInconclusive, RunFailed},
	"hook_failed":                   {EvalInconclusive, RunFailed},
	"max_turns":                     {EvalInconclusive, RunDegraded},
	"budget_exceeded":               {EvalInconclusive, RunDegraded},
	"timeout":                       {EvalInconclusive, RunDegraded},
	"max_tokens":                    {EvalInconclusive, RunDegraded},
	"stalled":                       {EvalInconclusive, RunDegraded},
	"cancelled":                     {EvalInconclusive, RunDegraded},
	"context_exhausted":             {EvalInconclusive, RunDegraded},
	"handoff_failed":                {EvalInconclusive, RunDegraded},
	"control_stream_lost":           {EvalInconclusive, RunDegraded},
	"guardrail_blocked":             {EvalInconclusive, RunDegraded},
	"rule_of_two_violation":         {EvalInconclusive, RunDegraded},
	"command_output_capture_failed": {EvalInconclusive, RunDegraded},
	"command_output_archive_failed": {EvalInconclusive, RunDegraded},
}

// RunOutcomes returns every RunTrace.Outcome value the harness sets, plus
// "max_tokens", sorted. Provider stop reasons passed through as the
// outcome, such as "refusal", are not listed.
func RunOutcomes() []string {
	return slices.Sorted(maps.Keys(runOutcomes))
}

// EvalOutcomeFor maps a RunTrace's termination outcome and verifier
// verdict onto an EvalOutcome. See docs/eval.md for the full table.
func EvalOutcomeFor(t RunTrace) EvalOutcome {
	if t.Outcome == "success" && verifierFailed(t.VerificationResults) {
		return EvalFailed
	}
	if class, ok := runOutcomes[t.Outcome]; ok {
		return class.eval
	}
	// Conservative default: an unknown/future outcome must not be
	// treated as a quality pass.
	return EvalInconclusive
}

// verifierFailed reports whether the last recorded verifier attempt
// returned Passed=false. Every attempt is recorded, so a run that
// failed verification and then passed after a repair is judged on the
// passing attempt. A nil or empty slice returns false.
func verifierFailed(results []VerificationResult) bool {
	if len(results) == 0 {
		return false
	}
	return !results[len(results)-1].Passed
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
	if class, ok := runOutcomes[t.Outcome]; ok {
		return class.health
	}
	// Provider stop reasons and unknown or future outcomes.
	return RunDegraded
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

// LastVerification reports the verdict of the final result in the trace,
// the same attempt EvalOutcomeFor judges. A run that failed verification
// and then passed on a retry is VerificationPassed.
func LastVerification(t *RunTrace) VerificationStatus {
	if t == nil || len(t.VerificationResults) == 0 {
		return VerificationNone
	}
	if t.VerificationResults[len(t.VerificationResults)-1].Passed {
		return VerificationPassed
	}
	return VerificationFailed
}
