package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
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
		if err := store.StoreRecording(context.Background(), rec); err != nil {
			t.Fatalf("StoreRecording %s: %v", id, err)
		}
		if err := store.StoreTrace(context.Background(), rec.FinalOutcome); err != nil {
			t.Fatalf("StoreTrace %s: %v", id, err)
		}
	}
	return dir
}

// TestSelectRecordings_ByID pins that explicit --recording selection
// preserves input order.
func TestSelectRecordings_ByID(t *testing.T) {
	dir := seedRecordings(t,
		[]string{"r1", "r2", "r3"},
		[]string{"success", "error", "max_turns"},
	)
	store, err := lakehouse.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	got, err := selectRecordings(context.Background(), store, []string{"r2", "r1"}, "")
	if err != nil {
		t.Fatalf("selectRecordings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d recordings, want 2", len(got))
	}
	if got[0].RunID != "r2" || got[1].RunID != "r1" {
		t.Errorf("order: got [%s, %s], want [r2, r1]", got[0].RunID, got[1].RunID)
	}
}

// TestSelectRecordings_MissingIDIsError pins that a missing ID is fatal,
// not silently skipped.
func TestSelectRecordings_MissingIDIsError(t *testing.T) {
	dir := seedRecordings(t,
		[]string{"r1"},
		[]string{"success"},
	)
	store, err := lakehouse.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	_, err = selectRecordings(context.Background(), store, []string{"nonexistent"}, "")
	if err == nil {
		t.Fatal("expected error for missing recording ID; got nil")
	}
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
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	got, err := selectRecordings(context.Background(), store, nil, "error")
	if err != nil {
		t.Fatalf("selectRecordings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d recordings, want 2", len(got))
	}
	for _, rec := range got {
		if rec.FinalOutcome.Outcome != "error" {
			t.Errorf("rec %q outcome = %q, want error", rec.RunID, rec.FinalOutcome.Outcome)
		}
	}
}

// TestCompletion_ReplayAndIngestFlagsRegistered pins that these
// subcommands are wired into the completion table so tab-completion
// surfaces their flags.
func TestCompletion_ReplayAndIngestFlagsRegistered(t *testing.T) {
	for _, sub := range []string{"replay", "ingest"} {
		flags, ok := evalCompletionFlags[sub]
		if !ok {
			t.Errorf("%s missing from evalCompletionFlags", sub)
			continue
		}
		if len(flags) == 0 {
			t.Errorf("%s has no completion flags", sub)
		}
	}
	for _, sub := range []string{"replay", "ingest"} {
		found := false
		for _, s := range evalCompletionSubcommands {
			if s == sub {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from evalCompletionSubcommands", sub)
		}
	}
}

// TestReplay_WorkspaceCaveat pins that with an empty --workspace flag and a
// file-exists judge referencing a concrete path, the judge fails
// informatively rather than crashing.
func TestReplay_WorkspaceCaveat(t *testing.T) {
	dir := seedRecordings(t, []string{"r1"}, []string{"success"})
	store, err := lakehouse.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	recordings, err := selectRecordings(context.Background(), store, []string{"r1"}, "")
	if err != nil {
		t.Fatalf("selectRecordings: %v", err)
	}

	task := types.EvalTask{
		ID: "task-1",
		Judge: types.EvalJudge{
			Type:  "file-exists",
			Paths: []string{"output.txt"},
		},
	}

	emptyDir := t.TempDir()
	_, err = os.Stat(filepath.Join(emptyDir, "output.txt"))
	if !os.IsNotExist(err) {
		t.Fatalf("test setup: empty workspace unexpectedly has output.txt")
	}
	res, err := runner.ReplayRecording(context.Background(), recordings[0], task, emptyDir)
	if err != nil {
		t.Fatalf("ReplayRecording: %v", err)
	}
	if res.Outcome == "pass" {
		t.Errorf("file-exists with absent file: outcome = pass, want fail")
	}
}
