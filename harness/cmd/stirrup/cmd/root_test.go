package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// TestRootCmd_Version pins the format of the harness `--version` output.
// The wiring in root.go (`rootCmd.Version = version.Full()`) is exercised
// here so a refactor that drops or rewrites the link-time version plumbing
// fails this test rather than silently shipping a binary whose --version
// flag prints "stirrup " or panics.
//
// The eval CLI has the equivalent guard in TestRun_Version
// (eval/cmd/eval/main_test.go); this test is its harness counterpart.
func TestRootCmd_Version(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"--version"})
	defer func() {
		// Reset shared-state mutations on the package-level rootCmd so
		// later tests in this package see a clean command. SetArgs(nil)
		// restores os.Args-based parsing; SetOut(nil) restores the cobra
		// default writer.
		rootCmd.SetOut(nil)
		rootCmd.SetArgs(nil)
	}()

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute() returned error: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "stirrup version ") {
		t.Fatalf("unexpected --version output: %q (want prefix %q)", out, "stirrup version ")
	}
	// Default link-time version when no -ldflags injected is "dev".
	if !strings.Contains(out, "dev") {
		t.Fatalf("--version output %q should include the default link-time version %q", out, "dev")
	}
}

// TestRootCmd_BareInvocationPrintsHint pins issue #249's root behaviour:
// a bare `stirrup` (no subcommand, no --help / --version) prints the
// short two-subcommand orientation hint to stdout and exits 0 — not
// Cobra's full usage block. The hint is plain text: no ANSI so it reads
// identically in a terminal, a pager, or a captured file.
func TestRootCmd_BareInvocationPrintsHint(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{})
	// SetArgs does not clear flag values parsed by a prior Execute() on
	// the shared package-level rootCmd. TestRootCmd_Version leaves the
	// auto-registered --version bool marked Changed=true, and Cobra
	// short-circuits to the version output before Run fires. Reset it so
	// this test observes the bare-invocation Run path regardless of
	// execution order.
	if vf := rootCmd.Flags().Lookup("version"); vf != nil {
		_ = vf.Value.Set("false")
		vf.Changed = false
	}
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetArgs(nil)
	}()

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute() returned error: %v (a bare invocation must exit 0)", err)
	}

	out := buf.String()
	for _, want := range []string{
		"stirrup — a coding agent harness",
		"stirrup harness --prompt",
		"stirrup job",
		"stirrup harness --help",
		"stirrup --version",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bare-stirrup hint missing %q\n--- output ---\n%s", want, out)
		}
	}
	// The hint must be plain text — no boxes, no headings beyond the
	// title, and crucially no ANSI escapes.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("bare-stirrup hint must not contain ANSI escapes\n--- output ---\n%s", out)
	}
	// A regression that printed Cobra's full help instead of the hint
	// would surface its "Available Commands:" / "Flags:" sections.
	if strings.Contains(out, "Available Commands:") || strings.Contains(out, "Use \"stirrup [command] --help\"") {
		t.Errorf("bare-stirrup printed Cobra's full help, want the terse hint\n--- output ---\n%s", out)
	}
}

// fakeExporter is a workspaceexport.Exporter that returns a fixed
// error. Used by the exportWorkspace tests to exercise the required /
// optional error-handling branches without standing up a real GCS
// endpoint or credential source.
type fakeExporter struct {
	err error
}

func (f fakeExporter) Export(_ context.Context, _, _ string) error { return f.err }

// TestBuildRunResult_NilTrace pins the M5 fix: a nil RunTrace must
// surface the "internal-error" sentinel rather than a structurally
// valid but semantically incoherent RunResult{SchemaVersion: 1}.
// Consumers parsing the stdout-json line distinguish a no-trace path
// from an empty-Outcome run on this sentinel.
func TestBuildRunResult_NilTrace(t *testing.T) {
	got := buildRunResult(nil)
	if got.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", got.SchemaVersion)
	}
	if got.Outcome != "internal-error" {
		t.Errorf("Outcome = %q, want \"internal-error\"", got.Outcome)
	}
	if got.RunID != "" {
		t.Errorf("RunID = %q, want empty", got.RunID)
	}
	if got.Turns != 0 {
		t.Errorf("Turns = %d, want 0", got.Turns)
	}
}

