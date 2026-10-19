package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
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

	require.NoError(t, rootCmd.Execute())

	out := buf.String()
	require.True(t, strings.HasPrefix(out, "stirrup version "), "unexpected --version output: %q", out)
	// Default link-time version when no -ldflags injected is "dev".
	require.Contains(t, out, "dev", "--version output should include the default link-time version")
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

	require.NoError(t, rootCmd.Execute(), "a bare invocation must exit 0")

	out := buf.String()
	for _, want := range []string{
		"stirrup — a coding agent harness",
		"stirrup harness --prompt",
		"stirrup job",
		"stirrup harness --help",
		"stirrup --version",
	} {
		t.Run("hint mentions "+want, func(t *testing.T) {
			require.Contains(t, out, want)
		})
	}
	// The hint must be plain text — no boxes, no headings beyond the
	// title, and crucially no ANSI escapes.
	assert.NotContains(t, out, "\x1b[", "the bare-stirrup hint must not contain ANSI escapes")
	// A regression that printed Cobra's full help instead of the hint
	// would surface its "Available Commands:" / "Flags:" sections.
	assert.NotContains(t, out, "Available Commands:", "bare stirrup printed Cobra's full help")
	assert.NotContains(t, out, `Use "stirrup [command] --help"`, "bare stirrup printed Cobra's full help")
}

// fakeExporter is a workspaceexport.Exporter that returns a fixed error,
// avoiding a real GCS endpoint in the exportWorkspace tests.
type fakeExporter struct {
	err    error
	status string
}

func (f fakeExporter) Export(_ context.Context, _ executor.WorkspaceArchiver, dest string) (types.ExportReceipt, error) {
	return fakeExportResult(dest, f.status, f.err)
}

// fakeExportResult is the receipt a fake exporter returns for dest: failed
// with err, otherwise status, which defaults to uploaded.
func fakeExportResult(dest, status string, err error) (types.ExportReceipt, error) {
	if err != nil {
		return types.ExportReceipt{URI: dest, Status: types.ExportStatusFailed}, err
	}
	if status == "" {
		status = types.ExportStatusUploaded
	}
	return types.ExportReceipt{URI: dest, Status: status}, nil
}

// TestBuildRunResult_NilTraceReportsInternalError pins that a nil RunTrace surfaces the
// "internal-error" sentinel rather than a structurally valid but
// semantically incoherent RunResult.
func TestBuildRunResult_NilTraceReportsInternalError(t *testing.T) {
	got := buildRunResult(nil, types.DefaultMaxFinalAssistantTextBytes)
	assert.Equal(t, 1, got.SchemaVersion)
	assert.Equal(t, "internal-error", got.Outcome)
	assert.Empty(t, got.RunID)
	assert.Zero(t, got.Turns)
}

// TestBuildRunResult_ExposesLatestVerificationVerdict pins that
// buildRunResult exposes the most recent VerificationResult as VerifierVerdict.
func TestBuildRunResult_ExposesLatestVerificationVerdict(t *testing.T) {
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
	assert.Equal(t, "success", got.Outcome)
	assert.Equal(t, "run-1", got.RunID)
	assert.EqualValues(t, 750, got.DurationMs)
	require.NotNil(t, got.VerifierVerdict, "want a verdict for the most recent verification result")
	assert.True(t, got.VerifierVerdict.Passed, "most recent verification passed")
	assert.Equal(t, "second pass green", got.VerifierVerdict.Feedback)
}

