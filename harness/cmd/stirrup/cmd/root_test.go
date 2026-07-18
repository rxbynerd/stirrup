package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// TestRootCmd_Version pins the format of the harness `--version` output.
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

// TestRootCmd_BareInvocationPrintsHint pins the root behaviour: a bare
// `stirrup` (no subcommand, no --help / --version) prints the short
// orientation hint to stdout and exits 0 — not Cobra's full usage block.
func TestRootCmd_BareInvocationPrintsHint(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{})
	// SetArgs doesn't clear flag values from a prior Execute() on the
	// shared rootCmd; a --version Changed=true from another test would
	// make Cobra short-circuit before Run fires.
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

// fakeExporter is a workspaceexport.Exporter that returns a fixed error,
// avoiding a real GCS endpoint in the exportWorkspace tests.
type fakeExporter struct {
	err error
}

func (f fakeExporter) Export(_ context.Context, _, _ string) error { return f.err }

// TestBuildRunResult_NilTrace pins that a nil RunTrace surfaces the
// "internal-error" sentinel rather than a structurally valid but
// semantically incoherent RunResult.
func TestBuildRunResult_NilTrace(t *testing.T) {
	got := buildRunResult(nil, types.DefaultMaxFinalAssistantTextBytes)
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

// TestBuildRunResult_WithVerificationResult pins that buildRunResult
// exposes the most recent VerificationResult as VerifierVerdict.
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
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
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

// TestBuildRunResult_NoVerificationResultsLeavesVerdictNil pins that an
// empty VerificationResults slice leaves VerifierVerdict nil, distinct
// from a Passed=false default.
func TestBuildRunResult_NoVerificationResultsLeavesVerdictNil(t *testing.T) {
	rt := &types.RunTrace{
		ID:      "run-2",
		Outcome: "success",
	}
	if got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes); got.VerifierVerdict != nil {
		t.Errorf("VerifierVerdict = %+v, want nil", got.VerifierVerdict)
	}
}

// TestBuildRunResult_FinalAssistantText pins that a populated
// FinalAssistantText carries through verbatim and an empty one is
// omitted from the wire via the omitempty tag.
func TestBuildRunResult_FinalAssistantText(t *testing.T) {
	cases := []struct {
		name     string
		traceIn  string
		wantText string
	}{
		{"populated", "the answer is 42", "the answer is 42"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &types.RunTrace{
				ID:                 "run-fat",
				Outcome:            "success",
				FinalAssistantText: tc.traceIn,
			}
			got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
			if got.FinalAssistantText != tc.wantText {
				t.Errorf("FinalAssistantText = %q, want %q", got.FinalAssistantText, tc.wantText)
			}
			if got.FinalAssistantTextTruncated {
				t.Errorf("FinalAssistantTextTruncated = true, want false: text is well under the default cap")
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			hasField := strings.Contains(string(encoded), "finalAssistantText")
			if tc.wantText == "" && hasField {
				t.Errorf("empty FinalAssistantText should be omitted from JSON, got %s", encoded)
			}
			if tc.wantText != "" && !hasField {
				t.Errorf("populated FinalAssistantText should be present in JSON, got %s", encoded)
			}
			if strings.Contains(string(encoded), "finalAssistantTextTruncated") {
				t.Errorf("finalAssistantTextTruncated=false should be omitted from JSON, got %s", encoded)
			}
		})
	}
}

