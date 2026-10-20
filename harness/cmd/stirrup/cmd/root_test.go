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
	must := require.New(t)
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

	must.NoError(rootCmd.Execute())

	out := buf.String()
	must.True(strings.HasPrefix(out, "stirrup version "), "unexpected --version output: %q", out)
	// Default link-time version when no -ldflags injected is "dev".
	must.Contains(out, "dev", "--version output should include the default link-time version")
}

// TestRootCmd_BareInvocationPrintsHint pins the root behaviour: a bare
// `stirrup` (no subcommand, no --help / --version) prints the short
// orientation hint to stdout and exits 0 — not Cobra's full usage block.
func TestRootCmd_BareInvocationPrintsHint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(rootCmd.Execute(), "a bare invocation must exit 0")

	out := buf.String()
	for _, want := range []string{
		"stirrup — a coding agent harness",
		"stirrup harness --prompt",
		"stirrup job",
		"stirrup harness --help",
		"stirrup --version",
	} {
		t.Run("hint mentions "+want, func(t *testing.T) {
			must := require.New(t)
			must.Contains(out, want)
		})
	}
	// The hint must be plain text — no boxes, no headings beyond the
	// title, and crucially no ANSI escapes.
	check.NotContains(out, "\x1b[", "the bare-stirrup hint must not contain ANSI escapes")
	// A regression that printed Cobra's full help instead of the hint
	// would surface its "Available Commands:" / "Flags:" sections.
	check.NotContains(out, "Available Commands:", "bare stirrup printed Cobra's full help")
	check.NotContains(out, `Use "stirrup [command] --help"`, "bare stirrup printed Cobra's full help")
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
	check := assert.New(t)
	got := buildRunResult(nil, types.DefaultMaxFinalAssistantTextBytes)
	check.Equal(1, got.SchemaVersion)
	check.Equal("internal-error", got.Outcome)
	check.Empty(got.RunID)
	check.Zero(got.Turns)
}

// TestBuildRunResult_ExposesLatestVerificationVerdict pins that
// buildRunResult exposes the most recent VerificationResult as VerifierVerdict.
func TestBuildRunResult_ExposesLatestVerificationVerdict(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	check.Equal("success", got.Outcome)
	check.Equal("run-1", got.RunID)
	check.EqualValues(750, got.DurationMs)
	must.NotNil(got.VerifierVerdict, "want a verdict for the most recent verification result")
	check.True(got.VerifierVerdict.Passed, "most recent verification passed")
	check.Equal("second pass green", got.VerifierVerdict.Feedback)
}

// TestBuildRunResult_NoVerificationResultsLeavesVerdictNil pins that an
// empty VerificationResults slice leaves VerifierVerdict nil, distinct
// from a Passed=false default.
func TestBuildRunResult_NoVerificationResultsLeavesVerdictNil(t *testing.T) {
	must := require.New(t)
	rt := &types.RunTrace{
		ID:      "run-2",
		Outcome: "success",
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	must.Nil(got.VerifierVerdict)
}

func TestBuildRunResult_CarriesAndOmitsTraceReference(t *testing.T) {
	must := require.New(t)
	ref := &types.TraceReference{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", RunSpanID: "00f067aa0ba902b7", Sampled: true}
	got := buildRunResult(&types.RunTrace{ID: "run-ref", Outcome: "success", TraceReference: ref}, types.DefaultMaxFinalAssistantTextBytes)
	must.NotNil(got.TraceReference)
	must.Equal(*ref, *got.TraceReference)

	raw, err := json.Marshal(buildRunResult(&types.RunTrace{ID: "run-jsonl", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes))
	must.NoError(err)
	must.NotContains(string(raw), "traceReference", "a RunResult without an OTel span must omit traceReference")
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
			must := require.New(t)
			got := buildRunResult(&types.RunTrace{Outcome: tc.outcome, VerificationResults: tc.results}, types.DefaultMaxFinalAssistantTextBytes)
			must.Equal(tc.wantError, got.Error)
			must.NotNil(got.VerifierVerdict)
			must.Equal(tc.wantVerdict, *got.VerifierVerdict)
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
			must := require.New(t)
			rt := &types.RunTrace{
				ID:                 "run-fat",
				Outcome:            "success",
				FinalAssistantText: tc.traceIn,
			}
			got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
			must.Equal(tc.wantText, got.FinalAssistantText)
			must.False(got.FinalAssistantTextTruncated, "text is well under the default cap")
			encoded, err := json.Marshal(got)
			must.NoError(err)
			if tc.wantText == "" {
				must.NotContains(string(encoded), "finalAssistantText", "empty FinalAssistantText should be omitted from JSON")
			} else {
				must.Contains(string(encoded), "finalAssistantText", "populated FinalAssistantText should be present in JSON")
			}
			must.NotContains(string(encoded), "finalAssistantTextTruncated", "finalAssistantTextTruncated=false should be omitted from JSON")
		})
	}
}