// TestBuildRunResult_NoVerificationResultsLeavesVerdictNil pins that an
// empty VerificationResults slice leaves VerifierVerdict nil, distinct
// from a Passed=false default.
func TestBuildRunResult_NoVerificationResultsLeavesVerdictNil(t *testing.T) {
	rt := &types.RunTrace{
		ID:      "run-2",
		Outcome: "success",
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	require.Nil(t, got.VerifierVerdict)
}

func TestBuildRunResult_CarriesAndOmitsTraceReference(t *testing.T) {
	ref := &types.TraceReference{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", RunSpanID: "00f067aa0ba902b7", Sampled: true}
	got := buildRunResult(&types.RunTrace{ID: "run-ref", Outcome: "success", TraceReference: ref}, types.DefaultMaxFinalAssistantTextBytes)
	require.NotNil(t, got.TraceReference)
	require.Equal(t, *ref, *got.TraceReference)

	raw, err := json.Marshal(buildRunResult(&types.RunTrace{ID: "run-jsonl", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "traceReference", "a RunResult without an OTel span must omit traceReference")
}

func TestBuildRunResult_VerificationOutcomesPopulateError(t *testing.T) {
	cases := []struct {
		name        string
		outcome     string
		results     []types.VerificationResult
		wantError   string
		wantVerdict types.VerifierResult
	}{
		{
			name:    "verification_failed carries the deciding attempt's feedback",
			outcome: "verification_failed",
			results: []types.VerificationResult{
				{Attempt: 1, Feedback: "attempt-1 failure"},
				{Attempt: 2, Feedback: "attempt-2 failure"},
			},
			wantError:   "attempt-2 failure",
			wantVerdict: types.VerifierResult{Attempt: 2, Feedback: "attempt-2 failure"},
		},
		{
			name:        "verification_failed without feedback gets a fallback",
			outcome:     "verification_failed",
			results:     []types.VerificationResult{{Attempt: 1}},
			wantError:   "verification failed",
			wantVerdict: types.VerifierResult{Attempt: 1},
		},
		{
			name:        "verification_error carries the verifier error",
			outcome:     "verification_error",
			results:     []types.VerificationResult{{Attempt: 1, Error: "exec failed: [REDACTED]"}},
			wantError:   "exec failed: [REDACTED]",
			wantVerdict: types.VerifierResult{Attempt: 1, Error: "exec failed: [REDACTED]"},
		},
		{
			name:    "success after a repair carries no error",
			outcome: "success",
			results: []types.VerificationResult{
				{Attempt: 1, Feedback: "tests failed"},
				{Attempt: 2, Passed: true},
			},
			wantVerdict: types.VerifierResult{Attempt: 2, Passed: true},
		},
		{
			name:        "a later outcome does not inherit verifier feedback",
			outcome:     "cancelled",
			results:     []types.VerificationResult{{Attempt: 1, Feedback: "tests failed"}},
			wantVerdict: types.VerifierResult{Attempt: 1, Feedback: "tests failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRunResult(&types.RunTrace{Outcome: tc.outcome, VerificationResults: tc.results}, types.DefaultMaxFinalAssistantTextBytes)
			require.Equal(t, tc.wantError, got.Error)
			require.NotNil(t, got.VerifierVerdict)
			require.Equal(t, tc.wantVerdict, *got.VerifierVerdict)
		})
	}
}

// TestBuildRunResult_FinalAssistantTextCarriedAndOmittedWhenEmpty pins that a populated
// FinalAssistantText carries through verbatim and an empty one is
// omitted from the wire via the omitempty tag.
func TestBuildRunResult_FinalAssistantTextCarriedAndOmittedWhenEmpty(t *testing.T) {
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
			require.Equal(t, tc.wantText, got.FinalAssistantText)
			require.False(t, got.FinalAssistantTextTruncated, "text is well under the default cap")
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			if tc.wantText == "" {
				require.NotContains(t, string(encoded), "finalAssistantText", "empty FinalAssistantText should be omitted from JSON")
			} else {
				require.Contains(t, string(encoded), "finalAssistantText", "populated FinalAssistantText should be present in JSON")
			}
			require.NotContains(t, string(encoded), "finalAssistantTextTruncated", "finalAssistantTextTruncated=false should be omitted from JSON")
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
	assert.True(t, strings.HasPrefix(got.FinalAssistantText, wantPrefix), "FinalAssistantText = %q, want prefix %q", got.FinalAssistantText, wantPrefix)
	assert.True(t, strings.HasSuffix(got.FinalAssistantText, "[truncated by harness]"), "FinalAssistantText = %q, want truncation marker suffix", got.FinalAssistantText)
	assert.True(t, got.FinalAssistantTextTruncated)
	assert.Greater(t, len(got.FinalAssistantText), maxBytes, "the truncation marker must keep the capped text longer than the cap")

	// buildRunResult must not mutate the RunTrace it was given.
	assert.Equal(t, longText, rt.FinalAssistantText)

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"finalAssistantTextTruncated":true`)
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
	assert.Equal(t, "short answer", got.FinalAssistantText)
	assert.False(t, got.FinalAssistantTextTruncated)
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
	require.Equal(t, 1, got.HookFailures, "skipped entries must not count")
}

// TestBuildRunResult_NoHookResultsLeavesHookFailuresZero pins that a
// nil HookResults resolves HookFailures to zero without panicking.
func TestBuildRunResult_NoHookResultsLeavesHookFailuresZero(t *testing.T) {
	rt := &types.RunTrace{ID: "run-no-hooks", Outcome: "success"}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	require.Zero(t, got.HookFailures)
}

func TestBuildRunResult_CarriesGitHandoff(t *testing.T) {
	handoff := &types.GitHandoff{Branch: "stirrup/run-git", SHA: "bbb", BaseSHA: "aaa"}
	got := buildRunResult(&types.RunTrace{ID: "run-git", Outcome: "success", Git: handoff}, types.DefaultMaxFinalAssistantTextBytes)
	require.NotNil(t, got.Git)
	assert.Equal(t, *handoff, *got.Git)
	assert.Empty(t, got.Error, "a successful run must not report an error")
}

func TestBuildRunResult_MirrorsRunErrorAndExportReceipt(t *testing.T) {
	rt := &types.RunTrace{
		ID:              "run-export-fail",
		Outcome:         "handoff_failed",
		Error:           "workspace export: upload refused",
		WorkspaceExport: &types.ExportReceipt{URI: "gs://b/ws.tar.gz", Status: types.ExportStatusFailed, Required: true, Error: "upload refused"},
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	assert.Equal(t, rt.Error, got.Error)
	require.NotNil(t, got.WorkspaceExport)
	assert.Equal(t, *rt.WorkspaceExport, *got.WorkspaceExport)
}

// An earlier failure outcome keeps RunResult.Error for that failure; the
// git failure stays in the git section.
func TestBuildRunResult_GitErrorOnFailedRunStaysInGitSection(t *testing.T) {
	rt := &types.RunTrace{
		ID:      "run-budget",
		Outcome: "budget_exceeded",
		Error:   "exceeded the run's token budget",
		Git:     &types.GitHandoff{Branch: "stirrup/run-budget", Error: "final checkpoint: commit: exit status 1"},
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	assert.Equal(t, rt.Error, got.Error, "the earliest failure must be kept")
	require.NotNil(t, got.Git)
	assert.Equal(t, rt.Git.Error, got.Git.Error)
}

func TestBuildRunResult_NoGitHandoffOmitsSection(t *testing.T) {
	got := buildRunResult(&types.RunTrace{ID: "run-none", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"git"`, "a run without a git handoff must omit the git key")
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

	require.Eventually(t, func() bool {
		return closer.callCount() > 0
	}, time.Second, 5*time.Millisecond, "watchdog never closed the loop within the deadline")
	require.Equal(t, 1, closer.callCount(), "Close must run exactly once")
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
	require.Zero(t, closer.callCount(), "stop() must prevent the proactive close")
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
	require.Zero(t, closer.callCount(), "no shutdown signal was ever sent")
}

func TestHandleSignals_FirstInterruptsSecondShutsDown(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	interrupted := make(chan struct{})
	shutdown := make(chan struct{})
	done := make(chan struct{})
	go func() {
		handleSignals(sigCh, &bytes.Buffer{}, func() { close(interrupted) }, func() { close(shutdown) })
		close(done)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		require.FailNow(t, "first signal did not call interrupt")
	}
	select {
	case <-shutdown:
		require.FailNow(t, "first signal called shutdown, want post-run work left running")
	case <-time.After(20 * time.Millisecond):
	}

	sigCh <- syscall.SIGTERM
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		require.FailNow(t, "second signal did not call shutdown")
	}
	<-done
}

func TestHandleSignals_NilShutdownReturnsAfterFirstSignal(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	interrupted := false
	done := make(chan struct{})
	go func() {
		handleSignals(sigCh, &bytes.Buffer{}, func() { interrupted = true }, nil)
		close(done)
	}()

	sigCh <- syscall.SIGINT
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "handleSignals did not return after the first signal with no shutdown func")
	}
	require.True(t, interrupted, "first signal did not call interrupt")
}

