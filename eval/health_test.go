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
		must := require.New(t)
		must.Equal(TrialCounts{Pass: 3, Fail: 1, Error: 1}, task.Counts())
	})
	t.Run("require healthy counts unhealthy passes as fails", func(t *testing.T) {
		must := require.New(t)
		must.Equal(TrialCounts{Pass: 1, Fail: 3, Error: 1}, task.GateCounts(true))
	})
	t.Run("without require healthy matches counts", func(t *testing.T) {
		must := require.New(t)
		must.Equal(task.Counts(), task.GateCounts(false))
	})
	t.Run("healthy pass fraction", func(t *testing.T) {
		must := require.New(t)
		must.InDelta(0.2, task.HealthyPassFraction(), 1e-9)
	})
}

func TestGateCounts_RequireHealthyFailsPassWithoutHealth(t *testing.T) {
	must := require.New(t)
	legacy := TaskResult{Outcome: "pass"}
	got := legacy.GateCounts(true)
	must.Equal(0, got.Pass)
	must.Equal(1, got.Fail)
}

func TestHealthyPassRate_CountsHealthyPasses(t *testing.T) {
	t.Run("nil for no tasks", func(t *testing.T) {
		must := require.New(t)
		must.Nil(HealthyPassRate(nil))
	})
	t.Run("nil when no task has run health", func(t *testing.T) {
		must := require.New(t)
		legacy := []TaskResult{{Outcome: "pass"}, {Outcome: "fail"}}
		must.Nil(HealthyPassRate(legacy))
	})
	t.Run("healthy passes over all tasks", func(t *testing.T) {
		must := require.New(t)
		tasks := []TaskResult{
			{Outcome: "pass", RunHealth: types.RunHealthy},
			{Outcome: "pass", RunHealth: types.RunFailed},
			{Outcome: "fail", RunHealth: types.RunHealthy},
			{Outcome: "pass", RunHealth: types.RunDegraded},
		}
		got := HealthyPassRate(tasks)
		must.NotNil(got)
		must.InDelta(0.25, *got, 1e-9)
	})
}

func TestSuiteResult_LegacyJSONDecodesWithoutHealth(t *testing.T) {
	must := require.New(t)
	legacy := `{"suiteId":"s","runId":"r","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:01Z",
"tasks":[{"taskId":"a","outcome":"pass","judgeVerdict":{"passed":true,"reason":""},"durationMs":1,"passFraction":1}],"passRate":1}`
	var r SuiteResult
	must.NoError(json.Unmarshal([]byte(legacy), &r))
	must.Nil(r.HealthyPassRate)
	must.False(r.RequireHealthy)

	task := r.Tasks[0]
	must.Empty(task.RunHealth)
	must.Empty(task.Verification)
	must.Empty(task.Delivery)
	must.Equal(1, task.Counts().Pass)

	out, err := json.Marshal(r)
	must.NoError(err)
	for _, key := range []string{"healthyPassRate", "requireHealthy", "runHealth", "verification", "delivery"} {
		t.Run("omits "+key, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(string(out), key)
		})
	}
}
