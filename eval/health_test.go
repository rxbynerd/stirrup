package eval

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestGateCounts_RequireHealthyDemotesUnhealthyPasses(t *testing.T) {
	task := TaskResult{
		Outcome: "pass",
		Trials: []TrialResult{
			{Outcome: "pass", RunHealth: types.RunHealthy},
			{Outcome: "pass", RunHealth: types.RunFailed},
			{Outcome: "pass", RunHealth: types.RunDegraded},
			{Outcome: "fail", RunHealth: types.RunHealthy},
			{Outcome: "error", RunHealth: types.RunFailed},
		},
	}

	t.Run("counts ignore run health", func(t *testing.T) {
		require.Equal(t, TrialCounts{Pass: 3, Fail: 1, Error: 1}, task.Counts())
	})
	t.Run("require healthy counts unhealthy passes as fails", func(t *testing.T) {
		require.Equal(t, TrialCounts{Pass: 1, Fail: 3, Error: 1}, task.GateCounts(true))
	})
	t.Run("without require healthy matches counts", func(t *testing.T) {
		require.Equal(t, task.Counts(), task.GateCounts(false))
	})
	t.Run("healthy pass fraction", func(t *testing.T) {
		require.InDelta(t, 0.2, task.HealthyPassFraction(), 1e-9)
	})
}

func TestGateCounts_RequireHealthyFailsPassWithoutHealth(t *testing.T) {
	legacy := TaskResult{Outcome: "pass"}
	got := legacy.GateCounts(true)
	require.Equal(t, 0, got.Pass)
	require.Equal(t, 1, got.Fail)
}

func TestHealthyPassRate_CountsHealthyPasses(t *testing.T) {
	t.Run("nil for no tasks", func(t *testing.T) {
		require.Nil(t, HealthyPassRate(nil))
	})
	t.Run("nil when no task has run health", func(t *testing.T) {
		legacy := []TaskResult{{Outcome: "pass"}, {Outcome: "fail"}}
		require.Nil(t, HealthyPassRate(legacy))
	})
	t.Run("healthy passes over all tasks", func(t *testing.T) {
		tasks := []TaskResult{
			{Outcome: "pass", RunHealth: types.RunHealthy},
			{Outcome: "pass", RunHealth: types.RunFailed},
			{Outcome: "fail", RunHealth: types.RunHealthy},
			{Outcome: "pass", RunHealth: types.RunDegraded},
		}
		got := HealthyPassRate(tasks)
		require.NotNil(t, got)
		require.InDelta(t, 0.25, *got, 1e-9)
	})
}

func TestSuiteResult_LegacyJSONDecodesWithoutHealth(t *testing.T) {
	legacy := `{"suiteId":"s","runId":"r","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:01Z",
"tasks":[{"taskId":"a","outcome":"pass","judgeVerdict":{"passed":true,"reason":""},"durationMs":1,"passFraction":1}],"passRate":1}`
	var r SuiteResult
	require.NoError(t, json.Unmarshal([]byte(legacy), &r))
	require.Nil(t, r.HealthyPassRate)
	require.False(t, r.RequireHealthy)

	task := r.Tasks[0]
	require.Empty(t, task.RunHealth)
	require.Empty(t, task.Verification)
	require.Empty(t, task.Delivery)
	require.Equal(t, 1, task.Counts().Pass)

	out, err := json.Marshal(r)
	require.NoError(t, err)
	for _, key := range []string{"healthyPassRate", "requireHealthy", "runHealth", "verification", "delivery"} {
		t.Run("omits "+key, func(t *testing.T) {
			require.NotContains(t, string(out), key)
		})
	}
}
