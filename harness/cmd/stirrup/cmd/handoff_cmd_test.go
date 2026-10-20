package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// slowExporter takes a moment to upload, then counts the completed
// upload, so a test can tell whether "done" arrived before it finished.
type slowExporter struct {
	err       error
	completed atomic.Int32
}

func (e *slowExporter) Export(_ context.Context, _ executor.WorkspaceArchiver, dest string) (types.ExportReceipt, error) {
	time.Sleep(50 * time.Millisecond)
	e.completed.Add(1)
	return fakeExportResult(dest, "", e.err)
}

func installSlowExporter(t *testing.T, exp *slowExporter) {
	t.Helper()
	prev := newWorkspaceExporter
	t.Cleanup(func() { newWorkspaceExporter = prev })
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) { return exp, nil }
}

// stdioDoneEvent returns the single "done" event from captured stdout,
// which carries the stdio transport's JSON event stream.
func stdioDoneEvent(t *testing.T, stdout string) types.HarnessEvent {
	t.Helper()
	must := require.New(t)
	var dones []types.HarnessEvent
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev types.HarnessEvent
		if err := json.Unmarshal([]byte(line), &ev); err == nil && ev.Type == "done" {
			dones = append(dones, ev)
		}
	}
	must.Len(dones, 1, "stdout:\n%s", stdout)
	return dones[0]
}

