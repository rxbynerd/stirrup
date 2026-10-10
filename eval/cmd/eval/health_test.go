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
	"github.com/rxbynerd/stirrup/types"
)

const failingExitHarness = `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
touch out.txt
[ -n "$TRACE" ] && echo '{"id":"run-1","turns":1,"outcome":"error"}' > "$TRACE"
exit 1
`

func writeHealthSuite(t *testing.T, attrs string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "health.hcl")
	src := "suite \"health\" {\n" + attrs + `
  task "t1" {
    prompt = "p"
    judge {
      type  = "file-exists"
      paths = ["out.txt"]
    }
  }
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runHealthSuite(t *testing.T, suitePath string, extra ...string) eval.SuiteResult {
	t.Helper()
	harnessPath := writeFakeHarness(t, failingExitHarness)
	outputDir := filepath.Join(t.TempDir(), "out")
	args := append([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir}, extra...)
	if code := run(args, io.Discard); code != 0 {
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
	return result
}

func TestCmdRun_FailedRunKeepsJudgePassAndReportsHealth(t *testing.T) {
	result := runHealthSuite(t, writeHealthSuite(t, ""))
	task := result.Tasks[0]
	if task.Outcome != "pass" || task.RunHealth != types.RunFailed {
		t.Errorf("outcome %q health %q, want pass / failed", task.Outcome, task.RunHealth)
	}
	if result.PassRate != 1 || result.HealthyPassRate == nil || *result.HealthyPassRate != 0 {
		t.Errorf("rates = %v / %v, want 1 / 0", result.PassRate, result.HealthyPassRate)
	}
	if result.RequireHealthy {
		t.Error("requireHealthy set without the flag or the attribute")
	}
}

func TestCmdRun_RequireHealthyFlagAndAttributeAreRecorded(t *testing.T) {
	if r := runHealthSuite(t, writeHealthSuite(t, ""), "--require-healthy"); !r.RequireHealthy {
		t.Error("--require-healthy not recorded in the result")
	}
	if r := runHealthSuite(t, writeHealthSuite(t, "  require_healthy = true\n")); !r.RequireHealthy {
		t.Error("the require_healthy attribute not recorded in the result")
	}
}

func TestPrintSummary_HealthyPassRate(t *testing.T) {
	rate := 0.25
	result := eval.SuiteResult{SuiteID: "s", RunID: "r", Trials: 2, PassRate: 1, HealthyPassRate: &rate, RequireHealthy: true,
		Tasks: []eval.TaskResult{{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "pass"}}}
	var b bytes.Buffer
	printSummary(&b, result, false)
	for _, want := range []string{
		"Pass rate: 100.0% (mean per-task pass fraction)",
		"Healthy pass rate: 25.0% (judge passed and run healthy; 2 tasks, 2 trials each)",
		"require_healthy: the comparison gate counts only healthy judge passes",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, b.String())
		}
	}

	result.HealthyPassRate, result.RequireHealthy = nil, false
	b.Reset()
	printSummary(&b, result, false)
	if strings.Contains(b.String(), "ealthy") {
		t.Errorf("a result without run health mentions health:\n%s", b.String())
	}
}