// TestBuildRunResult_FinalAssistantTextCappedAndFlagged pins that text
// longer than the resolved cap is truncated with a marker appended and
// FinalAssistantTextTruncated set, while rt itself is left untouched.
func TestBuildRunResult_FinalAssistantTextCappedAndFlagged(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	longText := strings.Repeat("a", 100)
	rt := &types.RunTrace{
		ID:                 "run-cap",
		Outcome:            "success",
		FinalAssistantText: longText,
	}
	const maxBytes = 10
	got := buildRunResult(rt, maxBytes)

	wantPrefix := strings.Repeat("a", maxBytes)
	check.True(strings.HasPrefix(got.FinalAssistantText, wantPrefix), "FinalAssistantText = %q, want prefix %q", got.FinalAssistantText, wantPrefix)
	check.True(strings.HasSuffix(got.FinalAssistantText, "[truncated by harness]"), "FinalAssistantText = %q, want truncation marker suffix", got.FinalAssistantText)
	check.True(got.FinalAssistantTextTruncated)
	check.Greater(len(got.FinalAssistantText), maxBytes, "the truncation marker must keep the capped text longer than the cap")

	// buildRunResult must not mutate the RunTrace it was given.
	check.Equal(longText, rt.FinalAssistantText)

	encoded, err := json.Marshal(got)
	must.NoError(err)
	check.Contains(string(encoded), `"finalAssistantTextTruncated":true`)
}

// TestBuildRunResult_FinalAssistantTextUnderCapUntouched pins that text
// at or under the cap passes through unmodified.
func TestBuildRunResult_FinalAssistantTextUnderCapUntouched(t *testing.T) {
	check := assert.New(t)
	rt := &types.RunTrace{
		ID:                 "run-under-cap",
		Outcome:            "success",
		FinalAssistantText: "short answer",
	}
	got := buildRunResult(rt, 1024)
	check.Equal("short answer", got.FinalAssistantText)
	check.False(got.FinalAssistantTextTruncated)
}

// TestBuildRunResult_HookFailuresCounted pins that HookFailures counts
// failed (but not skipped) lifecycle hook executions across both phases.
func TestBuildRunResult_HookFailuresCounted(t *testing.T) {
	must := require.New(t)
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
	must.Equal(1, got.HookFailures, "skipped entries must not count")
}

// TestBuildRunResult_NoHookResultsLeavesHookFailuresZero pins that a
// nil HookResults resolves HookFailures to zero without panicking.
func TestBuildRunResult_NoHookResultsLeavesHookFailuresZero(t *testing.T) {
	must := require.New(t)
	rt := &types.RunTrace{ID: "run-no-hooks", Outcome: "success"}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	must.Zero(got.HookFailures)
}

func TestBuildRunResult_CarriesGitHandoff(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	handoff := &types.GitHandoff{Branch: "stirrup/run-git", SHA: "bbb", BaseSHA: "aaa"}
	got := buildRunResult(&types.RunTrace{ID: "run-git", Outcome: "success", Git: handoff}, types.DefaultMaxFinalAssistantTextBytes)
	must.NotNil(got.Git)
	check.Equal(*handoff, *got.Git)
	check.Empty(got.Error, "a successful run must not report an error")
}