// TestBuildRunResult_WithVerificationResult pins the verifier-verdict
// propagation: when the trace carries at least one VerificationResult,
// buildRunResult exposes the most recent entry as VerifierVerdict on
// the wire shape. Empty VerificationResults means VerifierVerdict is
// absent (presence of the optional pointer disambiguates "no verifier
// ran" from "verifier passed silently").
func TestBuildRunResult_WithVerificationResult(t *testing.T) {
	started := time.Now()
	rt := &types.RunTrace{
		ID:          "run-1",
		StartedAt:   started,
		CompletedAt: started.Add(750 * time.Millisecond),
		Turns:       3,
		Outcome:     "success",
		VerificationResults: []types.VerificationResult{
			{Passed: false, Feedback: "first pass missed the test"},
			{Passed: true, Feedback: "second pass green"},
		},
	}
	got := buildRunResult(rt)
	if got.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", got.Outcome)
	}
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", got.RunID)
	}
	if got.DurationMs != 750 {
		t.Errorf("DurationMs = %d, want 750", got.DurationMs)
	}
	if got.VerifierVerdict == nil {
		t.Fatal("VerifierVerdict = nil, want non-nil for the most recent verification result")
	}
	if !got.VerifierVerdict.Passed {
		t.Error("VerifierVerdict.Passed = false, want true (most recent verification passed)")
	}
	if got.VerifierVerdict.Feedback != "second pass green" {
		t.Errorf("VerifierVerdict.Feedback = %q, want \"second pass green\"", got.VerifierVerdict.Feedback)
	}
}

// TestBuildRunResult_NoVerificationResultsLeavesVerdictNil pins the
// disambiguation rule: an empty VerificationResults slice must leave
// VerifierVerdict nil so consumers see "no verifier ran" rather than
// a Passed=false default that would conflate with a real failure.
func TestBuildRunResult_NoVerificationResultsLeavesVerdictNil(t *testing.T) {
	rt := &types.RunTrace{
		ID:      "run-2",
		Outcome: "success",
	}
	if got := buildRunResult(rt); got.VerifierVerdict != nil {
		t.Errorf("VerifierVerdict = %+v, want nil", got.VerifierVerdict)
	}
}

// TestBuildRunResult_HookFailuresCounted pins the issue #461 field: a
// non-zero HookFailures count when the trace carries failed (but not
// skipped) lifecycle hook executions, computed across both phases.
func TestBuildRunResult_HookFailuresCounted(t *testing.T) {
	rt := &types.RunTrace{
		ID:      "run-hooks",
		Outcome: "hook_failed",
		HookResults: []types.HookExecution{
			{Phase: "preRun", Index: 0, Command: "true"},                         // succeeded
			{Phase: "postRun", Index: 0, Command: "false", Error: "exit code 1"}, // failed
			{Phase: "postRun", Index: 1, Command: "true", Skipped: true},         // skipped, not a failure
		},
	}
	got := buildRunResult(rt)
	if got.HookFailures != 1 {
		t.Errorf("HookFailures = %d, want 1 (skipped entries must not count)", got.HookFailures)
	}
}

// TestBuildRunResult_NoHookResultsLeavesHookFailuresZero pins the
// hookless-run default: an empty/nil HookResults must not panic and
// must resolve HookFailures to zero.
func TestBuildRunResult_NoHookResultsLeavesHookFailuresZero(t *testing.T) {
	rt := &types.RunTrace{ID: "run-no-hooks", Outcome: "success"}
	got := buildRunResult(rt)
	if got.HookFailures != 0 {
		t.Errorf("HookFailures = %d, want 0", got.HookFailures)
	}
}

// fakeCloser is an io.Closer test double that records whether/how many
// times Close was called, for armShutdownWatchdog tests.
type fakeCloser struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeCloser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeCloser) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestArmShutdownWatchdog_ClosesAfterGraceWhenRunNeverReturns pins the
// proactive-teardown path (issue #461 finding #1): if shutdownCtx fires
// and the caller never calls stop() (simulating Run() still blocked
// past the grace window — e.g. the orchestrator's SIGKILL is about to
// land), the watchdog closes the loop on its own.
func TestArmShutdownWatchdog_ClosesAfterGraceWhenRunNeverReturns(t *testing.T) {
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	armShutdownWatchdog(shutdownCtx, closer, 10*time.Millisecond)
	shutdownCancel()

	deadline := time.After(1 * time.Second)
	for closer.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("watchdog never closed the loop within the deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if closer.callCount() != 1 {
		t.Errorf("Close called %d times, want 1", closer.callCount())
	}
}

