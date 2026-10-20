package runner

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

// runHealthSuite runs the suite and fails the test if the run itself errors.
func runHealthSuite(t *testing.T, suite types.EvalSuite, cfg RunConfig) eval.SuiteResult {
	t.Helper()
	must := require.New(t)
	result, err := RunSuite(context.Background(), suite, cfg)
	must.NoError(err)
	return result
}

func TestRunSuite_JudgePassOnFailedRunIsCountedButNotHealthy(t *testing.T) {
	must := require.New(t)
	harness := healthHarness(t, "error", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	task := result.Tasks[0]
	must.Equal("pass", task.Outcome, "the judge verdict alone decides the outcome")
	must.Equal(types.RunFailed, task.RunHealth)
	must.Equal(types.VerificationNone, task.Verification)
	must.Equal(eval.DeliveryNotMeasured, task.Delivery)
	must.InDelta(1.0, result.PassRate, 1e-9)
	must.NotNil(result.HealthyPassRate)
	must.InDelta(0.0, *result.HealthyPassRate, 1e-9)
	must.False(result.RequireHealthy, "neither the flag nor the suite asked for it")
}

func TestRunSuite_NonZeroExitWithSuccessTraceIsFailed(t *testing.T) {
	must := require.New(t)
	harness := healthHarness(t, "success", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	must.Equal(types.RunFailed, result.Tasks[0].RunHealth,
		"the exit status is the operator-visible contract")
}

func TestRunSuite_HealthyRunCountsInBothRates(t *testing.T) {
	must := require.New(t)
	harness := healthHarness(t, "success", "[]", 0)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	must.Equal(types.RunHealthy, result.Tasks[0].RunHealth)
	must.NotNil(result.HealthyPassRate)
	must.InDelta(1.0, *result.HealthyPassRate, 1e-9)
	must.InDelta(1.0, result.PassRate, 1e-9)
}

func TestRunSuite_LimitHitTraceOutcomesAreDegraded(t *testing.T) {
	for _, outcome := range []string{"max_turns", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			must := require.New(t)
			harness := healthHarness(t, outcome, "[]", 0)

			result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
			must.Equal(types.RunDegraded, result.Tasks[0].RunHealth)
			must.NotNil(result.HealthyPassRate)
			must.InDelta(0.0, *result.HealthyPassRate, 1e-9)
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
			must := require.New(t)
			harness := healthHarness(t, "success", tc.verification, 0)

			result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
			task := result.Tasks[0]
			must.Equal(tc.want, task.Verification)
			must.Equal(types.RunHealthy, task.RunHealth,
				"verification is reported separately from run health")
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
			must := require.New(t)
			tc.cfg.HarnessPath = harness
			result := runHealthSuite(t, tc.suite(healthSuite()), tc.cfg)
			must.True(result.RequireHealthy, "requireHealthy not recorded in the result")

			task := result.Tasks[0]
			must.Equal("pass", task.Outcome, "outcome is judge-based")
			must.InDelta(1.0, result.PassRate, 1e-9, "pass rate is judge-based")

			got := task.GateCounts(result.RequireHealthy)
			must.Equal(0, got.Pass, "the unhealthy pass is not counted as a pass")
			must.Equal(1, got.Fail, "the unhealthy pass is counted as a fail")
		})
	}
}

func TestRunSuite_TrialsAggregateTheWorstHealth(t *testing.T) {
	must := require.New(t)
	harness := healthHarness(t, "error", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness, Trials: 2})
	task := result.Tasks[0]
	must.Equal(types.RunFailed, task.RunHealth)
	must.Len(task.Trials, 2)
	for i, tr := range task.Trials {
		t.Run("trial "+strconv.Itoa(i+1), func(t *testing.T) {
			must := require.New(t)
			must.Equal(types.RunFailed, tr.RunHealth)
			must.Equal(eval.DeliveryNotMeasured, tr.Delivery)
		})
	}
	must.NotNil(result.HealthyPassRate)
	must.InDelta(0.0, *result.HealthyPassRate, 1e-9)
	must.InDelta(1.0, result.PassRate, 1e-9)
}

func TestAggregateTrials_HealthAndVerificationFold(t *testing.T) {
	trial := func(health types.RunHealth, v types.VerificationStatus) eval.TaskResult {
		return eval.TaskResult{Outcome: "pass", RunHealth: health, Verification: v, Delivery: eval.DeliveryNotMeasured}
	}

	t.Run("mixed healthy and degraded trials", func(t *testing.T) {
		must := require.New(t)
		got := aggregateTrials("t", []eval.TaskResult{
			trial(types.RunHealthy, types.VerificationNone),
			trial(types.RunDegraded, types.VerificationPassed),
			trial(types.RunHealthy, types.VerificationPassed),
		})
		must.Equal(types.RunDegraded, got.RunHealth, "the worst trial sets run health")
		must.Equal(types.VerificationPassed, got.Verification)
	})

	t.Run("healthy trial with a failed trial", func(t *testing.T) {
		must := require.New(t)
		got := aggregateTrials("t", []eval.TaskResult{
			trial(types.RunHealthy, types.VerificationPassed),
			trial(types.RunFailed, types.VerificationFailed),
		})
		must.Equal(types.RunFailed, got.RunHealth)
		must.Equal(types.VerificationFailed, got.Verification)
	})
}

func TestErrorResult_IsAFailedRun(t *testing.T) {
	must := require.New(t)
	r := errorResult("t", time.Now(), context.Canceled)
	must.Equal(types.RunFailed, r.RunHealth)
	must.Equal(types.VerificationNone, r.Verification)
	must.Equal(eval.DeliveryNotMeasured, r.Delivery)
}