// TestBuildRunResult_FinalAssistantTextCappedAndFlagged pins that text
// longer than the resolved cap is truncated with a marker appended and
// FinalAssistantTextTruncated set, while rt itself is left untouched.
func TestBuildRunResult_FinalAssistantTextCappedAndFlagged(t *testing.T) {
	longText := strings.Repeat("a", 100)
	rt := &types.RunTrace{
		ID:                 "run-cap",
		Outcome:            "success",
		FinalAssistantText: longText,
	}
	const maxBytes = 10
	got := buildRunResult(rt, maxBytes)

	wantPrefix := strings.Repeat("a", maxBytes)
	if !strings.HasPrefix(got.FinalAssistantText, wantPrefix) {
		t.Errorf("FinalAssistantText = %q, want prefix %q", got.FinalAssistantText, wantPrefix)
	}
	if !strings.HasSuffix(got.FinalAssistantText, "[truncated by harness]") {
		t.Errorf("FinalAssistantText = %q, want truncation marker suffix", got.FinalAssistantText)
	}
	if !got.FinalAssistantTextTruncated {
		t.Error("FinalAssistantTextTruncated = false, want true")
	}
	if len(got.FinalAssistantText) <= maxBytes {
		t.Errorf("capped FinalAssistantText len = %d, want > cap %d (marker must still be present)", len(got.FinalAssistantText), maxBytes)
	}

	// buildRunResult must not mutate the RunTrace it was given.
	if rt.FinalAssistantText != longText {
		t.Errorf("rt.FinalAssistantText was mutated to %q, want untouched %q", rt.FinalAssistantText, longText)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"finalAssistantTextTruncated":true`) {
		t.Errorf("marshalled RunResult missing finalAssistantTextTruncated:true, got %s", encoded)
	}
}

// TestBuildRunResult_FinalAssistantTextUnderCapUntouched pins that text
// at or under the cap passes through unmodified.
func TestBuildRunResult_FinalAssistantTextUnderCapUntouched(t *testing.T) {
	rt := &types.RunTrace{
		ID:                 "run-under-cap",
		Outcome:            "success",
		FinalAssistantText: "short answer",
	}
	got := buildRunResult(rt, 1024)
	if got.FinalAssistantText != "short answer" {
		t.Errorf("FinalAssistantText = %q, want unmodified %q", got.FinalAssistantText, "short answer")
	}
	if got.FinalAssistantTextTruncated {
		t.Error("FinalAssistantTextTruncated = true, want false")
	}
}

// TestBuildRunResult_HookFailuresCounted pins that HookFailures counts
// failed (but not skipped) lifecycle hook executions across both phases.
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
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	if got.HookFailures != 1 {
		t.Errorf("HookFailures = %d, want 1 (skipped entries must not count)", got.HookFailures)
	}
}

// TestBuildRunResult_NoHookResultsLeavesHookFailuresZero pins that a
// nil HookResults resolves HookFailures to zero without panicking.
func TestBuildRunResult_NoHookResultsLeavesHookFailuresZero(t *testing.T) {
	rt := &types.RunTrace{ID: "run-no-hooks", Outcome: "success"}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	if got.HookFailures != 0 {
		t.Errorf("HookFailures = %d, want 0", got.HookFailures)
	}
}

// fakeCloser is an io.Closer test double recording Close call count,
// for armShutdownWatchdog tests.
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

// TestArmShutdownWatchdog_ClosesAfterGraceWhenRunNeverReturns pins that
// if shutdownCtx fires and stop() is never called (Run() still blocked
// past the grace window), the watchdog closes the loop on its own.
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

// TestArmShutdownWatchdog_StopPreventsClose pins that calling stop()
// before the grace window elapses prevents the watchdog from calling
// Close — the caller's own `defer loop.Close()` owns teardown instead.
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

// TestArmShutdownWatchdog_NoShutdownNeverCloses pins that a watchdog
// whose shutdownCtx never fires never closes the loop.
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

// TestExportWorkspace_NoopWhenEmpty pins that WorkspaceExportTo=="" skips
// exporter construction entirely, asserted via the factory closure.
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

// TestExportWorkspace_RequiredPropagatesError pins that with
// exportRequired=true, any failure from Export surfaces as a non-nil
// error so the caller exits non-zero.
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

// TestExportWorkspace_OptionalLogsError pins that with
// exportRequired=false, an Export failure is logged but does not
// propagate.
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
