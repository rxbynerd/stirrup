package runner

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// healthHarness writes out.txt into its workspace (its working directory)
// and a trace with the given outcome and verification results, then exits
// with the given status.
func healthHarness(t *testing.T, outcome, verification string, exit int) string {
	t.Helper()
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
touch out.txt
echo '{"id":"t","turns":1,"outcome":"` + outcome + `","verificationResults":` + verification + `}' > "$TRACE"
exit ` + strconv.Itoa(exit) + `
`
	return writeFakeHarness(t, script)
}

func healthSuite() types.EvalSuite {
	return types.EvalSuite{
		ID: "health",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"out.txt"}}},
		},
	}
}

func TestRunSuite_JudgePassOnFailedRunIsCountedButNotHealthy(t *testing.T) {
	harness := healthHarness(t, "error", "[]", 1)

	result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	task := result.Tasks[0]
	if task.Outcome != "pass" {
		t.Errorf("outcome = %q, want pass (the judge verdict alone)", task.Outcome)
	}
	if task.RunHealth != types.RunFailed {
		t.Errorf("runHealth = %q, want failed", task.RunHealth)
	}
	if task.Verification != types.VerificationNone {
		t.Errorf("verification = %q, want none", task.Verification)
	}
	if task.Delivery != eval.DeliveryNotMeasured {
		t.Errorf("delivery = %q, want %q", task.Delivery, eval.DeliveryNotMeasured)
	}
	if result.PassRate != 1 {
		t.Errorf("passRate = %v, want 1", result.PassRate)
	}
	if result.HealthyPassRate == nil || *result.HealthyPassRate != 0 {
		t.Errorf("healthyPassRate = %v, want 0", result.HealthyPassRate)
	}
	if result.RequireHealthy {
		t.Error("requireHealthy set although neither the flag nor the suite asked for it")
	}
}

func TestRunSuite_NonZeroExitWithSuccessTraceIsFailed(t *testing.T) {
	harness := healthHarness(t, "success", "[]", 1)

	result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if got := result.Tasks[0].RunHealth; got != types.RunFailed {
		t.Errorf("runHealth = %q, want failed (the exit status is the operator-visible contract)", got)
	}
}

func TestRunSuite_HealthyRunCountsInBothRates(t *testing.T) {
	harness := healthHarness(t, "success", "[]", 0)

	result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if got := result.Tasks[0].RunHealth; got != types.RunHealthy {
		t.Errorf("runHealth = %q, want healthy", got)
	}
	if result.HealthyPassRate == nil || *result.HealthyPassRate != 1 || result.PassRate != 1 {
		t.Errorf("rates = %v / %v, want 1 / 1", result.PassRate, result.HealthyPassRate)
	}
}

func TestRunSuite_LimitHitTraceOutcomesAreDegraded(t *testing.T) {
	for _, outcome := range []string{"max_turns", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			harness := healthHarness(t, outcome, "[]", 0)

			result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness})
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if got := result.Tasks[0].RunHealth; got != types.RunDegraded {
				t.Errorf("runHealth = %q, want degraded", got)
			}
			if got := result.HealthyPassRate; got == nil || *got != 0 {
				t.Errorf("healthyPassRate = %v, want 0", got)
			}
		})
	}
}

func TestRunSuite_VerificationReflectsTheLastResult(t *testing.T) {
	cases := []struct {
		name         string
		verification string
		want         types.VerificationStatus
	}{
		{"failed then passed", `[{"passed":false},{"passed":true}]`, types.VerificationPassed},
		{"passed then failed", `[{"passed":true},{"passed":false}]`, types.VerificationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := healthHarness(t, "success", tc.verification, 0)

			result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness})
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			task := result.Tasks[0]
			if task.Verification != tc.want {
				t.Errorf("verification = %q, want %q", task.Verification, tc.want)
			}
			if task.RunHealth != types.RunHealthy {
				t.Errorf("runHealth = %q, want healthy (verification is reported separately)", task.RunHealth)
			}
		})
	}
}

func TestRunSuite_RequireHealthyGatesWithoutChangingTheOutcome(t *testing.T) {
	harness := healthHarness(t, "error", "[]", 1)

	cases := []struct {
		name  string
		cfg   RunConfig
		suite func(types.EvalSuite) types.EvalSuite
	}{
		{"flag", RunConfig{RequireHealthy: true}, func(s types.EvalSuite) types.EvalSuite { return s }},
		{"suite attribute", RunConfig{}, func(s types.EvalSuite) types.EvalSuite { s.RequireHealthy = true; return s }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.HarnessPath = harness
			result, err := RunSuite(context.Background(), tc.suite(healthSuite()), tc.cfg)
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if !result.RequireHealthy {
				t.Fatal("requireHealthy not recorded in the result")
			}
			task := result.Tasks[0]
			if task.Outcome != "pass" || result.PassRate != 1 {
				t.Errorf("outcome %q passRate %v, want pass / 1 (judge-based)", task.Outcome, result.PassRate)
			}
			if got := task.GateCounts(result.RequireHealthy); got.Pass != 0 || got.Fail != 1 {
				t.Errorf("gate counts = %+v, want the unhealthy pass counted as a fail", got)
			}
		})
	}
}

func TestRunSuite_TrialsAggregateTheWorstHealth(t *testing.T) {
	harness := healthHarness(t, "error", "[]", 1)

	result, err := RunSuite(context.Background(), healthSuite(), RunConfig{HarnessPath: harness, Trials: 2})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	task := result.Tasks[0]
	if task.RunHealth != types.RunFailed || len(task.Trials) != 2 {
		t.Fatalf("task health %q with %d trials, want failed with 2", task.RunHealth, len(task.Trials))
	}
	for i, tr := range task.Trials {
		if tr.RunHealth != types.RunFailed || tr.Delivery != eval.DeliveryNotMeasured {
			t.Errorf("trial %d health %q delivery %q", i+1, tr.RunHealth, tr.Delivery)
		}
	}
	if result.HealthyPassRate == nil || *result.HealthyPassRate != 0 || result.PassRate != 1 {
		t.Errorf("rates = %v / %v, want 1 / 0", result.PassRate, result.HealthyPassRate)
	}
}

func TestAggregateTrials_HealthAndVerificationFold(t *testing.T) {
	run := func(health types.RunHealth, v types.VerificationStatus) eval.TaskResult {
		return eval.TaskResult{Outcome: "pass", RunHealth: health, Verification: v, Delivery: eval.DeliveryNotMeasured}
	}
	got := aggregateTrials("t", []eval.TaskResult{
		run(types.RunHealthy, types.VerificationNone),
		run(types.RunDegraded, types.VerificationPassed),
		run(types.RunHealthy, types.VerificationPassed),
	})
	if got.RunHealth != types.RunDegraded {
		t.Errorf("health = %q, want degraded (the worst trial)", got.RunHealth)
	}
	if got.Verification != types.VerificationPassed {
		t.Errorf("verification = %q, want passed", got.Verification)
	}

	got = aggregateTrials("t", []eval.TaskResult{
		run(types.RunHealthy, types.VerificationPassed),
		run(types.RunFailed, types.VerificationFailed),
	})
	if got.RunHealth != types.RunFailed || got.Verification != types.VerificationFailed {
		t.Errorf("health %q verification %q, want failed / failed", got.RunHealth, got.Verification)
	}
}

func TestErrorResultIsAFailedRun(t *testing.T) {
	r := errorResult("t", time.Now(), context.Canceled)
	if r.RunHealth != types.RunFailed || r.Verification != types.VerificationNone || r.Delivery != eval.DeliveryNotMeasured {
		t.Errorf("error result health %q verification %q delivery %q", r.RunHealth, r.Verification, r.Delivery)
	}
}
