package reporter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func healthTask(id, outcome string, health types.RunHealth) eval.TaskResult {
	return eval.TaskResult{TaskID: id, Outcome: outcome, RunHealth: health, PassFraction: map[string]float64{"pass": 1}[outcome]}
}

func healthSuiteResult(runID string, require bool, tasks ...eval.TaskResult) eval.SuiteResult {
	return eval.SuiteResult{
		RunID:           runID,
		Tasks:           tasks,
		PassRate:        meanFraction(tasks),
		HealthyPassRate: eval.HealthyPassRate(tasks),
		RequireHealthy:  require,
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
	legacy := `{"suiteId":"s","runId":"%s","tasks":[
{"taskId":"a","outcome":"pass","judgeVerdict":{"passed":true,"reason":""},"durationMs":1,"passFraction":1},
{"taskId":"b","outcome":"fail","judgeVerdict":{"passed":false,"reason":""},"durationMs":1,"passFraction":0}],"passRate":0.5}`
	decode := func(id string) eval.SuiteResult {
		var r eval.SuiteResult
		if err := json.Unmarshal([]byte(strings.Replace(legacy, "%s", id, 1)), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	report := Compare(decode("base"), decode("curr"), DefaultOptions())
	s := report.Summary
	if s.BaselinePassRate != 0.5 || s.CurrentPassRate != 0.5 || s.PassRateDelta != 0 {
		t.Errorf("legacy rates = %v / %v / %v, want 0.5 / 0.5 / 0", s.BaselinePassRate, s.CurrentPassRate, s.PassRateDelta)
	}
	if s.Baseline.HealthyPassRate != nil || s.Current.HealthyPassRate != nil {
		t.Error("a legacy result reports a healthy pass rate")
	}
	text := FormatText(report)
	if strings.Contains(text, "Healthy") || strings.Contains(text, "healthy") {
		t.Errorf("legacy comparison mentions health:\n%s", text)
	}
}

func TestCompare_ReportsBothRatesAndSampleSize(t *testing.T) {
	baseline := healthSuiteResult("base", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunHealthy))
	current := healthSuiteResult("curr", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunFailed))

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary
	if s.CurrentPassRate != 1 {
		t.Errorf("current pass rate = %v, want 1 (judge-based without require_healthy)", s.CurrentPassRate)
	}
	if s.Current.HealthyPassRate == nil || *s.Current.HealthyPassRate != 0.5 {
		t.Errorf("current healthy pass rate = %v, want 0.5", s.Current.HealthyPassRate)
	}
	if s.Gate != eval.GateInconclusive {
		t.Errorf("gate = %q; a judge-based comparison with no change must not block", s.Gate)
	}

	text := FormatText(report)
	for _, want := range []string{
		"Pass Rate: 100.0% → 100.0%",
		"Healthy Pass Rate: 100.0% → 50.0% (-50.0%)",
		"n=2 tasks, K=1, healthy 50.0%",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

func TestCompare_RequireHealthyGatesOnHealthyPasses(t *testing.T) {
	baseline := healthSuiteResult("base", false,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunHealthy))
	current := healthSuiteResult("curr", true,
		healthTask("a", "pass", types.RunHealthy), healthTask("b", "pass", types.RunFailed))

	report := Compare(baseline, current, DefaultOptions())
	s := report.Summary
	if s.CurrentPassRate != 0.5 || s.PassRateDelta != -0.5 {
		t.Errorf("current pass rate %v delta %v, want 0.5 / -0.5 under require_healthy", s.CurrentPassRate, s.PassRateDelta)
	}
	if s.Current.JudgePassRate == nil || *s.Current.JudgePassRate != 1 || !s.Current.RequireHealthy {
		t.Errorf("current judge pass rate %v require %v, want 1 / true", s.Current.JudgePassRate, s.Current.RequireHealthy)
	}
	if !s.HasRegressions || len(report.Regressions) != 1 || report.Regressions[0].TaskID != "b" {
		t.Errorf("regressions = %+v, want task b regressed", report.Regressions)
	}
	if s.Gate != eval.GateBlock {
		t.Errorf("gate = %q, want block: b passed every baseline trial and no current trial counts", s.Gate)
	}

	text := FormatText(report)
	for _, want := range []string{"current counts a pass only when the run was healthy (require_healthy)", "judge-only pass rate is 100.0%"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

func TestWriteJUnit_RunHealthProperties(t *testing.T) {
	healthy := healthTask("ok", "pass", types.RunHealthy)
	healthy.Verification, healthy.Delivery = types.VerificationPassed, eval.DeliveryNotMeasured
	unhealthy := healthTask("flaky", "pass", types.RunFailed)
	unhealthy.Verification, unhealthy.Delivery = types.VerificationNone, eval.DeliveryNotMeasured
	result := healthSuiteResult("r", true, healthy, unhealthy)
	result.SuiteID = "s"
	result.Trials = 2

	var buf bytes.Buffer
	if err := WriteJUnit(&buf, result); err != nil {
		t.Fatal(err)
	}
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
		if !strings.Contains(out, want) {
			t.Errorf("JUnit missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<failure") || strings.Contains(out, "<error") {
		t.Errorf("a judge pass must stay a passing testcase:\n%s", out)
	}
	if got := strings.Count(out, "the judge passed but the run health"); got != 1 {
		t.Errorf("healthy case also carries the unhealthy note (%d notes)", got)
	}
}