// runFinishedTrace reads the run summary from a JSONL trace file.
func runFinishedTrace(t *testing.T, path string) types.RunTrace {
	t.Helper()
	must := require.New(t)
	f, err := os.Open(path)
	must.NoError(err)
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var ev struct {
			Kind  string          `json:"kind"`
			Trace *types.RunTrace `json:"trace"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil && ev.Kind == "run_finished" && ev.Trace != nil {
			return *ev.Trace
		}
	}
	must.FailNowf("no run_finished event", "in %s", path)
	return types.RunTrace{}
}

func exportRunConfig(t *testing.T, baseURL string, required *bool) (*types.RunConfig, string) {
	t.Helper()
	config := runOutcomeTestConfig(t, baseURL)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	config.TraceEmitter = types.TraceEmitterConfig{Type: "jsonl", FilePath: tracePath}
	config.Executor.WorkspaceExportTo = "gs://bucket/runs/cmd-runoutcome-test/workspace.tar.gz"
	config.Executor.WorkspaceExportRequired = required
	return config, tracePath
}

func TestRunWithConfig_RequiredExportFailureReportsHandoffFailed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	installSlowExporter(t, &slowExporter{err: errors.New("bucket denied")})
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	config, tracePath := exportRunConfig(t, srv.URL, nil)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Error(runErr)
	check.Equal(1, classifyExitCode(runErr))

	done := stdioDoneEvent(t, stdout)
	check.Equal("handoff_failed", done.StopReason)
	must.NotNil(done.Trace)
	check.Equal("handoff_failed", done.Trace.Outcome)

	result := decodeStirrupResult(t, stdout)
	check.Equal("handoff_failed", result.Outcome)
	check.Contains(result.Error, "bucket denied")

	r := result.WorkspaceExport
	must.NotNil(r)
	check.Equal(types.ExportStatusFailed, r.Status)
	check.True(r.Required)

	rt := runFinishedTrace(t, tracePath)
	check.Equal("handoff_failed", rt.Outcome)
	check.NotNil(rt.WorkspaceExport)
}

func TestRunWithConfig_OptionalExportFailureKeepsSuccess(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	installSlowExporter(t, &slowExporter{err: errors.New("bucket denied")})
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	optional := false
	config, _ := exportRunConfig(t, srv.URL, &optional)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.NoError(runErr)

	result := decodeStirrupResult(t, stdout)
	check.Equal("success", result.Outcome)

	r := result.WorkspaceExport
	must.NotNil(r)
	check.Equal(types.ExportStatusFailed, r.Status)
	check.False(r.Required)
	check.Contains(r.Error, "bucket denied")

	check.Empty(result.Error)
}

func TestRunWithConfig_FailedRunStillExports(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exp := &slowExporter{}
	installSlowExporter(t, exp)
	srv := openAICompatibleErrorServer(t)
	defer srv.Close()
	config, _ := exportRunConfig(t, srv.URL, nil)

	stdoutDone := captureStdout(t)
	_ = runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Equal(int32(1), exp.completed.Load(), "exports for a failed run")

	result := decodeStirrupResult(t, stdout)
	check.Equal("error", result.Outcome)
	check.NotEmpty(result.Error)

	r := result.WorkspaceExport
	must.NotNil(r)
	check.Equal(types.ExportStatusUploaded, r.Status)
}

func handoffJobTask(t *testing.T, providerURL string) *pb.RunConfig {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	task := followUpJobConfig(t, providerURL, 30, 0)
	task.FollowUpGrace = nil
	task.Executor.WorkspaceExportTo = "gs://bucket/runs/job-followup-primary/workspace.tar.gz"
	return task
}

func runHandoffJob(t *testing.T, task *pb.RunConfig, exp *slowExporter) (done *pb.HarnessEvent, uploadedBeforeDone int32, runErr error) {
	t.Helper()
	must := require.New(t)
	useTempMarkerPaths(t)
	installSlowExporter(t, exp)
	srv := newScriptedControlPlane(task, func(int) *pb.ControlEvent { return nil })
	var seen atomic.Int32
	seen.Store(-1)
	srv.onEvent = func(ev *pb.HarnessEvent) *pb.ControlEvent {
		if ev.Type == "done" {
			seen.Store(exp.completed.Load())
		}
		return nil
	}
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	srv.waitDone(t, "the job run")
	select {
	case runErr = <-errCh:
	case <-time.After(15 * time.Second):
		must.FailNow("runJob did not return")
	}
	for _, ev := range srv.events() {
		if ev.Type == "done" {
			done = ev
		}
	}
	return done, seen.Load(), runErr
}

func TestRunJob_ExportCompletesBeforeDone(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exp := &slowExporter{}

	done, uploaded, runErr := runHandoffJob(t, handoffJobTask(t, provider.URL), exp)
	must.NoError(runErr)
	check.Equal(int32(1), uploaded, "uploads completed when done arrived")

	check.Equal("success", done.GetStopReason())
	check.Equal("success", done.GetTrace().GetOutcome())

	r := done.GetTrace().GetWorkspaceExport()
	check.Equal("uploaded", r.GetStatus())
	check.True(r.GetRequired())
	check.Equal("gs://bucket/runs/job-followup-primary/workspace.tar.gz", r.GetUri())

	must.Len(sink.calls, 1)
	must.NotNil(sink.calls[0].WorkspaceExport)
	check.Equal(types.ExportStatusUploaded, sink.calls[0].WorkspaceExport.Status)
}

func TestRunJob_RequiredExportFailureReportsHandoffFailed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exp := &slowExporter{err: errors.New("bucket denied")}

	done, uploaded, runErr := runHandoffJob(t, handoffJobTask(t, provider.URL), exp)
	must.Error(runErr)
	check.Equal(1, classifyExitCode(runErr))
	check.Equal(int32(1), uploaded, "upload attempts finished when done arrived")

	check.Equal("handoff_failed", done.GetStopReason())
	check.Equal("handoff_failed", done.GetTrace().GetOutcome())
	check.Contains(done.GetTrace().GetError(), "bucket denied")
	check.Equal("failed", done.GetTrace().GetWorkspaceExport().GetStatus())

	must.Len(sink.calls, 1)
	check.Equal("handoff_failed", sink.calls[0].Outcome)
	check.Contains(sink.calls[0].Error, "bucket denied")
}

func TestRunJob_WorkspaceExportRequiredFalseFromWire(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	task := handoffJobTask(t, provider.URL)
	optional := false
	task.Executor.WorkspaceExportRequired = &optional

	done, _, runErr := runHandoffJob(t, task, &slowExporter{err: errors.New("bucket denied")})
	must.NoError(runErr)
	check.Equal("success", done.GetStopReason())
	check.False(done.GetTrace().GetWorkspaceExport().GetRequired())
}

// TestApplyOverrides_ExportWorkspaceRequiredTriState pins that an unset
// flag keeps the --config value, while an explicit value, including
// false, overrides it.
func TestApplyOverrides_ExportWorkspaceRequiredTriState(t *testing.T) {
	fileValue := true
	cases := []struct {
		name string
		args []string
		want *bool
	}{
		{name: "unset flag keeps file value", args: nil, want: &fileValue},
		{name: "bare flag sets true", args: []string{"--export-workspace-required"}, want: ptrBool(true)},
		{name: "false value overrides file value", args: []string{"--export-workspace-required=false"}, want: ptrBool(false)},
		{name: "true value sets true", args: []string{"--export-workspace-required=true"}, want: ptrBool(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			must.NoError(cmd.ParseFlags(tc.args))
			cfg := baseFileConfig()
			cfg.Executor.WorkspaceExportRequired = &fileValue
			must.NoError(applyOverrides(cmd, cfg, nil))
			got := cfg.Executor.WorkspaceExportRequired
			must.NotNil(got)
			must.Equal(*tc.want, *got)
		})
	}
}

func TestRunConfigCommand_RegistersExportWorkspaceRequired(t *testing.T) {
	must := require.New(t)
	must.NotNil(runConfigCmd.Flags().Lookup("export-workspace-required"),
		"run-config should accept --export-workspace-required, which maps to executor.workspaceExportRequired")
}

func ptrBool(v bool) *bool { return &v }

func TestPrintRunSummary_ErrorAndWorkspaceExportLines(t *testing.T) {
	check := assert.New(t)
	rt := outputModeRunTrace()
	rt.Outcome = "handoff_failed"
	rt.Error = "workspace export: bucket denied"
	rt.WorkspaceExport = &types.ExportReceipt{URI: "gs://b/ws.tar.gz", Status: types.ExportStatusFailed, Required: true}

	stderrDone := captureStderr(t)
	printRunSummary(rt)
	stderr := stderrDone()
	check.Contains(stderr, "Error: workspace export: bucket denied\n")
	check.Contains(stderr, "Workspace export: failed (gs://b/ws.tar.gz)\n")

	stderrDone = captureStderr(t)
	printRunSummary(outputModeRunTrace())
	stderr = stderrDone()
	check.NotContains(stderr, "Error:")
	check.NotContains(stderr, "Workspace export:")
}