func installFakeExporter(t *testing.T, exp workspaceexport.Exporter, buildErr error) *bool {
	t.Helper()
	called := false
	orig := newWorkspaceExporter
	t.Cleanup(func() { newWorkspaceExporter = orig })
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		called = true
		return exp, buildErr
	}
	return &called
}

func TestWorkspaceHandoff_NoDestinationSkipsExport(t *testing.T) {
	called := installFakeExporter(t, fakeExporter{}, nil)
	h := workspaceHandoff{primaryRunID: "run-1"}
	receipt, err := h.Handoff(context.Background(), &types.RunConfig{RunID: "run-1"}, nil)
	require.NoError(t, err)
	require.Nil(t, receipt)
	require.False(t, *called, "newWorkspaceExporter was invoked despite WorkspaceExportTo being empty")
}

func TestWorkspaceHandoff_SuccessfulExport(t *testing.T) {
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.NoError(t, err)
	require.NotNil(t, receipt)
	want := types.ExportReceipt{URI: cfg.Executor.WorkspaceExportTo, Status: types.ExportStatusUploaded}
	require.Equal(t, want, *receipt)
	require.Equal(t, []string{cfg.Executor.WorkspaceExportTo}, rec.destinations())
}

func TestWorkspaceHandoff_EmptyWorkspaceIsAnEmptySuccess(t *testing.T) {
	installFakeExporter(t, fakeExporter{status: types.ExportStatusEmpty}, nil)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.NoError(t, err, "an empty workspace is a successful handoff")
	require.NotNil(t, receipt)
	require.Equal(t, types.ExportStatusEmpty, receipt.Status)
}

