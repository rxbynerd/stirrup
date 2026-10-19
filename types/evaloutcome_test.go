package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEvalOutcomeFor_MapsRunOutcomes walks the (Outcome, VerificationResults) →
// EvalOutcome mapping documented in docs/eval.md.
func TestEvalOutcomeFor_MapsRunOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		trace RunTrace
		want  EvalOutcome
	}{
		{
			name:  "success without verifier",
			trace: RunTrace{Outcome: "success"},
			want:  EvalPassed,
		},
		{
			name: "success with passing verifier",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Passed: true},
				},
			},
			want: EvalPassed,
		},
		{
			name: "success with multiple passing verifiers",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Passed: true},
					{Passed: true},
				},
			},
			want: EvalPassed,
		},
		{
			name: "success with failing verifier",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Passed: false},
				},
			},
			want: EvalFailed,
		},
		{
			name: "success with last verifier attempt failing",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Attempt: 1, Passed: true},
					{Attempt: 2, Passed: false},
				},
			},
			want: EvalFailed,
		},
		{
			name: "success after a failed attempt and a passing repair",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Attempt: 1, Passed: false, Feedback: "tests failed"},
					{Attempt: 2, Passed: true},
				},
			},
			want: EvalPassed,
		},
		{
			name: "verification_failed with recorded attempts",
			trace: RunTrace{
				Outcome: "verification_failed",
				VerificationResults: []VerificationResult{
					{Attempt: 1, Passed: false},
					{Attempt: 2, Passed: false},
				},
			},
			want: EvalFailed,
		},
		{
			name: "verification_error with a recorded verifier error",
			trace: RunTrace{
				Outcome: "verification_error",
				VerificationResults: []VerificationResult{
					{Attempt: 1, Passed: false, Error: "exec failed"},
				},
			},
			want: EvalInconclusive,
		},
		{name: "verification_failed", trace: RunTrace{Outcome: "verification_failed"}, want: EvalFailed},
		{name: "error", trace: RunTrace{Outcome: "error"}, want: EvalFailed},
		{name: "tool_failures", trace: RunTrace{Outcome: "tool_failures"}, want: EvalFailed},
		{name: "structured_output_failed", trace: RunTrace{Outcome: "structured_output_failed"}, want: EvalFailed},

		{name: "max_turns", trace: RunTrace{Outcome: "max_turns"}, want: EvalInconclusive},
		{name: "budget_exceeded", trace: RunTrace{Outcome: "budget_exceeded"}, want: EvalInconclusive},
		{name: "timeout", trace: RunTrace{Outcome: "timeout"}, want: EvalInconclusive},
		{name: "max_tokens", trace: RunTrace{Outcome: "max_tokens"}, want: EvalInconclusive},
		{name: "stalled", trace: RunTrace{Outcome: "stalled"}, want: EvalInconclusive},
		{name: "cancelled", trace: RunTrace{Outcome: "cancelled"}, want: EvalInconclusive},
		{name: "verification_error", trace: RunTrace{Outcome: "verification_error"}, want: EvalInconclusive},
		{name: "context_exhausted", trace: RunTrace{Outcome: "context_exhausted"}, want: EvalInconclusive},

		// Lifecycle-hook infra failures are pinned explicitly rather than
		// relying on the unknown-outcome default below.
		{name: "setup_failed", trace: RunTrace{Outcome: "setup_failed"}, want: EvalInconclusive},
		{name: "hook_failed", trace: RunTrace{Outcome: "hook_failed"}, want: EvalInconclusive},
		{name: "handoff_failed", trace: RunTrace{Outcome: "handoff_failed"}, want: EvalInconclusive},
		{name: "control_stream_lost", trace: RunTrace{Outcome: "control_stream_lost"}, want: EvalInconclusive},

		// Unknown / empty outcomes fall through to inconclusive so a
		// future outcome class added upstream cannot silently land in
		// the wrong bucket.
		{name: "empty outcome", trace: RunTrace{Outcome: ""}, want: EvalInconclusive},
		{name: "unknown outcome", trace: RunTrace{Outcome: "guardrail_blocked"}, want: EvalInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, EvalOutcomeFor(tc.trace))
		})
	}
}

func TestRunHealthFor_MapsRunOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		want    RunHealth
	}{
		{name: "success", outcome: "success", want: RunHealthy},
		{name: "error", outcome: "error", want: RunFailed},
		{name: "tool_failures", outcome: "tool_failures", want: RunFailed},
		{name: "verification_failed", outcome: "verification_failed", want: RunFailed},
		{name: "verification_error", outcome: "verification_error", want: RunFailed},
		{name: "setup_failed", outcome: "setup_failed", want: RunFailed},
		{name: "hook_failed", outcome: "hook_failed", want: RunFailed},
		{name: "structured_output_failed", outcome: "structured_output_failed", want: RunFailed},
		{name: "max_turns", outcome: "max_turns", want: RunDegraded},
		{name: "timeout", outcome: "timeout", want: RunDegraded},
		{name: "max_tokens", outcome: "max_tokens", want: RunDegraded},
		{name: "budget_exceeded", outcome: "budget_exceeded", want: RunDegraded},
		{name: "stalled", outcome: "stalled", want: RunDegraded},
		{name: "cancelled", outcome: "cancelled", want: RunDegraded},
		{name: "refusal", outcome: "refusal", want: RunDegraded},
		{name: "unknown future outcome", outcome: "some_future_outcome", want: RunDegraded},
		{name: "empty outcome", outcome: "", want: RunDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RunHealthFor(&RunTrace{Outcome: tc.outcome}, false))
		})
	}
}

func TestRunHealthFor_HarnessFailureAndMissingTrace(t *testing.T) {
	for _, outcome := range []string{"success", "max_turns", "error"} {
		t.Run("non-zero exit with outcome "+outcome, func(t *testing.T) {
			require.Equal(t, RunFailed, RunHealthFor(&RunTrace{Outcome: outcome}, true),
				"a non-zero exit fails the run regardless of the outcome")
		})
	}
	t.Run("nil trace", func(t *testing.T) {
		require.Equal(t, RunFailed, RunHealthFor(nil, false))
	})
}

func TestRunHealthFor_SuccessWithFailedVerifierStaysHealthy(t *testing.T) {
	trace := &RunTrace{Outcome: "success", VerificationResults: []VerificationResult{{Passed: false}}}

	t.Run("health", func(t *testing.T) {
		require.Equal(t, RunHealthy, RunHealthFor(trace, false),
			"the verifier verdict is reported separately")
	})
	t.Run("eval outcome", func(t *testing.T) {
		require.Equal(t, EvalFailed, EvalOutcomeFor(*trace),
			"the eval outcome still counts the failed verifier")
	})
}

func TestLastVerification_ReturnsLatestResultStatus(t *testing.T) {
	pass, fail := VerificationResult{Passed: true}, VerificationResult{Passed: false}
	cases := []struct {
		name  string
		trace *RunTrace
		want  VerificationStatus
	}{
		{"nil trace", nil, VerificationNone},
		{"no results", &RunTrace{}, VerificationNone},
		{"single pass", &RunTrace{VerificationResults: []VerificationResult{pass}}, VerificationPassed},
		{"single fail", &RunTrace{VerificationResults: []VerificationResult{fail}}, VerificationFailed},
		{"fail then pass", &RunTrace{VerificationResults: []VerificationResult{fail, pass}}, VerificationPassed},
		{"pass then fail", &RunTrace{VerificationResults: []VerificationResult{pass, fail}}, VerificationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, LastVerification(tc.trace))
		})
	}
}
