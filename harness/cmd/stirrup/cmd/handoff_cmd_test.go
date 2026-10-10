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

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// slowExporter takes a moment to upload, then counts the completed
// upload, so a test can tell whether "done" arrived before it finished.
type slowExporter struct {
	err       error
	completed atomic.Int32
}

func (e *slowExporter) Export(context.Context, string, string) error {
	time.Sleep(50 * time.Millisecond)
	e.completed.Add(1)
	return e.err
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
	if len(dones) != 1 {
		t.Fatalf("got %d done events on stdout, want 1:\n%s", len(dones), stdout)
	}
	return dones[0]
}

// runFinishedTrace reads the run summary from a JSONL trace file.
func runFinishedTrace(t *testing.T, path string) types.RunTrace {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
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
	t.Fatalf("no run_finished event in %s", path)
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
	installSlowExporter(t, &slowExporter{err: errors.New("bucket denied")})
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	config, tracePath := exportRunConfig(t, srv.URL, nil)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr == nil || classifyExitCode(runErr) != 1 {
		t.Errorf("runWithConfig error = %v (exit %d), want a non-zero exit", runErr, classifyExitCode(runErr))
	}
	done := stdioDoneEvent(t, stdout)
	if done.StopReason != "handoff_failed" || done.Trace == nil || done.Trace.Outcome != "handoff_failed" {
		t.Errorf("done = %+v, want stop reason and trace outcome handoff_failed", done)
	}
	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "handoff_failed" {
		t.Errorf("RunResult outcome = %q, want handoff_failed", result.Outcome)
	}
	if !strings.Contains(result.Error, "bucket denied") {
		t.Errorf("RunResult error = %q, want the export failure", result.Error)
	}
	if r := result.WorkspaceExport; r == nil || r.Status != types.ExportStatusFailed || !r.Required {
		t.Errorf("RunResult workspaceExport = %+v, want a failed required receipt", r)
	}
	if rt := runFinishedTrace(t, tracePath); rt.Outcome != "handoff_failed" || rt.WorkspaceExport == nil {
		t.Errorf("trace outcome = %q, workspaceExport = %+v; want handoff_failed with the receipt", rt.Outcome, rt.WorkspaceExport)
	}
}

func TestRunWithConfig_OptionalExportFailureKeepsSuccess(t *testing.T) {
	installSlowExporter(t, &slowExporter{err: errors.New("bucket denied")})
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	optional := false
	config, _ := exportRunConfig(t, srv.URL, &optional)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr != nil {
		t.Errorf("runWithConfig error = %v, want nil for an optional export failure", runErr)
	}
	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "success" {
		t.Errorf("RunResult outcome = %q, want success", result.Outcome)
	}
	r := result.WorkspaceExport
	if r == nil || r.Status != types.ExportStatusFailed || r.Required || !strings.Contains(r.Error, "bucket denied") {
		t.Errorf("RunResult workspaceExport = %+v, want status failed with the error text", r)
	}
	if result.Error != "" {
		t.Errorf("RunResult error = %q, want empty on success", result.Error)
	}
}

func TestRunWithConfig_FailedRunStillExports(t *testing.T) {
	exp := &slowExporter{}
	installSlowExporter(t, exp)
	srv := openAICompatibleErrorServer(t)
	defer srv.Close()
	config, _ := exportRunConfig(t, srv.URL, nil)

	stdoutDone := captureStdout(t)
	_ = runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if n := exp.completed.Load(); n != 1 {
		t.Fatalf("exports = %d, want 1 for a failed run", n)
	}
	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "error" {
		t.Errorf("RunResult outcome = %q, want error", result.Outcome)
	}
	if result.Error == "" {
		t.Error("RunResult error is empty for a failed run")
	}
	if r := result.WorkspaceExport; r == nil || r.Status != types.ExportStatusUploaded {
		t.Errorf("RunResult workspaceExport = %+v, want the uploaded receipt", r)
	}
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
		t.Fatal("runJob did not return")
	}
	for _, ev := range srv.events() {
		if ev.Type == "done" {
			done = ev
		}
	}
	return done, seen.Load(), runErr
}

func TestRunJob_ExportCompletesBeforeDone(t *testing.T) {
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exp := &slowExporter{}

	done, uploaded, runErr := runHandoffJob(t, handoffJobTask(t, provider.URL), exp)
	if runErr != nil {
		t.Fatalf("runJob error = %v, want nil", runErr)
	}
	if uploaded != 1 {
		t.Errorf("uploads completed when done arrived = %d, want 1", uploaded)
	}
	if done.GetStopReason() != "success" || done.GetTrace().GetOutcome() != "success" {
		t.Errorf("done = %v, want success with the run summary", done)
	}
	r := done.GetTrace().GetWorkspaceExport()
	if r.GetStatus() != "uploaded" || !r.GetRequired() || r.GetUri() != "gs://bucket/runs/job-followup-primary/workspace.tar.gz" {
		t.Errorf("done.trace.workspace_export = %v, want the uploaded required receipt", r)
	}
	if len(sink.calls) != 1 || sink.calls[0].WorkspaceExport == nil || sink.calls[0].WorkspaceExport.Status != types.ExportStatusUploaded {
		t.Errorf("RunResults = %+v, want one carrying the uploaded receipt", sink.calls)
	}
}

