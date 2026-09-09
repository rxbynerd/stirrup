package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// recordingExporter captures every Export destination so tests can
// assert per-run object paths.
type recordingExporter struct {
	mu    sync.Mutex
	dests []string
	err   error
}

func (r *recordingExporter) Export(_ context.Context, _, dest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dests = append(r.dests, dest)
	return r.err
}

func (r *recordingExporter) destinations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dests...)
}

func installRecordingExporter(t *testing.T, rec *recordingExporter) {
	t.Helper()
	prev := newWorkspaceExporter
	t.Cleanup(func() { newWorkspaceExporter = prev })
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) { return rec, nil }
}

func finaliseTestTrace(id string) *types.RunTrace {
	now := time.Now()
	return &types.RunTrace{ID: id, Outcome: "success", StartedAt: now, CompletedAt: now}
}

func TestFollowUpExportURI(t *testing.T) {
	cases := []struct {
		name, base, runID, want string
	}{
		{"nested object", "gs://bucket/runs/r1/workspace.tar.gz", "run-2", "gs://bucket/runs/r1/run-2/workspace.tar.gz"},
		{"object at bucket root", "gs://bucket/workspace.tar.gz", "run-2", "gs://bucket/run-2/workspace.tar.gz"},
		{"bucket only", "gs://bucket", "run-2", "gs://bucket/run-2"},
		{"trailing slash", "gs://bucket/runs/", "run-2", "gs://bucket/runs/run-2"},
		{"bucket with trailing slash", "gs://bucket/", "run-2", "gs://bucket/run-2"},
		{"empty base stays disabled", "", "run-2", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := followUpExportURI(tc.base, tc.runID); got != tc.want {
				t.Errorf("followUpExportURI(%q, %q) = %q, want %q", tc.base, tc.runID, got, tc.want)
			}
		})
	}
}

// TestPostRunPolicy_FinaliseEmitsThenExports pins the per-run
// contract: one emission and one export per completed run, with the
// export going to the destination the caller selected for that run.
func TestPostRunPolicy_FinaliseEmitsThenExports(t *testing.T) {
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)

	var emitted []string
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, rt *types.RunTrace) {
			emitted = append(emitted, rt.ID)
		},
	}
	cfg := &types.RunConfig{}
	cfg.Executor.Workspace = t.TempDir()
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"

	if err := policy.finalise(cfg, finaliseTestTrace("primary"), nil, cfg.Executor.WorkspaceExportTo); err != nil {
		t.Fatalf("primary finalise: %v", err)
	}
	cfg.RunID = "run-2"
	if err := policy.finalise(cfg, finaliseTestTrace("run-2"), nil, followUpExportURI(cfg.Executor.WorkspaceExportTo, cfg.RunID)); err != nil {
		t.Fatalf("follow-up finalise: %v", err)
	}

	if want := []string{"primary", "run-2"}; !equalStrings(emitted, want) {
		t.Errorf("emitted run IDs = %v, want %v", emitted, want)
	}
	want := []string{
		"gs://bucket/runs/primary/workspace.tar.gz",
		"gs://bucket/runs/primary/run-2/workspace.tar.gz",
	}
	if got := rec.destinations(); !equalStrings(got, want) {
		t.Errorf("export destinations = %v, want %v", got, want)
	}
}

// TestPostRunPolicy_FinaliseFailedRunEmitsButSkipsExport pins that a
// run error still reaches the result surface (the failure must be
// observable) while the export — which has nothing meaningful to ship
// for a run that never completed — is skipped, and the error propagates.
func TestPostRunPolicy_FinaliseFailedRunEmitsButSkipsExport(t *testing.T) {
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)

	emitted := 0
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) { emitted++ },
	}
	cfg := &types.RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"

	runErr := errors.New("git setup: boom")
	err := policy.finalise(cfg, finaliseTestTrace("primary"), runErr, cfg.Executor.WorkspaceExportTo)
	if !errors.Is(err, runErr) {
		t.Fatalf("finalise error = %v, want it to wrap the run error", err)
	}
	if emitted != 1 {
		t.Errorf("emit called %d times, want 1", emitted)
	}
	if got := rec.destinations(); len(got) != 0 {
		t.Errorf("export ran for a failed run: %v", got)
	}
}

