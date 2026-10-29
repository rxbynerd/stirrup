package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
)

func trialResult(id string, outcomes ...string) eval.TaskResult {
	r := eval.TaskResult{TaskID: id}
	for i, o := range outcomes {
		r.Trials = append(r.Trials, eval.TrialResult{Trial: i + 1, Outcome: o})
	}
	c := r.Counts()
	r.Outcome = c.Outcome()
	r.PassFraction = c.PassFraction()
	return r
}

func writeResultFile(t *testing.T, dir, name string, r eval.SuiteResult) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(dir, name)
	must.NoError(writeJSON(path, r))
	return path
}

// TestCmdCompare_ExitCodeFollowsGate runs against a single-run, all-pass
// baseline over the committed dogfood-seed task IDs, so each case's gate
// does not depend on the committed baseline's measured pass fractions.
func TestCmdCompare_ExitCodeFollowsGate(t *testing.T) {
	must := require.New(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "baselines", "dogfood-seed.json"))
	must.NoError(err)
	var committed eval.SuiteResult
	must.NoError(json.Unmarshal(data, &committed))
	base := eval.SuiteResult{SuiteID: committed.SuiteID, RunID: "baseline"}
	ids := make([]string, len(committed.Tasks))
	for i, task := range committed.Tasks {
		ids[i] = task.TaskID
		base.Tasks = append(base.Tasks, eval.TaskResult{TaskID: task.TaskID, Outcome: "pass", PassFraction: 1})
	}
	baseline := writeResultFile(t, t.TempDir(), "baseline.json", base)

	build := func(outcomes func(i int) []string) eval.SuiteResult {
		r := eval.SuiteResult{SuiteID: base.SuiteID, RunID: "current", Trials: 3}
		for i, id := range ids {
			r.Tasks = append(r.Tasks, trialResult(id, outcomes(i)...))
		}
		return r
	}

	cases := []struct {
		name     string
		current  eval.SuiteResult
		extra    []string
		wantCode int
		wantGate string
		wantText []string
		notText  []string
	}{
		{
			name:     "unchanged",
			current:  build(func(int) []string { return []string{"pass", "pass", "pass"} }),
			wantCode: 0,
			wantGate: "Gate: PASS",
		},
		{
			name: "one flaky trial passes at the default margin",
			current: build(func(i int) []string {
				if i == 0 {
					return []string{"pass", "fail", "pass"}
				}
				return []string{"pass", "pass", "pass"}
			}),
			wantCode: 0,
			wantGate: "Gate: PASS",
		},
		{
			name: "warn margin flag tightens the gate to a non-blocking warning",
			current: build(func(i int) []string {
				if i == 0 {
					return []string{"pass", "fail", "pass"}
				}
				return []string{"pass", "pass", "pass"}
			}),
			extra:    []string{"--warn-margin", "0.05"},
			wantCode: 0,
			wantGate: "Gate: WARN",
		},
		{
			name: "deterministic flip blocks",
			current: build(func(i int) []string {
				if i == 1 {
					return []string{"fail", "fail", "error"}
				}
				return []string{"pass", "pass", "pass"}
			}),
			wantCode: 1,
			wantGate: "Gate: BLOCK",
		},
		{
			name: "an upper-bound block exits 1 although no task is listed as a regression",
			current: build(func(int) []string {
				return []string{"pass", "pass", "fail"}
			}),
			wantCode: 1,
			wantGate: "Gate: BLOCK",
			wantText: []string{"one-sided 95% upper bound"},
			notText:  []string{"Regressions ("},
		},
		{
			name: "a listed regression under a non-blocking gate exits 0",
			current: build(func(i int) []string {
				if i == 0 {
					return []string{"fail", "fail", "pass"}
				}
				return []string{"pass", "pass", "pass"}
			}),
			wantCode: 0,
			wantGate: "Gate: WARN",
			wantText: []string{"Regressions (1):"},
		},
		{
			name: "a baseline task missing from the current run warns without failing",
			current: func() eval.SuiteResult {
				r := build(func(int) []string { return []string{"pass", "pass", "pass"} })
				r.Tasks = r.Tasks[1:]
				return r
			}(),
			wantCode: 0,
			wantGate: "Gate: WARN",
			wantText: []string{"Missing from current run (1): " + ids[0]},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			dir := t.TempDir()
			currentPath := writeResultFile(t, dir, "current.json", tc.current)
			reportPath := filepath.Join(dir, "report.json")
			args := append([]string{"compare", "--current", currentPath, "--baseline", baseline, "--output", reportPath}, tc.extra...)

			var out bytes.Buffer
			code := run(args, &out)
			must.Equal(tc.wantCode, code, "stdout:\n%s", out.String())
			must.Contains(out.String(), tc.wantGate)
			must.Contains(out.String(), fmt.Sprintf("n=%d tasks, K=1", len(ids)), "output should report the K=1 baseline")
			for _, want := range tc.wantText {
				must.Contains(out.String(), want)
			}
			for _, unwanted := range tc.notText {
				must.NotContains(out.String(), unwanted)
			}

			raw, err := os.ReadFile(reportPath)
			must.NoError(err, "reading --output report")
			var report eval.ComparisonReport
			must.NoError(json.Unmarshal(raw, &report), "parsing --output report")
			wantSummaryGate := strings.ToLower(strings.TrimPrefix(tc.wantGate, "Gate: "))
			must.Equal(wantSummaryGate, report.Summary.Gate)
		})
	}
}

