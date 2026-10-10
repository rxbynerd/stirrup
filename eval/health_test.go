package eval

import (
	"encoding/json"
	"strings"
	"testing"

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
	if got := task.Counts(); got != (TrialCounts{Pass: 3, Fail: 1, Error: 1}) {
		t.Errorf("Counts = %+v, want the judge-based tally", got)
	}
	if got := task.GateCounts(true); got != (TrialCounts{Pass: 1, Fail: 3, Error: 1}) {
		t.Errorf("GateCounts(true) = %+v, want unhealthy passes counted as fails", got)
	}
	if got := task.GateCounts(false); got != task.Counts() {
		t.Errorf("GateCounts(false) = %+v, want Counts() = %+v", got, task.Counts())
	}
	if got := task.HealthyPassFraction(); got != 0.2 {
		t.Errorf("HealthyPassFraction = %v, want 0.2", got)
	}
}

func TestGateCounts_SingleRunWithoutHealth(t *testing.T) {
	legacy := TaskResult{Outcome: "pass"}
	if got := legacy.GateCounts(true); got.Pass != 0 || got.Fail != 1 {
		t.Errorf("a pass without recorded health under require_healthy = %+v, want a fail", got)
	}
}

func TestHealthyPassRate(t *testing.T) {
	if got := HealthyPassRate(nil); got != nil {
		t.Errorf("HealthyPassRate(nil) = %v, want nil", *got)
	}
	legacy := []TaskResult{{Outcome: "pass"}, {Outcome: "fail"}}
	if got := HealthyPassRate(legacy); got != nil {
		t.Errorf("HealthyPassRate over results without run health = %v, want nil", *got)
	}

	tasks := []TaskResult{
		{Outcome: "pass", RunHealth: types.RunHealthy},
		{Outcome: "pass", RunHealth: types.RunFailed},
		{Outcome: "fail", RunHealth: types.RunHealthy},
		{Outcome: "pass", RunHealth: types.RunDegraded},
	}
	got := HealthyPassRate(tasks)
	if got == nil || *got != 0.25 {
		t.Errorf("HealthyPassRate = %v, want 0.25", got)
	}
}

func TestSuiteResult_LegacyJSONDecodesWithoutHealth(t *testing.T) {
	legacy := `{"suiteId":"s","runId":"r","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:01Z",
"tasks":[{"taskId":"a","outcome":"pass","judgeVerdict":{"passed":true,"reason":""},"durationMs":1,"passFraction":1}],"passRate":1}`
	var r SuiteResult
	if err := json.Unmarshal([]byte(legacy), &r); err != nil {
		t.Fatal(err)
	}
	if r.HealthyPassRate != nil || r.RequireHealthy {
		t.Errorf("legacy result decoded with health data: %+v", r)
	}
	task := r.Tasks[0]
	if task.RunHealth != "" || task.Verification != "" || task.Delivery != "" {
		t.Errorf("legacy task decoded with health data: %+v", task)
	}
	if got := task.Counts(); got.Pass != 1 {
		t.Errorf("Counts = %+v, want one pass", got)
	}

	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"healthyPassRate", "requireHealthy", "runHealth", "verification", "delivery"} {
		if strings.Contains(string(out), key) {
			t.Errorf("re-encoded legacy result gained %q: %s", key, out)
		}
	}
}
