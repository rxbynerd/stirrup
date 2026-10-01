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
			for _, want := range tc.wantText {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
			for _, unwanted := range tc.notText {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("output contains %q:\n%s", unwanted, out.String())
				}
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

func TestCmdCompare_ErrorsExitTwo(t *testing.T) {
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
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
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
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			var out bytes.Buffer
			if code := run(append([]string{"compare"}, tc.args...), &out); code != 2 {
				t.Errorf("exit code = %d, want 2\nstdout: %s\nlog: %s", code, out.String(), logged.String())
			}
			if !strings.Contains(logged.String(), tc.wantLog) {
				t.Errorf("log missing %q:\n%s", tc.wantLog, logged.String())
			}
			if out.Len() != 0 {
				t.Errorf("an errored compare printed a report:\n%s", out.String())
			}
		})
	}
}
