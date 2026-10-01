package eval

import "testing"

func trials(outcomes ...string) []TrialResult {
	out := make([]TrialResult, len(outcomes))
	for i, o := range outcomes {
		out[i] = TrialResult{Trial: i + 1, Outcome: o}
	}
	return out
}

func TestTrialCounts_MajorityOutcome(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []string
		want     string
		fraction float64
	}{
		{"single pass", []string{"pass"}, "pass", 1},
		{"single fail", []string{"fail"}, "fail", 0},
		{"single error", []string{"error"}, "error", 0},
		{"two of three pass", []string{"pass", "fail", "pass"}, "pass", 2.0 / 3},
		{"two of three fail confirms the failure", []string{"fail", "pass", "fail"}, "fail", 1.0 / 3},
		{"errors never confirm a failure", []string{"fail", "error", "pass"}, "error", 1.0 / 3},
		{"error majority", []string{"error", "error", "pass"}, "error", 1.0 / 3},
		{"pass majority despite an error", []string{"pass", "error", "pass"}, "pass", 2.0 / 3},
		{"fail majority despite an error", []string{"fail", "fail", "error"}, "fail", 0},
		{"even tie has no majority", []string{"pass", "fail"}, "error", 0.5},
		{"two-two tie has no majority", []string{"pass", "pass", "fail", "fail"}, "error", 0.5},
		{"three of four pass", []string{"pass", "pass", "fail", "pass"}, "pass", 0.75},
		{"unknown outcome counts as error", []string{"skipped", "pass", "pass"}, "pass", 2.0 / 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := TaskResult{Trials: trials(tc.outcomes...)}.Counts()
			if c.Total() != len(tc.outcomes) {
				t.Errorf("Total() = %d, want %d", c.Total(), len(tc.outcomes))
			}
			if got := c.Outcome(); got != tc.want {
				t.Errorf("Outcome() = %q, want %q (counts %+v)", got, tc.want, c)
			}
			if got := c.PassFraction(); got != tc.fraction {
				t.Errorf("PassFraction() = %v, want %v", got, tc.fraction)
			}
		})
	}
}

func TestTrialCounts_Empty(t *testing.T) {
	var c TrialCounts
	if c.Outcome() != "error" || c.PassFraction() != 0 || c.Total() != 0 {
		t.Errorf("empty counts: outcome=%q fraction=%v total=%d", c.Outcome(), c.PassFraction(), c.Total())
	}
}

// TestTaskResult_CountsWithoutTrials pins that a result without Trials,
// as written before trials existed, counts as one trial of its Outcome.
func TestTaskResult_CountsWithoutTrials(t *testing.T) {
	for outcome, want := range map[string]TrialCounts{
		"pass":  {Pass: 1},
		"fail":  {Fail: 1},
		"error": {Error: 1},
		"":      {Error: 1},
	} {
		if got := (TaskResult{Outcome: outcome}).Counts(); got != want {
			t.Errorf("Counts() for outcome %q = %+v, want %+v", outcome, got, want)
		}
	}
}

func TestTaskResult_CountsPrefersTrials(t *testing.T) {
	r := TaskResult{Outcome: "pass", Trials: trials("fail", "fail", "pass")}
	if got := r.Counts(); got != (TrialCounts{Pass: 1, Fail: 2}) {
		t.Errorf("Counts() = %+v, want trials to take precedence over Outcome", got)
	}
}