func TestBuildRunResult_MirrorsRunErrorAndExportReceipt(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	rt := &types.RunTrace{
		ID:              "run-export-fail",
		Outcome:         "handoff_failed",
		Error:           "workspace export: upload refused",
		WorkspaceExport: &types.ExportReceipt{URI: "gs://b/ws.tar.gz", Status: types.ExportStatusFailed, Required: true, Error: "upload refused"},
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	check.Equal(rt.Error, got.Error)
	must.NotNil(got.WorkspaceExport)
	check.Equal(*rt.WorkspaceExport, *got.WorkspaceExport)
}

// An earlier failure outcome keeps RunResult.Error for that failure; the
// git failure stays in the git section.
func TestBuildRunResult_GitErrorOnFailedRunStaysInGitSection(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	rt := &types.RunTrace{
		ID:      "run-budget",
		Outcome: "budget_exceeded",
		Error:   "exceeded the run's token budget",
		Git:     &types.GitHandoff{Branch: "stirrup/run-budget", Error: "final checkpoint: commit: exit status 1"},
	}
	got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
	check.Equal(rt.Error, got.Error, "the earliest failure must be kept")
	must.NotNil(got.Git)
	check.Equal(rt.Git.Error, got.Git.Error)
}

func TestBuildRunResult_NoGitHandoffOmitsSection(t *testing.T) {
	must := require.New(t)
	got := buildRunResult(&types.RunTrace{ID: "run-none", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes)
	data, err := json.Marshal(got)
	must.NoError(err)
	must.NotContains(string(data), `"git"`, "a run without a git handoff must omit the git key")
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
	must := require.New(t)
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	armShutdownWatchdog(shutdownCtx, closer, 10*time.Millisecond)
	shutdownCancel()

	must.Eventually(func() bool {
		return closer.callCount() > 0
	}, time.Second, 5*time.Millisecond, "watchdog never closed the loop within the deadline")
	must.Equal(1, closer.callCount(), "Close must run exactly once")
}

// TestArmShutdownWatchdog_StopPreventsClose pins that calling stop()
// before the grace window elapses prevents the watchdog from calling
// Close — the caller's own `defer loop.Close()` owns teardown instead.
func TestArmShutdownWatchdog_StopPreventsClose(t *testing.T) {
	must := require.New(t)
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	stop := armShutdownWatchdog(shutdownCtx, closer, 20*time.Millisecond)
	shutdownCancel()
	stop()

	time.Sleep(50 * time.Millisecond)
	must.Zero(closer.callCount(), "stop() must prevent the proactive close")
}

// TestArmShutdownWatchdog_NoShutdownNeverCloses pins that a watchdog
// whose shutdownCtx never fires never closes the loop.
func TestArmShutdownWatchdog_NoShutdownNeverCloses(t *testing.T) {
	must := require.New(t)
	closer := &fakeCloser{}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	stop := armShutdownWatchdog(shutdownCtx, closer, 5*time.Millisecond)
	defer stop()

	time.Sleep(30 * time.Millisecond)
	must.Zero(closer.callCount(), "no shutdown signal was ever sent")
}

func TestHandleSignals_FirstInterruptsSecondShutsDown(t *testing.T) {
	must := require.New(t)
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
		must.FailNow("first signal did not call interrupt")
	}
	select {
	case <-shutdown:
		must.FailNow("first signal called shutdown, want post-run work left running")
	case <-time.After(20 * time.Millisecond):
	}

	sigCh <- syscall.SIGTERM
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		must.FailNow("second signal did not call shutdown")
	}
	<-done
}