// TestArmShutdownWatchdog_StopPreventsClose pins the normal-return path:
// calling stop() before the grace window elapses (Run() returned
// through the ordinary path) must prevent the watchdog from ever
// calling Close — the caller's own `defer loop.Close()` owns teardown
// in that case.
func TestArmShutdownWatchdog_StopPreventsClose(t *testing.T) {
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	stop := armShutdownWatchdog(shutdownCtx, closer, 20*time.Millisecond)
	shutdownCancel()
	stop()

	time.Sleep(50 * time.Millisecond)
	if closer.callCount() != 0 {
		t.Errorf("Close called %d times, want 0 (stop() must prevent the proactive close)", closer.callCount())
	}
}

// TestArmShutdownWatchdog_NoShutdownNeverCloses pins the no-signal path:
// a watchdog whose shutdownCtx never fires must never close the loop,
// regardless of how long it runs.
func TestArmShutdownWatchdog_NoShutdownNeverCloses(t *testing.T) {
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	stop := armShutdownWatchdog(shutdownCtx, closer, 5*time.Millisecond)
	defer stop()

	time.Sleep(30 * time.Millisecond)
	if closer.callCount() != 0 {
		t.Errorf("Close called %d times, want 0 (no shutdown signal was ever sent)", closer.callCount())
	}
}

// TestExportWorkspace_NoopWhenEmpty pins the WorkspaceExportTo=="" path:
// no exporter is constructed and no HTTP call is made. Tested by
// asserting the factory closure is never invoked, so a regression that
// flipped the order of the early return and the factory call would
// surface as a failed test rather than a surprising metadata-server
// timeout on a workstation.
func TestExportWorkspace_NoopWhenEmpty(t *testing.T) {
	called := false
	orig := newWorkspaceExporter
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		called = true
		return fakeExporter{}, nil
	}
	defer func() { newWorkspaceExporter = orig }()

	cfg := &types.RunConfig{}
	if err := exportWorkspace(context.Background(), cfg, true); err != nil {
		t.Fatalf("exportWorkspace returned %v, want nil", err)
	}
	if called {
		t.Error("newWorkspaceExporter was invoked despite WorkspaceExportTo being empty")
	}
}

// TestExportWorkspace_RequiredPropagatesError pins the
// exportRequired=true contract: any failure from Export must surface
// as a non-nil error so the caller exits non-zero. A Cloud Run
// deployment that demands the workspace tarball for downstream
// automation depends on this signalling.
func TestExportWorkspace_RequiredPropagatesError(t *testing.T) {
	sentinel := errors.New("simulated GCS upload failure")
	orig := newWorkspaceExporter
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		return fakeExporter{err: sentinel}, nil
	}
	defer func() { newWorkspaceExporter = orig }()

	cfg := &types.RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"
	cfg.Executor.Workspace = t.TempDir()

	err := exportWorkspace(context.Background(), cfg, true)
	if err == nil {
		t.Fatal("exportWorkspace returned nil, want non-nil (export required)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not wrap sentinel: %v", err)
	}
}

// TestExportWorkspace_OptionalLogsError pins the exportRequired=false
// contract: an Export failure is logged with slog but does not
// propagate. A run that opted into best-effort export must still
// surface its real outcome rather than being masked by a transient
// GCS upload error.
func TestExportWorkspace_OptionalLogsError(t *testing.T) {
	orig := newWorkspaceExporter
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		return fakeExporter{err: errors.New("simulated GCS upload failure")}, nil
	}
	defer func() { newWorkspaceExporter = orig }()

	cfg := &types.RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"
	cfg.Executor.Workspace = t.TempDir()

	if err := exportWorkspace(context.Background(), cfg, false); err != nil {
		t.Errorf("exportWorkspace returned %v, want nil (export optional)", err)
	}
}

// TestExportWorkspace_BuilderErrorRequiredVsOptional pins the
// build-side branch of the same required/optional dichotomy: a
// factory error is treated identically to an Export error.
func TestExportWorkspace_BuilderErrorRequiredVsOptional(t *testing.T) {
	build := errors.New("simulated factory failure")
	orig := newWorkspaceExporter
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		return nil, build
	}
	defer func() { newWorkspaceExporter = orig }()

	cfg := &types.RunConfig{}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	t.Run("required", func(t *testing.T) {
		err := exportWorkspace(context.Background(), cfg, true)
		if !errors.Is(err, build) {
			t.Errorf("required: want sentinel in chain, got %v", err)
		}
	})
	t.Run("optional", func(t *testing.T) {
		if err := exportWorkspace(context.Background(), cfg, false); err != nil {
			t.Errorf("optional: want nil, got %v", err)
		}
	})
}
