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
	require.NoError(t, os.WriteFile(suitePath, []byte(src), 0o644))

	outputDir := filepath.Join(dir, "out")
	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir, "--trials", "3"}, io.Discard)
	require.Equal(t, 0, code)

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	require.NoError(t, err)
	var result eval.SuiteResult
	require.NoError(t, json.Unmarshal(data, &result))
	require.Equal(t, 3, result.Trials, "--trials overrides the suite attribute")
	require.Len(t, result.Tasks[0].Trials, 3)

	for _, trial := range []string{"trial-1", "trial-2", "trial-3"} {
		t.Run(trial, func(t *testing.T) {
			_, err := os.Stat(filepath.Join(outputDir, "trials-suite", "task-a", trial, "trace.jsonl"))
			require.NoError(t, err, "per-trial artifact")
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
		var b bytes.Buffer
		printSummary(&b, single, false)
		require.Equal(t, "Suite: s (run: r)\nTasks: 2 total, 1 passed, 1 failed, 0 errors\nPass rate: 50.0%\n", b.String())
	})

	t.Run("multiple trials", func(t *testing.T) {
		var b bytes.Buffer
		printSummary(&b, multi, false)
		require.Contains(t, b.String(), "Trials: 3 per task (6 harness runs)")
		require.Contains(t, b.String(), "(majority of trials)")
		require.Contains(t, b.String(), "Pass rate: 50.0% (mean per-task pass fraction)")
	})

	t.Run("dry run", func(t *testing.T) {
		var b bytes.Buffer
		printSummary(&b, multi, true)
		require.Contains(t, b.String(), "6 harness runs planned; the dry run validates each task once")
		require.NotContains(t, b.String(), "majority of trials", "dry-run summary describes majority outcomes although each task ran once")
	})
}

func TestCmdRun_SingleRunResultAlwaysCarriesTrialsAndPassFraction(t *testing.T) {
	harnessPath := writeFakeHarness(t, traceWritingHarness)
	suitePath := writeTwoTaskSuite(t, "")
	outputDir := filepath.Join(t.TempDir(), "out")

	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir}, io.Discard)
	require.Equal(t, 0, code)

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	require.NoError(t, err)
	var doc struct {
		Trials int `json:"trials"`
		Tasks  []map[string]any
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	require.Equal(t, 1, doc.Trials, "suite trials in %s", data)
	require.Len(t, doc.Tasks, 2)
	for _, task := range doc.Tasks {
		got, ok := task["passFraction"]
		require.True(t, ok, "task %v: passFraction is missing", task["taskId"])
		require.InDelta(t, 1.0, got, 1e-9, "task %v: passFraction", task["taskId"])
		require.NotContains(t, task, "trials", "task %v: single-run result lists per-trial entries", task["taskId"])
	}
}
