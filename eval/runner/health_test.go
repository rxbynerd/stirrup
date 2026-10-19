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
	result, err := RunSuite(context.Background(), suite, cfg)
	require.NoError(t, err)
	return result
}

func TestRunSuite_JudgePassOnFailedRunIsCountedButNotHealthy(t *testing.T) {
	harness := healthHarness(t, "error", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	task := result.Tasks[0]
	require.Equal(t, "pass", task.Outcome, "the judge verdict alone decides the outcome")
	require.Equal(t, types.RunFailed, task.RunHealth)
	require.Equal(t, types.VerificationNone, task.Verification)
	require.Equal(t, eval.DeliveryNotMeasured, task.Delivery)
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
	require.NotNil(t, result.HealthyPassRate)
	require.InDelta(t, 0.0, *result.HealthyPassRate, 1e-9)
	require.False(t, result.RequireHealthy, "neither the flag nor the suite asked for it")
}

func TestRunSuite_NonZeroExitWithSuccessTraceIsFailed(t *testing.T) {
	harness := healthHarness(t, "success", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	require.Equal(t, types.RunFailed, result.Tasks[0].RunHealth,
		"the exit status is the operator-visible contract")
}

func TestRunSuite_HealthyRunCountsInBothRates(t *testing.T) {
	harness := healthHarness(t, "success", "[]", 0)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
	require.Equal(t, types.RunHealthy, result.Tasks[0].RunHealth)
	require.NotNil(t, result.HealthyPassRate)
	require.InDelta(t, 1.0, *result.HealthyPassRate, 1e-9)
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
}

func TestRunSuite_LimitHitTraceOutcomesAreDegraded(t *testing.T) {
	for _, outcome := range []string{"max_turns", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			harness := healthHarness(t, outcome, "[]", 0)

			result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
			require.Equal(t, types.RunDegraded, result.Tasks[0].RunHealth)
			require.NotNil(t, result.HealthyPassRate)
			require.InDelta(t, 0.0, *result.HealthyPassRate, 1e-9)
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

			result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness})
			task := result.Tasks[0]
			require.Equal(t, tc.want, task.Verification)
			require.Equal(t, types.RunHealthy, task.RunHealth,
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
			tc.cfg.HarnessPath = harness
			result := runHealthSuite(t, tc.suite(healthSuite()), tc.cfg)
			require.True(t, result.RequireHealthy, "requireHealthy not recorded in the result")

			task := result.Tasks[0]
			require.Equal(t, "pass", task.Outcome, "outcome is judge-based")
			require.InDelta(t, 1.0, result.PassRate, 1e-9, "pass rate is judge-based")

			got := task.GateCounts(result.RequireHealthy)
			require.Equal(t, 0, got.Pass, "the unhealthy pass is not counted as a pass")
			require.Equal(t, 1, got.Fail, "the unhealthy pass is counted as a fail")
		})
	}
}

func TestRunSuite_TrialsAggregateTheWorstHealth(t *testing.T) {
	harness := healthHarness(t, "error", "[]", 1)

	result := runHealthSuite(t, healthSuite(), RunConfig{HarnessPath: harness, Trials: 2})
	task := result.Tasks[0]
	require.Equal(t, types.RunFailed, task.RunHealth)
	require.Len(t, task.Trials, 2)
	for i, tr := range task.Trials {
		t.Run("trial "+strconv.Itoa(i+1), func(t *testing.T) {
			require.Equal(t, types.RunFailed, tr.RunHealth)
			require.Equal(t, eval.DeliveryNotMeasured, tr.Delivery)
		})
	}
	require.NotNil(t, result.HealthyPassRate)
	require.InDelta(t, 0.0, *result.HealthyPassRate, 1e-9)
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
}

func TestAggregateTrials_HealthAndVerificationFold(t *testing.T) {
	trial := func(health types.RunHealth, v types.VerificationStatus) eval.TaskResult {
		return eval.TaskResult{Outcome: "pass", RunHealth: health, Verification: v, Delivery: eval.DeliveryNotMeasured}
	}

	t.Run("mixed healthy and degraded trials", func(t *testing.T) {
		got := aggregateTrials("t", []eval.TaskResult{
			trial(types.RunHealthy, types.VerificationNone),
			trial(types.RunDegraded, types.VerificationPassed),
			trial(types.RunHealthy, types.VerificationPassed),
		})
		require.Equal(t, types.RunDegraded, got.RunHealth, "the worst trial sets run health")
		require.Equal(t, types.VerificationPassed, got.Verification)
	})

	t.Run("healthy trial with a failed trial", func(t *testing.T) {
		got := aggregateTrials("t", []eval.TaskResult{
			trial(types.RunHealthy, types.VerificationPassed),
			trial(types.RunFailed, types.VerificationFailed),
		})
		require.Equal(t, types.RunFailed, got.RunHealth)
		require.Equal(t, types.VerificationFailed, got.Verification)
	})
}

func TestErrorResult_IsAFailedRun(t *testing.T) {
	r := errorResult("t", time.Now(), context.Canceled)
	require.Equal(t, types.RunFailed, r.RunHealth)
	require.Equal(t, types.VerificationNone, r.Verification)
	require.Equal(t, eval.DeliveryNotMeasured, r.Delivery)
}
