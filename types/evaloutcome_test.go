package types

import "testing"

// TestEvalOutcomeFor walks the (Outcome, VerificationResults) →
// EvalOutcome mapping documented in docs/eval.md.
func TestEvalOutcomeFor(t *testing.T) {
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
			name: "success with mixed verifier verdicts",
			trace: RunTrace{
				Outcome: "success",
				VerificationResults: []VerificationResult{
					{Passed: true},
					{Passed: false},
				},
			},
			want: EvalFailed,
		},
		{name: "verification_failed", trace: RunTrace{Outcome: "verification_failed"}, want: EvalFailed},
		{name: "error", trace: RunTrace{Outcome: "error"}, want: EvalFailed},
		{name: "tool_failures", trace: RunTrace{Outcome: "tool_failures"}, want: EvalFailed},

		{name: "max_turns", trace: RunTrace{Outcome: "max_turns"}, want: EvalInconclusive},
		{name: "budget_exceeded", trace: RunTrace{Outcome: "budget_exceeded"}, want: EvalInconclusive},
		{name: "timeout", trace: RunTrace{Outcome: "timeout"}, want: EvalInconclusive},
		{name: "max_tokens", trace: RunTrace{Outcome: "max_tokens"}, want: EvalInconclusive},
		{name: "stalled", trace: RunTrace{Outcome: "stalled"}, want: EvalInconclusive},
		{name: "cancelled", trace: RunTrace{Outcome: "cancelled"}, want: EvalInconclusive},
		{name: "verification_error", trace: RunTrace{Outcome: "verification_error"}, want: EvalInconclusive},

		// Lifecycle-hook infra failures are pinned explicitly rather than
		// relying on the unknown-outcome default below.
		{name: "setup_failed", trace: RunTrace{Outcome: "setup_failed"}, want: EvalInconclusive},
		{name: "hook_failed", trace: RunTrace{Outcome: "hook_failed"}, want: EvalInconclusive},

		// Unknown / empty outcomes fall through to inconclusive so a
		// future outcome class added upstream cannot silently land in
		// the wrong bucket.
		{name: "empty outcome", trace: RunTrace{Outcome: ""}, want: EvalInconclusive},
		{name: "unknown outcome", trace: RunTrace{Outcome: "guardrail_blocked"}, want: EvalInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvalOutcomeFor(tc.trace)
			if got != tc.want {
				t.Errorf("EvalOutcomeFor(%+v) = %q, want %q", tc.trace.Outcome, got, tc.want)
			}
		})
	}
}
