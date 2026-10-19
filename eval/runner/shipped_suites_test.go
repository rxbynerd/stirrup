package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/spec"
)

// TestShippedSuites_DryRunValidates runs --dry-run validation over every
// shipped suite, so a suite whose merged RunConfig fails ValidateRunConfig
// is caught before an operator runs it.
func TestShippedSuites_DryRunValidates(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "suites", "*.hcl"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no shipped suites found")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			suite, err := spec.LoadSuiteHCL(path)
			require.NoError(t, err, "LoadSuiteHCL")
			result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
			require.NoError(t, err, "RunSuite")
			require.NotEmpty(t, result.Tasks, "suite has no tasks")
			for _, task := range result.Tasks {
				t.Run(task.TaskID, func(t *testing.T) {
					require.Equal(t, "pass", task.Outcome, "task error %q", task.Error)
				})
			}
		})
	}
}