func TestWorkspaceHandoff_ExportFailureReturnsReceiptAndError(t *testing.T) {
	sentinel := errors.New("simulated GCS upload failure")
	installFakeExporter(t, fakeExporter{err: sentinel}, nil)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.ErrorIs(t, err, sentinel)
	require.NotNil(t, receipt)
	require.Equal(t, cfg.Executor.WorkspaceExportTo, receipt.URI)
	require.NotEqual(t, types.ExportStatusUploaded, receipt.Status, "a failed export must not report uploaded")
}

func TestWorkspaceHandoff_ExporterBuildFailure(t *testing.T) {
	build := errors.New("simulated factory failure")
	installFakeExporter(t, nil, build)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.ErrorIs(t, err, build)
	require.NotNil(t, receipt, "a failed exporter build still returns a receipt")
}

func TestWorkspaceHandoff_FollowUpExportsToRunScopedURI(t *testing.T) {
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)
	cfg := &types.RunConfig{RunID: "primary"}
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"
	h := workspaceHandoff{primaryRunID: "primary"}

	_, err := h.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.NoError(t, err)
	cfg.RunID = "run-2"
	receipt, err := h.Handoff(context.Background(), cfg, &archiverExecutor{})
	require.NoError(t, err)
	want := []string{"gs://bucket/runs/primary/workspace.tar.gz", "gs://bucket/runs/primary/run-2/workspace.tar.gz"}
	require.Equal(t, want, rec.destinations())
	require.Equal(t, want[1], receipt.URI)
}

func TestBuildRunResult_CarriesDegradedCapabilities(t *testing.T) {
	t.Run("reports degraded capabilities", func(t *testing.T) {
		rt := &types.RunTrace{
			ID:      "run-degraded",
			Outcome: "success",
			DegradedCapabilities: []types.DegradedCapability{
				{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"},
			},
		}
		got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
		require.Len(t, got.DegradedCapabilities, 1)
		require.Equal(t, "docs", got.DegradedCapabilities[0].Name)
	})

	t.Run("omits the field when nothing degraded", func(t *testing.T) {
		result := buildRunResult(&types.RunTrace{ID: "clean", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes)
		raw, err := json.Marshal(result)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "degradedCapabilities")
	})
}
