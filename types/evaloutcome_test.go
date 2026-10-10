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
			got := EvalOutcomeFor(tc.trace)
			if got != tc.want {
				t.Errorf("EvalOutcomeFor(%+v) = %q, want %q", tc.trace.Outcome, got, tc.want)
			}
		})
	}
}

func TestRunHealthFor(t *testing.T) {
	cases := []struct {
		outcome string
		want    RunHealth
	}{
		{"success", RunHealthy},
		{"error", RunFailed},
		{"tool_failures", RunFailed},
		{"verification_failed", RunFailed},
		{"verification_error", RunFailed},
		{"setup_failed", RunFailed},
		{"hook_failed", RunFailed},
		{"structured_output_failed", RunFailed},
		{"max_turns", RunDegraded},
		{"timeout", RunDegraded},
		{"max_tokens", RunDegraded},
		{"budget_exceeded", RunDegraded},
		{"stalled", RunDegraded},
		{"cancelled", RunDegraded},
		{"refusal", RunDegraded},
		{"some_future_outcome", RunDegraded},
		{"", RunDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			if got := RunHealthFor(&RunTrace{Outcome: tc.outcome}, false); got != tc.want {
				t.Errorf("RunHealthFor(%q, false) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
}

func TestRunHealthFor_HarnessFailureAndMissingTrace(t *testing.T) {
	for _, outcome := range []string{"success", "max_turns", "error"} {
		if got := RunHealthFor(&RunTrace{Outcome: outcome}, true); got != RunFailed {
			t.Errorf("a non-zero exit with outcome %q = %q, want %q", outcome, got, RunFailed)
		}
	}
	if got := RunHealthFor(nil, false); got != RunFailed {
		t.Errorf("a nil trace = %q, want %q", got, RunFailed)
	}
}

func TestRunHealthFor_SuccessWithFailedVerifierStaysHealthy(t *testing.T) {
	trace := &RunTrace{Outcome: "success", VerificationResults: []VerificationResult{{Passed: false}}}
	if got := RunHealthFor(trace, false); got != RunHealthy {
		t.Errorf("health = %q, want %q (the verifier verdict is reported separately)", got, RunHealthy)
	}
	if got := EvalOutcomeFor(*trace); got != EvalFailed {
		t.Errorf("EvalOutcomeFor = %q, want %q (unchanged)", got, EvalFailed)
	}
}

func TestLastVerification(t *testing.T) {
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
			if got := LastVerification(tc.trace); got != tc.want {
				t.Errorf("LastVerification = %q, want %q", got, tc.want)
			}
		})
	}
}