func TestHandleSignals_NilShutdownReturnsAfterFirstSignal(t *testing.T) {
	must := require.New(t)
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
		must.FailNow("handleSignals did not return after the first signal with no shutdown func")
	}
	must.True(interrupted, "first signal did not call interrupt")
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
	must := require.New(t)
	called := installFakeExporter(t, fakeExporter{}, nil)
	h := workspaceHandoff{primaryRunID: "run-1"}
	receipt, err := h.Handoff(context.Background(), &types.RunConfig{RunID: "run-1"}, nil)
	must.NoError(err)
	must.Nil(receipt)
	must.False(*called, "newWorkspaceExporter was invoked despite WorkspaceExportTo being empty")
}

func TestWorkspaceHandoff_SuccessfulExport(t *testing.T) {
	must := require.New(t)
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.NoError(err)
	must.NotNil(receipt)
	want := types.ExportReceipt{URI: cfg.Executor.WorkspaceExportTo, Status: types.ExportStatusUploaded}
	must.Equal(want, *receipt)
	must.Equal([]string{cfg.Executor.WorkspaceExportTo}, rec.destinations())
}

func TestWorkspaceHandoff_EmptyWorkspaceIsAnEmptySuccess(t *testing.T) {
	must := require.New(t)
	installFakeExporter(t, fakeExporter{status: types.ExportStatusEmpty}, nil)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.NoError(err, "an empty workspace is a successful handoff")
	must.NotNil(receipt)
	must.Equal(types.ExportStatusEmpty, receipt.Status)
}

func TestWorkspaceHandoff_ExportFailureReturnsReceiptAndError(t *testing.T) {
	must := require.New(t)
	sentinel := errors.New("simulated GCS upload failure")
	installFakeExporter(t, fakeExporter{err: sentinel}, nil)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.ErrorIs(err, sentinel)
	must.NotNil(receipt)
	must.Equal(cfg.Executor.WorkspaceExportTo, receipt.URI)
	must.NotEqual(types.ExportStatusUploaded, receipt.Status, "a failed export must not report uploaded")
}

func TestWorkspaceHandoff_ExporterBuildFailure(t *testing.T) {
	must := require.New(t)
	build := errors.New("simulated factory failure")
	installFakeExporter(t, nil, build)
	cfg := &types.RunConfig{RunID: "run-1"}
	cfg.Executor.WorkspaceExportTo = "gs://stirrup-results/runs/run-1/workspace.tar.gz"

	receipt, err := workspaceHandoff{primaryRunID: "run-1"}.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.ErrorIs(err, build)
	must.NotNil(receipt, "a failed exporter build still returns a receipt")
}

func TestWorkspaceHandoff_FollowUpExportsToRunScopedURI(t *testing.T) {
	must := require.New(t)
	rec := &recordingExporter{}
	installRecordingExporter(t, rec)
	cfg := &types.RunConfig{RunID: "primary"}
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"
	h := workspaceHandoff{primaryRunID: "primary"}

	_, err := h.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.NoError(err)
	cfg.RunID = "run-2"
	receipt, err := h.Handoff(context.Background(), cfg, &archiverExecutor{})
	must.NoError(err)
	want := []string{"gs://bucket/runs/primary/workspace.tar.gz", "gs://bucket/runs/primary/run-2/workspace.tar.gz"}
	must.Equal(want, rec.destinations())
	must.Equal(want[1], receipt.URI)
}

func TestBuildRunResult_CarriesDegradedCapabilities(t *testing.T) {
	t.Run("reports degraded capabilities", func(t *testing.T) {
		must := require.New(t)
		rt := &types.RunTrace{
			ID:      "run-degraded",
			Outcome: "success",
			DegradedCapabilities: []types.DegradedCapability{
				{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"},
			},
		}
		got := buildRunResult(rt, types.DefaultMaxFinalAssistantTextBytes)
		must.Len(got.DegradedCapabilities, 1)
		must.Equal("docs", got.DegradedCapabilities[0].Name)
	})

	t.Run("omits the field when nothing degraded", func(t *testing.T) {
		must := require.New(t)
		result := buildRunResult(&types.RunTrace{ID: "clean", Outcome: "success"}, types.DefaultMaxFinalAssistantTextBytes)
		raw, err := json.Marshal(result)
		must.NoError(err)
		must.NotContains(string(raw), "degradedCapabilities")
	})
}
