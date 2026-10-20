package reporter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func healthTask(id, outcome string, health types.RunHealth) eval.TaskResult {
	return eval.TaskResult{TaskID: id, Outcome: outcome, RunHealth: health, PassFraction: map[string]float64{"pass": 1}[outcome]}
}

func healthSuiteResult(runID string, requireHealthy bool, tasks ...eval.TaskResult) eval.SuiteResult {
	return eval.SuiteResult{
		RunID:           runID,
		Tasks:           tasks,
		PassRate:        meanFraction(tasks),
		HealthyPassRate: eval.HealthyPassRate(tasks),
		RequireHealthy:  requireHealthy,
	}
}

func meanFraction(tasks []eval.TaskResult) float64 {
	var sum float64
	for _, t := range tasks {
		sum += t.PassFraction
	}
	return sum / float64(len(tasks))
}

func TestCompare_LegacyResultsKeepTheirNumbers(t *testing.T) {
	must := require.New(t)
	legacy := `{"suiteId":"s","runId":"%s","tasks":[
{"taskId":"a","outcome":"pass","judgeVerdict":{"passed":true,"reason":""},"durationMs":1,"passFraction":1},
{"taskId":"b","outcome":"fail","judgeVerdict":{"passed":false,"reason":""},"durationMs":1,"passFraction":0}],"passRate":0.5}`
	decode := func(id string) eval.SuiteResult {
		var r eval.SuiteResult
		must.NoError(json.Unmarshal([]byte(strings.Replace(legacy, "%s", id, 1)), &r))
		return r
	}

	report := Compare(decode("base"), decode("curr"), DefaultOptions())
	s := report.Summary
	must.InDelta(0.5, s.BaselinePassRate, 1e-12)
	must.InDelta(0.5, s.CurrentPassRate, 1e-12)
	must.InDelta(0.0, s.PassRateDelta, 1e-12)
	must.Nil(s.Baseline.HealthyPassRate, "a legacy result reports a healthy pass rate")
	must.Nil(s.Current.HealthyPassRate, "a legacy result reports a healthy pass rate")

	text := FormatText(report)
	must.NotContains(text, "Healthy", "legacy comparison mentions health")
	must.NotContains(text, "healthy", "legacy comparison mentions health")
}

func TestCompare_ReportsBothRatesAndSampleSize(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	baseline := healthSuiteResult("base", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunHealthy))
	current := healthSuiteResult("curr", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunFailed))

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary
	must.InDelta(1.0, s.CurrentPassRate, 1e-12, "judge-based without require_healthy")
	must.NotNil(s.Current.HealthyPassRate)
	must.InDelta(0.5, *s.Current.HealthyPassRate, 1e-12)
	must.Equal(eval.GateInconclusive, s.Gate, "a judge-based comparison with no change must not block")

	text := FormatText(report)
	for _, want := range []string{
		"Pass Rate: 100.0% → 100.0%",
		"Healthy Pass Rate: 100.0% → 50.0% (-50.0%)",
		"n=2 tasks, K=1, healthy 50.0%",
	} {
		check.Contains(text, want)
	}
}

func TestCompare_RequireHealthyGatesOnHealthyPasses(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	baseline := healthSuiteResult("base", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunHealthy))
	current := healthSuiteResult("curr", true,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunFailed))

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary
	must.InDelta(0.5, s.CurrentPassRate, 1e-12, "current pass rate under require_healthy")
	must.InDelta(-0.5, s.PassRateDelta, 1e-12, "pass rate delta under require_healthy")
	must.NotNil(s.Current.JudgePassRate)
	must.InDelta(1.0, *s.Current.JudgePassRate, 1e-12)
	must.True(s.Current.RequireHealthy)
	must.True(s.HasRegressions)
	must.Len(report.Regressions, 1)
	must.Equal("b", report.Regressions[0].TaskID)
	must.Equal(eval.GateBlock, s.Gate, "b passed every baseline trial and no current trial counts")

	text := FormatText(report)
	for _, want := range []string{"current counts a pass only when the run was healthy (require_healthy)", "judge-only pass rate is 100.0%"} {
		check.Contains(text, want)
	}
}

func TestWriteJUnit_RunHealthProperties(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	healthy := healthTask("ok", "pass", types.RunHealthy)
	healthy.Verification, healthy.Delivery = types.VerificationPassed, eval.DeliveryNotMeasured
	unhealthy := healthTask("flaky", "pass", types.RunFailed)
	unhealthy.Verification, unhealthy.Delivery = types.VerificationNone, eval.DeliveryNotMeasured
	result := healthSuiteResult("r", true, healthy, unhealthy)
	result.SuiteID = "s"
	result.Trials = 2

	var buf bytes.Buffer
	must.NoError(WriteJUnit(&buf, result))
	out := buf.String()
	for _, want := range []string{
		`<property name="pass_rate" value="1.000"></property>`,
		`<property name="healthy_pass_rate" value="0.500"></property>`,
		`<property name="tasks" value="2"></property>`,
		`<property name="trials_per_task" value="2"></property>`,
		`<property name="require_healthy" value="true"></property>`,
		`<property name="run_health" value="failed"></property>`,
		`<property name="verification" value="passed"></property>`,
		`<property name="delivery" value="not_measured"></property>`,
		`the judge passed but the run health was failed`,
	} {
		check.Contains(out, want)
	}
	must.NotContains(out, "<failure", "a judge pass must stay a passing testcase")
	must.NotContains(out, "<error", "a judge pass must stay a passing testcase")
	must.Equal(1, strings.Count(out, "the judge passed but the run health"), "healthy case also carries the unhealthy note")
}
