package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/spec"
	"github.com/rxbynerd/stirrup/types"
)

// TestToolUseSuite_NegativeRecordingsFailTheirJudges replays, for every
// task of the shipped tool-use suite, a recording that completes cleanly
// but misses the behaviour the task judges, through the real harness
// binary. Each task must fail its judge on a healthy run, which shows the
// judge discriminates rather than passing whatever the run did.
func TestToolUseSuite_NegativeRecordingsFailTheirJudges(t *testing.T) {
	must := require.New(t)
	if testing.Short() {
		t.Skip("skipping harness build and exec in -short mode")
	}
	bin := filepath.Join(t.TempDir(), "stirrup")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/rxbynerd/stirrup/harness/cmd/stirrup").CombinedOutput()
	must.NoError(err, "go build failed:\n%s", out)

	suite, err := spec.LoadSuiteHCL(filepath.Join("..", "suites", "tooluse.hcl"))
	must.NoError(err)
	negatives, err := filepath.Abs(filepath.Join("..", "suites", "fixtures", "tooluse", "negative"))
	must.NoError(err)
	for i := range suite.Tasks {
		task := &suite.Tasks[i]
		must.NotNil(task.RunConfigOverrides, "task %s has no overrides", task.ID)
		must.NotNil(task.RunConfigOverrides.Provider, "task %s has no provider override", task.ID)
		negative := filepath.Join(negatives, task.ID+".jsonl")
		_, statErr := os.Stat(negative)
		must.NoError(statErr, "task %s has no negative recording", task.ID)
		task.RunConfigOverrides.Provider.ReplayFile = negative
	}
	entries, err := os.ReadDir(negatives)
	must.NoError(err)
	must.Len(entries, len(suite.Tasks), "every negative recording belongs to a task")

	// The judge leg each negative recording is built to fail.
	wantFailure := map[string]string{
		"read-search-edit-loop":     "(tool-trace) failed",
		"read-before-edit":          "(tool-trace) failed",
		"line-range-read":           "(file-contains) failed",
		"bounded-search":            "(file-contains) failed",
		"invalid-arg-recovery":      "(tool-trace) failed",
		"ambiguous-edit-recovery":   "(file-contains) failed",
		"renamed-tool-recovery":     `tool "grep_files" called 0 time(s)`,
		"no-tool-answer-escalation": "(tool-trace) failed",
		"multi-tool-turn":           "(tool-trace) failed",
	}
	must.Len(wantFailure, len(suite.Tasks), "every task names the judge leg its negative recording fails")

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: bin, Concurrency: 4})
	must.NoError(err)
	must.Len(result.Tasks, len(suite.Tasks))
	for _, task := range result.Tasks {
		t.Run(task.TaskID, func(t *testing.T) {
			must := require.New(t)
			must.Equal(types.RunHealthy, task.RunHealth, "the negative run itself must complete cleanly; error %q", task.Error)
			must.Equal("fail", task.Outcome, "judge verdict: %+v", task.JudgeVerdict)
			must.Contains(task.JudgeVerdict.Reason, wantFailure[task.TaskID])
		})
	}
}
