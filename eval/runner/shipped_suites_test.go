package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/eval/spec"
)

// TestShippedSuites_DryRunValidates runs --dry-run validation over every
// shipped suite, so a suite whose merged RunConfig fails ValidateRunConfig
// is caught before an operator runs it.
func TestShippedSuites_DryRunValidates(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "suites", "*.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no shipped suites found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			suite, err := spec.LoadSuiteHCL(path)
			if err != nil {
				t.Fatalf("LoadSuiteHCL: %v", err)
			}
			result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if len(result.Tasks) == 0 {
				t.Fatal("suite has no tasks")
			}
			for _, task := range result.Tasks {
				if task.Outcome != "pass" {
					t.Errorf("task %s: outcome %q, error %q", task.TaskID, task.Outcome, task.Error)
				}
			}
		})
	}
}