func TestCmdCompare_ErrorsExitTwo(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	baseline := writeResultFile(t, dir, "baseline.json", eval.SuiteResult{
		SuiteID: "s", RunID: "baseline",
		Tasks: []eval.TaskResult{{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "pass"}, {TaskID: "c", Outcome: "pass"}},
	})
	good := writeResultFile(t, dir, "current.json", eval.SuiteResult{
		SuiteID: "s", RunID: "current",
		Tasks: []eval.TaskResult{{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "pass"}, {TaskID: "c", Outcome: "pass"}},
	})
	corrupt := filepath.Join(dir, "corrupt.json")
	must.NoError(os.WriteFile(corrupt, []byte("{not json"), 0o644))
	missing := filepath.Join(dir, "missing.json")

	cases := []struct {
		name    string
		args    []string
		wantLog string
	}{
		{"missing current file", []string{"--current", missing, "--baseline", baseline}, "loading current result"},
		{"missing baseline file", []string{"--current", good, "--baseline", missing}, "loading baseline result"},
		{"corrupt current JSON", []string{"--current", corrupt, "--baseline", baseline}, "parsing result JSON"},
		{"corrupt baseline JSON", []string{"--current", good, "--baseline", corrupt}, "parsing result JSON"},
		{"no current flag", []string{"--baseline", baseline}, "-current is required"},
		{"no baseline flag", []string{"--current", good}, "-baseline is required"},
		{"warn margin above 1", []string{"--current", good, "--baseline", baseline, "--warn-margin", "2"}, "invalid compare options"},
		{"flip threshold of 1", []string{"--current", good, "--baseline", baseline, "--flip-threshold", "1"}, "invalid compare options"},
		{"unwritable output path", []string{"--current", good, "--baseline", baseline, "--output", filepath.Join(dir, "no", "such", "dir", "out.json")}, "writing comparison report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			var out bytes.Buffer
			code := run(append([]string{"compare"}, tc.args...), &out)
			must.Equal(2, code, "stdout: %s\nlog: %s", out.String(), logged.String())
			must.Contains(logged.String(), tc.wantLog)
			must.Empty(out.String(), "an errored compare printed a report")
		})
	}
}

func TestCmdCompare_TrialsPolicyMismatch(t *testing.T) {
	dir := t.TempDir()
	suite := func(runID, policy string) eval.SuiteResult {
		return eval.SuiteResult{
			SuiteID: "s", RunID: runID, Trials: 3, TrialsPolicy: policy,
			Tasks: []eval.TaskResult{{TaskID: "a", Outcome: "pass"}, {TaskID: "b", Outcome: "pass"}, {TaskID: "c", Outcome: "pass"}},
		}
	}
	legacy := writeResultFile(t, dir, "legacy.json", suite("legacy", ""))
	fixed := writeResultFile(t, dir, "fixed.json", suite("fixed", eval.TrialsPolicyFixed))
	confirm := writeResultFile(t, dir, "confirm.json", suite("confirm", eval.TrialsPolicyConfirmOnFail))
	unknown := writeResultFile(t, dir, "unknown.json", suite("unknown", "adaptive"))

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantLog    string
		wantReport []string
	}{
		{
			name:     "different policies are refused",
			args:     []string{"--baseline", fixed, "--current", confirm},
			wantCode: 2,
			wantLog:  "refusing to compare: trials policies differ: baseline ran fixed, current ran confirm-on-fail",
		},
		{
			name:     "a result without a policy counts as fixed",
			args:     []string{"--baseline", legacy, "--current", confirm},
			wantCode: 2,
			wantLog:  "baseline ran fixed, current ran confirm-on-fail",
		},
		{
			name:       "the escape compares with a warning",
			args:       []string{"--baseline", fixed, "--current", confirm, "--allow-policy-mismatch"},
			wantCode:   0,
			wantLog:    "warning: trials policies differ",
			wantReport: []string{"Gate: PASS", "Warning: trials policies differ (baseline fixed, current confirm-on-fail)", "first trial only"},
		},
		{
			name:       "matching policies compare silently",
			args:       []string{"--baseline", legacy, "--current", fixed},
			wantCode:   0,
			wantReport: []string{"Gate: PASS"},
		},
		{
			name:     "an unknown policy is refused even under the escape",
			args:     []string{"--baseline", fixed, "--current", unknown, "--allow-policy-mismatch"},
			wantCode: 2,
			wantLog:  `current result: unknown trials policy "adaptive"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			var out bytes.Buffer
			code := run(append([]string{"compare"}, tc.args...), &out)
			must.Equal(tc.wantCode, code, "stdout: %s\nlog: %s", out.String(), logged.String())
			if tc.wantLog == "" {
				must.Empty(logged.String())
			} else {
				must.Contains(logged.String(), tc.wantLog)
			}
			if tc.wantCode == 2 {
				must.Empty(out.String(), "a refused compare printed a report")
			}
			for _, want := range tc.wantReport {
				must.Contains(out.String(), want)
			}
		})
	}
}
