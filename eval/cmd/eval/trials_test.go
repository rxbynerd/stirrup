package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestCmdRun_TrialsFlag(t *testing.T) {
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
	if err := os.WriteFile(suitePath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(dir, "out")
	code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir, "--trials", "3"}, io.Discard)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result eval.SuiteResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Trials != 3 {
		t.Errorf("result trials = %d, want 3 (--trials overrides the suite attribute)", result.Trials)
	}
	if got := len(result.Tasks[0].Trials); got != 3 {
		t.Errorf("task trials = %d, want 3", got)
	}
	for _, trial := range []string{"trial-1", "trial-2", "trial-3"} {
		if _, err := os.Stat(filepath.Join(outputDir, "trials-suite", "task-a", trial, "trace.jsonl")); err != nil {
			t.Errorf("missing per-trial artifact: %v", err)
		}
	}
}

func TestPrintSummary(t *testing.T) {
	single := eval.SuiteResult{SuiteID: "s", RunID: "r", Trials: 1, PassRate: 0.5, Tasks: []eval.TaskResult{
		{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "fail"},
	}}
	var b bytes.Buffer
	printSummary(&b, single, false)
	want := "Suite: s (run: r)\nTasks: 2 total, 1 passed, 1 failed, 0 errors\nPass rate: 50.0%\n"
	if b.String() != want {
		t.Errorf("single-trial summary =\n%s\nwant\n%s", b.String(), want)
	}

	multi := single
	multi.Trials = 3
	b.Reset()
	printSummary(&b, multi, false)
	for _, s := range []string{"Trials: 3 per task (6 harness runs)", "(majority of trials)", "Pass rate: 50.0% (mean per-task pass fraction)"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("multi-trial summary missing %q:\n%s", s, b.String())
		}
	}

	b.Reset()
	printSummary(&b, multi, true)
	if !strings.Contains(b.String(), "6 harness runs planned; the dry run validates each task once") {
		t.Errorf("dry-run summary should state the planned runs:\n%s", b.String())
	}
	if strings.Contains(b.String(), "majority of trials") {
		t.Errorf("dry-run summary describes majority outcomes although each task ran once:\n%s", b.String())
	}
}

func TestCmdRun_SingleRunResultAlwaysCarriesTrialsAndPassFraction(t *testing.T) {
	harnessPath := writeFakeHarness(t, traceWritingHarness)
	suitePath := writeTwoTaskSuite(t, "")
	outputDir := filepath.Join(t.TempDir(), "out")

	if code := run([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir}, io.Discard); code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Trials int `json:"trials"`
		Tasks  []map[string]any
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Trials != 1 {
		t.Errorf("suite trials = %d, want 1 in %s", doc.Trials, data)
	}
	if len(doc.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(doc.Tasks))
	}
	for _, task := range doc.Tasks {
		if got, ok := task["passFraction"]; !ok || got != 1.0 {
			t.Errorf("task %v: passFraction = %v (present %v), want 1", task["taskId"], got, ok)
		}
		if _, ok := task["trials"]; ok {
			t.Errorf("task %v: single-run result lists per-trial entries", task["taskId"])
		}
	}
}
