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
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	return path
}

func runHealthSuite(t *testing.T, suitePath string, extra ...string) eval.SuiteResult {
	t.Helper()
	harnessPath := writeFakeHarness(t, failingExitHarness)
	outputDir := filepath.Join(t.TempDir(), "out")
	args := append([]string{"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir}, extra...)
	require.Zero(t, run(args, io.Discard), "run() exit code")

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	require.NoError(t, err)
	var result eval.SuiteResult
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

func TestCmdRun_FailedRunKeepsJudgePassAndReportsHealth(t *testing.T) {
	result := runHealthSuite(t, writeHealthSuite(t, ""))
	task := result.Tasks[0]

	t.Run("task keeps the judge pass and reports failed health", func(t *testing.T) {
		require.Equal(t, "pass", task.Outcome)
		require.Equal(t, types.RunFailed, task.RunHealth)
	})
	t.Run("rates count judge passes and healthy passes separately", func(t *testing.T) {
		require.InDelta(t, 1.0, result.PassRate, 1e-9)
		require.NotNil(t, result.HealthyPassRate)
		require.Zero(t, *result.HealthyPassRate)
	})
	t.Run("require healthy is off without the flag or attribute", func(t *testing.T) {
		require.False(t, result.RequireHealthy)
	})
}

func TestCmdRun_RequireHealthyFlagAndAttributeAreRecorded(t *testing.T) {
	t.Run("flag is recorded in the result", func(t *testing.T) {
		result := runHealthSuite(t, writeHealthSuite(t, ""), "--require-healthy")
		require.True(t, result.RequireHealthy)
	})
	t.Run("attribute is recorded in the result", func(t *testing.T) {
		result := runHealthSuite(t, writeHealthSuite(t, "  require_healthy = true\n"))
		require.True(t, result.RequireHealthy)
	})
}

func TestPrintSummary_HealthyPassRate(t *testing.T) {
	rate := 0.25
	result := eval.SuiteResult{SuiteID: "s", RunID: "r", Trials: 2, PassRate: 1, HealthyPassRate: &rate, RequireHealthy: true,
		Tasks: []eval.TaskResult{{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "pass"}}}
	var b bytes.Buffer
	printSummary(&b, result, false)
	summary := b.String()

	tests := []struct {
		name string
		want string
	}{
		{name: "pass rate line", want: "Pass rate: 100.0% (mean per-task pass fraction)"},
		{name: "healthy pass rate line", want: "Healthy pass rate: 25.0% (judge passed and run healthy; 2 tasks, 2 trials each)"},
		{name: "require healthy note", want: "require_healthy: the comparison gate counts only healthy judge passes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, summary, tc.want)
		})
	}

	result.HealthyPassRate, result.RequireHealthy = nil, false
	b.Reset()
	printSummary(&b, result, false)
	require.NotContains(t, b.String(), "ealthy", "a result without run health should not mention health")
}
