package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
)

const traceWritingHarness = `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
`

func TestCmdRun_TrialsFlagOverridesSuiteAttribute(t *testing.T) {
	must := require.New(t)
	harnessPath := writeFakeHarness(t, traceWritingHarness)
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "trials.hcl")
	src := `
suite "trials-suite" {
  trials = 2

  task "task-a" {
    prompt = "do task a"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	must.NoError(os.WriteFile(suitePath, []byte(src), 0o644))

	outputDir := filepath.Join(dir, "out")
	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir, "--trials", "3"}, io.Discard)
	must.Equal(0, code)

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	var result eval.SuiteResult
	must.NoError(json.Unmarshal(data, &result))
	must.Equal(3, result.Trials, "--trials overrides the suite attribute")
	must.Len(result.Tasks[0].Trials, 3)

	for _, trial := range []string{"trial-1", "trial-2", "trial-3"} {
		t.Run(trial, func(t *testing.T) {
			must := require.New(t)
			_, err := os.Stat(filepath.Join(outputDir, "trials-suite", "task-a", trial, "trace.jsonl"))
			must.NoError(err, "per-trial artifact")
		})
	}
}

func TestPrintSummary_ReportsTrialsAndDryRun(t *testing.T) {
	single := eval.SuiteResult{SuiteID: "s", RunID: "r", Trials: 1, PassRate: 0.5, Tasks: []eval.TaskResult{
		{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "fail"},
	}}
	multi := single
	multi.Trials = 3

	t.Run("single trial", func(t *testing.T) {
		must := require.New(t)
		var b bytes.Buffer
		printSummary(&b, single, false)
		must.Equal("Suite: s (run: r)\nTasks: 2 total, 1 passed, 1 failed, 0 errors\nPass rate: 50.0%\n", b.String())
	})

	t.Run("multiple trials", func(t *testing.T) {
		must := require.New(t)
		var b bytes.Buffer
		printSummary(&b, multi, false)
		must.Contains(b.String(), "Trials: 3 per task (6 harness runs)")
		must.Contains(b.String(), "(majority of trials)")
		must.Contains(b.String(), "Pass rate: 50.0% (mean per-task pass fraction)")
	})

	t.Run("dry run", func(t *testing.T) {
		must := require.New(t)
		var b bytes.Buffer
		printSummary(&b, multi, true)
		must.Contains(b.String(), "6 harness runs planned; the dry run validates each task once")
		must.NotContains(b.String(), "majority of trials", "dry-run summary describes majority outcomes although each task ran once")
	})

	confirm := eval.SuiteResult{SuiteID: "s", RunID: "r", Trials: 3, TrialsPolicy: eval.TrialsPolicyConfirmOnFail, Tasks: []eval.TaskResult{
		{TaskID: "a", Outcome: "pass"},
		{TaskID: "b", Outcome: "fail", Trials: []eval.TrialResult{{Trial: 1, Outcome: "fail"}, {Trial: 2, Outcome: "fail"}, {Trial: 3, Outcome: "pass"}}},
	}}
	healthy := 0.5
	confirm.HealthyPassRate = &healthy

	t.Run("confirm-on-fail counts the runs made", func(t *testing.T) {
		must := require.New(t)
		var b bytes.Buffer
		printSummary(&b, confirm, false)
		must.Contains(b.String(), "Trials: up to 3 per task, confirm-on-fail (4 harness runs; 1 of 2 tasks rerun after a first trial that did not pass)")
		must.Contains(b.String(), "(majority of trials)")
		must.Contains(b.String(), "Healthy pass rate: 50.0% (judge passed and run healthy; 2 tasks, up to 3 trials each)")
	})

	t.Run("confirm-on-fail dry run states the range", func(t *testing.T) {
		must := require.New(t)
		var b bytes.Buffer
		printSummary(&b, confirm, true)
		must.Contains(b.String(), "Trials: up to 3 per task, confirm-on-fail (2 to 6 harness runs planned; the dry run validates each task once)")
	})
}

func TestCmdRun_TrialsPolicyFlagIsRecordedInResult(t *testing.T) {
	must := require.New(t)
	harnessPath := writeFakeHarness(t, traceWritingHarness)
	suitePath := writeTwoTaskSuite(t, "")
	outputDir := filepath.Join(t.TempDir(), "out")

	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir, "--trials", "3", "--trials-policy", "confirm-on-fail"}, io.Discard)
	must.Equal(0, code)

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	var result eval.SuiteResult
	must.NoError(json.Unmarshal(data, &result))
	must.Equal(eval.TrialsPolicyConfirmOnFail, result.TrialsPolicy)
	must.Equal(3, result.Trials)
	for _, task := range result.Tasks {
		must.Empty(task.Trials, "task %s passed its first trial and was not rerun", task.TaskID)
	}
}

func TestCmdRun_SingleRunResultAlwaysCarriesTrialsAndPassFraction(t *testing.T) {
	must := require.New(t)
	harnessPath := writeFakeHarness(t, traceWritingHarness)
	suitePath := writeTwoTaskSuite(t, "")
	outputDir := filepath.Join(t.TempDir(), "out")

	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir}, io.Discard)
	must.Equal(0, code)

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	var doc struct {
		Trials int `json:"trials"`
		Tasks  []map[string]any
	}
	must.NoError(json.Unmarshal(data, &doc))
	must.Equal(1, doc.Trials, "suite trials in %s", data)
	must.Len(doc.Tasks, 2)
	for _, task := range doc.Tasks {
		got, ok := task["passFraction"]
		must.True(ok, "task %v: passFraction is missing", task["taskId"])
		must.InDelta(1.0, got, 1e-9, "task %v: passFraction", task["taskId"])
		must.NotContains(task, "trials", "task %v: single-run result lists per-trial entries", task["taskId"])
	}
}
