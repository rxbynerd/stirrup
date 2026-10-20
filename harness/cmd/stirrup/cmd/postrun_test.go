package cmd

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
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

func (r *recordingExporter) Export(_ context.Context, _ executor.WorkspaceArchiver, dest string) (types.ExportReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dests = append(r.dests, dest)
	return fakeExportResult(dest, "", r.err)
}

// archiverExecutor is an executor whose workspace archive is a fixed tar
// stream, standing in for a sandbox executor. A nil stream archives an
// empty workspace; err is returned after the stream is written.
type archiverExecutor struct {
	executor.NoneExecutor
	stream []byte
	err    error
}

func (a *archiverExecutor) ArchiveWorkspace(_ context.Context, w io.Writer, _ int64) (executor.ArchiveStats, error) {
	n, err := w.Write(a.stream)
	if err != nil {
		return executor.ArchiveStats{Bytes: int64(n)}, err
	}
	return executor.ArchiveStats{Bytes: int64(n)}, a.err
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

func TestFollowUpExportURI_DerivesPerRunDestination(t *testing.T) {
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
			must := require.New(t)
			got := followUpExportURI(tc.base, tc.runID)
			must.Equal(tc.want, got, "followUpExportURI(%q, %q)", tc.base, tc.runID)
		})
	}
}

// TestPostRunPolicy_FinaliseOnlyEmits pins the per-run contract: one
// emission per completed run and no export, which the loop has already
// performed before "done".
func TestPostRunPolicy_FinaliseOnlyEmits(t *testing.T) {
	must := require.New(t)
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

	err := policy.finalise(cfg, finaliseTestTrace("primary"), nil)
	must.NoError(err, "finalise primary run")
	policy.finaliseFollowUp(cfg, finaliseTestTrace("run-2"), nil)

	must.Equal([]string{"primary", "run-2"}, emitted, "emitted run IDs")
	must.Empty(rec.destinations(), "finalise must not export outside the loop")
}

// TestPostRunPolicy_FinaliseFailedRunEmitsAndPropagates pins that a run
// error still reaches the result surface (the failure must be
// observable) and propagates.
func TestPostRunPolicy_FinaliseFailedRunEmitsAndPropagates(t *testing.T) {
	must := require.New(t)
	emitted := 0
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) { emitted++ },
	}

	runErr := errors.New("finish trace: boom")
	err := policy.finalise(&types.RunConfig{}, finaliseTestTrace("primary"), runErr)
	must.ErrorIs(err, runErr, "finalise must wrap the run error")
	must.Equal(1, emitted, "emit calls")
}

// TestPostRunPolicy_FinaliseNilTraceEmitsNothing pins that a run which
// produced no trace at all is not emitted (the loop already sent its
// terminal "done", and a second result would be a fabricated one).
func TestPostRunPolicy_FinaliseNilTraceEmitsNothing(t *testing.T) {
	must := require.New(t)
	emitted := 0
	policy := &postRunPolicy{
		emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) { emitted++ },
	}
	runErr := errors.New("finish trace: boom")

	err := policy.finalise(&types.RunConfig{}, nil, runErr)
	must.ErrorIs(err, runErr, "finalise must wrap the run error")
	must.Zero(emitted, "emit calls for a nil trace")
}

// TestCLISessionBudget_ScalesWithFollowUpWindow pins the CLI's self-imposed
// session bound: none without a follow-up window, otherwise room for ten
// follow-ups each waited for through a full grace window.
func TestCLISessionBudget_ScalesWithFollowUpWindow(t *testing.T) {
	t.Run("unbounded without a window", func(t *testing.T) {
		must := require.New(t)
		got := cliSessionBudget(30*time.Second, 0)
		must.Zero(got, "the primary run has its own timeout")
	})
	t.Run("ten follow-ups each with a full window", func(t *testing.T) {
		must := require.New(t)
		want := 10 * (30*time.Second + 60*time.Second)
		got := cliSessionBudget(30*time.Second, 60)
		must.Equal(want, got, "budget with a 60 s window")
	})
}

// TestPostRunPolicy_FollowUpErrRecordsOnlyRequiredExportFailures pins the
// exit-status contract for follow-ups: a follow-up's own run error and an
// optional export failure are already on its done/RunResult and are not
// recorded, while the first required-export failure is.
func TestPostRunPolicy_FollowUpErrRecordsOnlyRequiredExportFailures(t *testing.T) {
	must := require.New(t)
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
	must.NoError(policy.followUpErr(), "a follow-up's run error must not become the exit status")

	policy.finaliseFollowUp(cfg, withReceipt("run-3", failed), nil)
	must.NoError(policy.followUpErr(), "an optional export failure must not become the exit status")

	required := failed
	required.Required = true
	policy.finaliseFollowUp(cfg, withReceipt("run-4", required), nil)
	err := policy.followUpErr()
	must.ErrorContains(err, "upload refused")
	must.ErrorContains(err, required.URI)

	policy.finaliseFollowUp(cfg, withReceipt("run-5", types.ExportReceipt{URI: "gs://other", Status: types.ExportStatusFailed, Required: true, Error: "later"}), nil)
	must.Same(err, policy.followUpErr(), "the first required export failure must be kept")
}
