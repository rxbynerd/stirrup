package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/eval/runner"
	"github.com/rxbynerd/stirrup/types"
)

// seedRecordings populates a FileStore with N recordings, returning the
// lakehouse path. Each recording carries an Outcome the outcome filter can
// target.
func seedRecordings(t *testing.T, runIDs []string, outcomes []string) string {
	t.Helper()
	dir := t.TempDir()
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = store.Close() }()
	for i, id := range runIDs {
		rec := types.RunRecording{
			RunID:  id,
			Config: types.RunConfig{RunID: id},
			FinalOutcome: types.RunTrace{
				ID:      id,
				Outcome: outcomes[i],
			},
		}
		require.NoError(t, store.StoreRecording(context.Background(), rec), "StoreRecording %s", id)
		require.NoError(t, store.StoreTrace(context.Background(), rec.FinalOutcome), "StoreTrace %s", id)
	}
	return dir
}

// TestSelectRecordings_ByIDPreservesInputOrder pins that explicit --recording
// selection preserves input order.
func TestSelectRecordings_ByIDPreservesInputOrder(t *testing.T) {
	dir := seedRecordings(t,
		[]string{"r1", "r2", "r3"},
		[]string{"success", "error", "max_turns"},
	)
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got, err := selectRecordings(context.Background(), store, []string{"r2", "r1"}, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "r2", got[0].RunID)
	require.Equal(t, "r1", got[1].RunID)
}

// TestSelectRecordings_MissingIDIsError pins that a missing ID is fatal,
// not silently skipped.
func TestSelectRecordings_MissingIDIsError(t *testing.T) {
	dir := seedRecordings(t,
		[]string{"r1"},
		[]string{"success"},
	)
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, err = selectRecordings(context.Background(), store, []string{"nonexistent"}, "")
	require.Error(t, err)
}

// TestSelectRecordings_OutcomeFilter pins that with no explicit IDs and a
// non-empty outcome filter, the returned set matches every recording whose
// Outcome equals the filter.
func TestSelectRecordings_OutcomeFilter(t *testing.T) {
	dir := seedRecordings(t,
		[]string{"r1", "r2", "r3"},
		[]string{"success", "error", "error"},
	)
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got, err := selectRecordings(context.Background(), store, nil, "error")
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, rec := range got {
		require.Equal(t, "error", rec.FinalOutcome.Outcome, "recording %s", rec.RunID)
	}
}

// TestCompletion_ReplayAndIngestFlagsRegistered pins that these
// subcommands are wired into the completion table so tab-completion
// surfaces their flags.
func TestCompletion_ReplayAndIngestFlagsRegistered(t *testing.T) {
	for _, sub := range []string{"replay", "ingest"} {
		t.Run(sub, func(t *testing.T) {
			flags, ok := evalCompletionFlags[sub]
			require.True(t, ok, "%s missing from evalCompletionFlags", sub)
			require.NotEmpty(t, flags, "%s has no completion flags", sub)
			require.Contains(t, evalCompletionSubcommands, sub)
		})
	}
}

// TestReplay_FileExistsJudgeFailsOnEmptyWorkspace pins that with an empty
// --workspace flag and a file-exists judge referencing a concrete path, the
// judge fails informatively rather than crashing.
func TestReplay_FileExistsJudgeFailsOnEmptyWorkspace(t *testing.T) {
	dir := seedRecordings(t, []string{"r1"}, []string{"success"})
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	recordings, err := selectRecordings(context.Background(), store, []string{"r1"}, "")
	require.NoError(t, err)

	task := types.EvalTask{
		ID: "task-1",
		Judge: types.EvalJudge{
			Type:  "file-exists",
			Paths: []string{"output.txt"},
		},
	}

	emptyDir := t.TempDir()
	_, err = os.Stat(filepath.Join(emptyDir, "output.txt"))
	require.ErrorIs(t, err, os.ErrNotExist, "test setup: empty workspace unexpectedly has output.txt")

	res, err := runner.ReplayRecording(context.Background(), recordings[0], task, emptyDir, judge.Options{}, "")
	require.NoError(t, err)
	require.NotEqual(t, "pass", res.Outcome, "file-exists with absent file must not pass")
}

// TestReplay_LogsErrorVerdictReason pins that a composite judge that could
// not rule, which returns no error, is still reported on stderr.
func TestReplay_LogsErrorVerdictReason(t *testing.T) {
	lakehouseDir := seedRecordings(t, []string{"r1"}, []string{"success"})
	workspace := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workspace, "dir"), 0o755))
	suitePath := filepath.Join(t.TempDir(), "suite.hcl")
	suite := `suite "s" {
  task "t" {
    prompt = "p"
    judge {
      type    = "composite"
      require = "all"
      judge {
        type    = "file-contains"
        path    = "dir"
        pattern = "x"
      }
      judge {
        type  = "file-exists"
        paths = ["dir"]
      }
    }
  }
}
`
	require.NoError(t, os.WriteFile(suitePath, []byte(suite), 0o644))
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cmdReplay([]string{"-lakehouse", lakehouseDir, "-suite", suitePath, "-workspace", workspace})

	const want = "replay r1: sub-judge 1 of 2 (file-contains) errored (require all); 1 skipped: "
	require.Contains(t, logs.String(), want)
}
