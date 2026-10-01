package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	path := filepath.Join(dir, name)
	if err := writeJSON(path, r); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCmdCompare_ExitCodeFollowsGate(t *testing.T) {
	baseline := filepath.Join("..", "..", "baselines", "dogfood-seed.json")
	data, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	var base eval.SuiteResult
	if err := json.Unmarshal(data, &base); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(base.Tasks))
	for i, task := range base.Tasks {
		ids[i] = task.TaskID
	}

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			currentPath := writeResultFile(t, dir, "current.json", tc.current)
			reportPath := filepath.Join(dir, "report.json")
			args := append([]string{"compare", "--current", currentPath, "--baseline", baseline, "--output", reportPath}, tc.extra...)

			var out bytes.Buffer
			if code := run(args, &out); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", code, tc.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tc.wantGate) {
				t.Errorf("output missing %q:\n%s", tc.wantGate, out.String())
			}
			if !strings.Contains(out.String(), fmt.Sprintf("n=%d tasks, K=1", len(ids))) {
				t.Errorf("output should report the K=1 baseline:\n%s", out.String())
			}

			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatalf("reading --output report: %v", err)
			}
			var report eval.ComparisonReport
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatalf("parsing --output report: %v", err)
			}
			if want := strings.ToLower(strings.TrimPrefix(tc.wantGate, "Gate: ")); report.Summary.Gate != want {
				t.Errorf("report gate = %q, want %q", report.Summary.Gate, want)
			}
		})
	}
}