// TestPostRunPolicy_FinaliseNilTraceEmitsNothing pins that a run which
// produced no trace at all is not emitted (the loop already sent its
// terminal "done", and a second result would be a fabricated one).
func TestPostRunPolicy_FinaliseNilTraceEmitsNothing(t *testing.T) {
	emitted := 0
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) { emitted++ },
	}
	runErr := errors.New("finish trace: boom")
	if err := policy.finalise(&types.RunConfig{}, nil, runErr, ""); !errors.Is(err, runErr) {
		t.Fatalf("finalise error = %v, want it to wrap the run error", err)
	}
	if emitted != 0 {
		t.Errorf("emit called %d times for a nil trace, want 0", emitted)
	}
}

// TestPostRunPolicy_FinaliseRequiredExportFailurePropagates pins that
// the required/soft-fail export policy is the same for every run the
// policy finalises.
func TestPostRunPolicy_FinaliseRequiredExportFailurePropagates(t *testing.T) {
	sentinel := errors.New("simulated GCS upload failure")
	rec := &recordingExporter{err: sentinel}
	installRecordingExporter(t, rec)

	cfg := &types.RunConfig{}
	cfg.Executor.Workspace = t.TempDir()
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"
	noop := func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) {}

	required := &postRunPolicy{emit: noop, exportRequired: true}
	if err := required.finalise(cfg, finaliseTestTrace("run-2"), nil, followUpExportURI(cfg.Executor.WorkspaceExportTo, "run-2")); !errors.Is(err, sentinel) {
		t.Errorf("required: error = %v, want the export failure", err)
	}

	optional := &postRunPolicy{emit: noop, exportRequired: false}
	if err := optional.finalise(cfg, finaliseTestTrace("run-3"), nil, followUpExportURI(cfg.Executor.WorkspaceExportTo, "run-3")); err != nil {
		t.Errorf("optional: error = %v, want nil", err)
	}
}

// TestCLISessionBudget pins the CLI's self-imposed session bound: none
// without a follow-up window, otherwise room for ten follow-ups each
// waited for through a full grace window.
func TestCLISessionBudget(t *testing.T) {
	if got := cliSessionBudget(30*time.Second, 0); got != 0 {
		t.Errorf("budget without a window = %v, want 0 (unbounded; the primary run has its own timeout)", got)
	}
	if got, want := cliSessionBudget(30*time.Second, 60), 10*(30*time.Second+60*time.Second); got != want {
		t.Errorf("budget with a 60 s window = %v, want %v", got, want)
	}
}

// TestPostRunPolicy_FollowUpErrRecordsOnlyRequiredExportFailures pins the
// exit-status contract for follow-ups: a follow-up's own run error is
// already on its done/RunResult and is not recorded, while the first
// required-export failure is.
func TestPostRunPolicy_FollowUpErrRecordsOnlyRequiredExportFailures(t *testing.T) {
	sentinel := errors.New("simulated GCS upload failure")
	rec := &recordingExporter{err: sentinel}
	installRecordingExporter(t, rec)
	noop := func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) {}
	cfg := &types.RunConfig{RunID: "run-2"}
	cfg.Executor.Workspace = t.TempDir()
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"

	policy := &postRunPolicy{emit: noop, exportRequired: true}
	policy.finaliseFollowUp(cfg, finaliseTestTrace("run-2"), errors.New("git setup: boom"))
	if err := policy.followUpErr(); err != nil {
		t.Fatalf("a follow-up's run error was recorded as the exit status: %v", err)
	}
	policy.finaliseFollowUp(cfg, finaliseTestTrace("run-3"), nil)
	if err := policy.followUpErr(); !errors.Is(err, sentinel) {
		t.Fatalf("required-export failure not recorded: %v", err)
	}

	optional := &postRunPolicy{emit: noop}
	optional.finaliseFollowUp(cfg, finaliseTestTrace("run-4"), nil)
	if err := optional.followUpErr(); err != nil {
		t.Fatalf("soft-fail export recorded as the exit status: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