func TestRunJob_RequiredExportFailureReportsHandoffFailed(t *testing.T) {
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exp := &slowExporter{err: errors.New("bucket denied")}

	done, uploaded, runErr := runHandoffJob(t, handoffJobTask(t, provider.URL), exp)
	if runErr == nil || classifyExitCode(runErr) != 1 {
		t.Errorf("runJob error = %v (exit %d), want a non-zero exit", runErr, classifyExitCode(runErr))
	}
	if uploaded != 1 {
		t.Errorf("upload attempts finished when done arrived = %d, want 1", uploaded)
	}
	if done.GetStopReason() != "handoff_failed" || done.GetTrace().GetOutcome() != "handoff_failed" {
		t.Errorf("done = %v, want handoff_failed", done)
	}
	if !strings.Contains(done.GetTrace().GetError(), "bucket denied") || done.GetTrace().GetWorkspaceExport().GetStatus() != "failed" {
		t.Errorf("done.trace = %v, want the export failure and a failed receipt", done.GetTrace())
	}
	if len(sink.calls) != 1 || sink.calls[0].Outcome != "handoff_failed" || !strings.Contains(sink.calls[0].Error, "bucket denied") {
		t.Errorf("RunResults = %+v, want one handoff_failed result carrying the export failure", sink.calls)
	}
}

func TestRunJob_WorkspaceExportRequiredFalseFromWire(t *testing.T) {
	provider, _ := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	task := handoffJobTask(t, provider.URL)
	optional := false
	task.Executor.WorkspaceExportRequired = &optional

	done, _, runErr := runHandoffJob(t, task, &slowExporter{err: errors.New("bucket denied")})
	if runErr != nil {
		t.Errorf("runJob error = %v, want nil for an optional export failure", runErr)
	}
	if done.GetStopReason() != "success" || done.GetTrace().GetWorkspaceExport().GetRequired() {
		t.Errorf("done = %v, want success with an optional receipt", done)
	}
}

// TestApplyOverrides_ExportWorkspaceRequiredTriState pins that an unset
// flag keeps the --config value, while an explicit value, including
// false, overrides it.
func TestApplyOverrides_ExportWorkspaceRequiredTriState(t *testing.T) {
	fileValue := true
	cases := []struct {
		args []string
		want *bool
	}{
		{nil, &fileValue},
		{[]string{"--export-workspace-required"}, ptrBool(true)},
		{[]string{"--export-workspace-required=false"}, ptrBool(false)},
		{[]string{"--export-workspace-required=true"}, ptrBool(true)},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := newTestHarnessCommand()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			cfg := baseFileConfig()
			cfg.Executor.WorkspaceExportRequired = &fileValue
			if err := applyOverrides(cmd, cfg, nil); err != nil {
				t.Fatalf("applyOverrides: %v", err)
			}
			got := cfg.Executor.WorkspaceExportRequired
			if got == nil || *got != *tc.want {
				t.Errorf("WorkspaceExportRequired = %v, want %v", got, *tc.want)
			}
		})
	}
}

func TestRunConfigCommand_RegistersExportWorkspaceRequired(t *testing.T) {
	if runConfigCmd.Flags().Lookup("export-workspace-required") == nil {
		t.Error("stirrup run-config does not accept --export-workspace-required, which now maps to executor.workspaceExportRequired")
	}
}

func ptrBool(v bool) *bool { return &v }

func TestPrintRunSummary_ErrorAndWorkspaceExportLines(t *testing.T) {
	rt := outputModeRunTrace()
	rt.Outcome = "handoff_failed"
	rt.Error = "workspace export: bucket denied"
	rt.WorkspaceExport = &types.ExportReceipt{URI: "gs://b/ws.tar.gz", Status: types.ExportStatusFailed, Required: true}

	stderrDone := captureStderr(t)
	printRunSummary(rt)
	stderr := stderrDone()
	for _, want := range []string{"Error: workspace export: bucket denied\n", "Workspace export: failed (gs://b/ws.tar.gz)\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q, got: %q", want, stderr)
		}
	}

	stderrDone = captureStderr(t)
	printRunSummary(outputModeRunTrace())
	if stderr := stderrDone(); strings.Contains(stderr, "Error:") || strings.Contains(stderr, "Workspace export:") {
		t.Errorf("a clean run printed error or export lines: %q", stderr)
	}
}
