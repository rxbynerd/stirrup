package cmd

import (
	"context"
	"errors"
	"strings"
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

func (r *recordingExporter) Export(_ context.Context, _, dest string) (types.ExportReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dests = append(r.dests, dest)
	return fakeExportResult(dest, "", r.err)
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

// TestPostRunPolicy_FinaliseOnlyEmits pins the per-run contract: one
// emission per completed run and no export, which the loop has already
// performed before "done".
func TestPostRunPolicy_FinaliseOnlyEmits(t *testing.T) {
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)

	var emitted []string
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, rt *types.RunTrace) {
			emitted = append(emitted, rt.ID)
		},
	}
	cfg := &types.RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"

	if err := policy.finalise(cfg, finaliseTestTrace("primary"), nil); err != nil {
		t.Fatalf("primary finalise: %v", err)
	}
	policy.finaliseFollowUp(cfg, finaliseTestTrace("run-2"), nil)

	if want := []string{"primary", "run-2"}; !equalStrings(emitted, want) {
		t.Errorf("emitted run IDs = %v, want %v", emitted, want)
	}
	if got := rec.destinations(); len(got) != 0 {
		t.Errorf("finalise exported to %v, want no export outside the loop", got)
	}
}

// TestPostRunPolicy_FinaliseFailedRunEmitsAndPropagates pins that a run
// error still reaches the result surface (the failure must be
// observable) and propagates.
func TestPostRunPolicy_FinaliseFailedRunEmitsAndPropagates(t *testing.T) {
	emitted := 0
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) { emitted++ },
	}

	runErr := errors.New("finish trace: boom")
	err := policy.finalise(&types.RunConfig{}, finaliseTestTrace("primary"), runErr)
	if !errors.Is(err, runErr) {
		t.Fatalf("finalise error = %v, want it to wrap the run error", err)
	}
	if emitted != 1 {
		t.Errorf("emit called %d times, want 1", emitted)
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
	if err := policy.finalise(&types.RunConfig{}, nil, runErr); !errors.Is(err, runErr) {
		t.Fatalf("finalise error = %v, want it to wrap the run error", err)
	}
	if emitted != 0 {
		t.Errorf("emit called %d times for a nil trace, want 0", emitted)
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
// exit-status contract for follow-ups: a follow-up's own run error and an
// optional export failure are already on its done/RunResult and are not
// recorded, while the first required-export failure is.
func TestPostRunPolicy_FollowUpErrRecordsOnlyRequiredExportFailures(t *testing.T) {
	noop := func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) {}
	withReceipt := func(id string, receipt types.ExportReceipt) *types.RunTrace {
		rt := finaliseTestTrace(id)
		rt.WorkspaceExport = &receipt
		return rt
	}
	cfg := &types.RunConfig{RunID: "run-2"}
	failed := types.ExportReceipt{URI: "gs://bucket/runs/primary/run-3/workspace.tar.gz", Status: types.ExportStatusFailed, Error: "upload refused"}

	policy := &postRunPolicy{emit: noop}
	policy.finaliseFollowUp(cfg, finaliseTestTrace("run-2"), errors.New("finish trace: boom"))
	if err := policy.followUpErr(); err != nil {
		t.Fatalf("a follow-up's run error was recorded as the exit status: %v", err)
	}
	policy.finaliseFollowUp(cfg, withReceipt("run-3", failed), nil)
	if err := policy.followUpErr(); err != nil {
		t.Fatalf("an optional export failure was recorded as the exit status: %v", err)
	}
	required := failed
	required.Required = true
	policy.finaliseFollowUp(cfg, withReceipt("run-4", required), nil)
	err := policy.followUpErr()
	if err == nil || !strings.Contains(err.Error(), "upload refused") || !strings.Contains(err.Error(), required.URI) {
		t.Fatalf("followUpErr() = %v, want the required export failure", err)
	}
	policy.finaliseFollowUp(cfg, withReceipt("run-5", types.ExportReceipt{URI: "gs://other", Status: types.ExportStatusFailed, Required: true, Error: "later"}), nil)
	if got := policy.followUpErr(); got != err {
		t.Errorf("followUpErr() = %v, want the first failure kept", got)
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
