package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// fakeHandoff is a WorkspaceHandoff that records the context and config
// it saw, runs onHandoff first, and returns a fixed result.
type fakeHandoff struct {
	mu        sync.Mutex
	calls     int
	ctx       ctxObservation
	runID     string
	err       error
	noExport  bool
	onHandoff func(ctx context.Context)
}

func (h *fakeHandoff) Handoff(ctx context.Context, config *types.RunConfig, _ executor.Executor) (*types.ExportReceipt, error) {
	if h.onHandoff != nil {
		h.onHandoff(ctx)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.ctx = observe(ctx)
	h.runID = config.RunID
	if h.noExport {
		return nil, nil
	}
	receipt := &types.ExportReceipt{URI: config.Executor.WorkspaceExportTo, Status: types.ExportStatusUploaded}
	return receipt, h.err
}

func (h *fakeHandoff) observed() (int, ctxObservation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.ctx
}

func exportConfig(required *bool) *types.RunConfig {
	config := buildTestConfig()
	config.Executor.WorkspaceExportTo = "gs://bucket/runs/test-run-1/workspace.tar.gz"
	config.Executor.WorkspaceExportRequired = required
	return config
}

func endTurnProvider() *mockProvider {
	return &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}
}

func toolUseProvider() *mockProvider {
	return &mockProvider{events: []types.StreamEvent{
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use"},
	}}
}

func doneEvent(t *testing.T, events []types.HarnessEvent) types.HarnessEvent {
	t.Helper()
	var dones []types.HarnessEvent
	for _, e := range events {
		if e.Type == "done" {
			dones = append(dones, e)
		}
	}
	if len(dones) != 1 {
		t.Fatalf("got %d done events, want exactly 1: %+v", len(dones), events)
	}
	return dones[0]
}

func TestRun_RequiredExportFailureYieldsHandoffFailed(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	loop.Handoff = &fakeHandoff{err: errors.New("upload refused")}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "handoff_failed" {
		t.Fatalf("Outcome = %q, want handoff_failed", runTrace.Outcome)
	}
	want := types.ExportReceipt{
		URI:      "gs://bucket/runs/test-run-1/workspace.tar.gz",
		Status:   types.ExportStatusFailed,
		Required: true,
		Error:    "upload refused",
	}
	if runTrace.WorkspaceExport == nil || *runTrace.WorkspaceExport != want {
		t.Errorf("WorkspaceExport = %+v, want %+v", runTrace.WorkspaceExport, want)
	}
	if runTrace.Error != "workspace export: upload refused" {
		t.Errorf("Error = %q, want the export failure", runTrace.Error)
	}

	done := doneEvent(t, tr.Events())
	if done.StopReason != "handoff_failed" {
		t.Errorf("done StopReason = %q, want handoff_failed", done.StopReason)
	}
	if done.Trace == nil {
		t.Fatal("done carries no trace summary")
	}
	if done.Trace.Outcome != "handoff_failed" || done.Trace.Error != runTrace.Error {
		t.Errorf("done trace outcome/error = %q/%q, want handoff_failed/%q", done.Trace.Outcome, done.Trace.Error, runTrace.Error)
	}
	if done.Trace.WorkspaceExport == nil || *done.Trace.WorkspaceExport != want {
		t.Errorf("done trace WorkspaceExport = %+v, want %+v", done.Trace.WorkspaceExport, want)
	}
}

func TestRun_OptionalExportFailureKeepsSuccess(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	loop.Handoff = &fakeHandoff{err: errors.New("upload refused")}
	optional := false

	runTrace, err := loop.Run(context.Background(), exportConfig(&optional))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	r := runTrace.WorkspaceExport
	if r == nil || r.Status != types.ExportStatusFailed || r.Required || r.Error != "upload refused" {
		t.Errorf("WorkspaceExport = %+v, want an optional failed receipt carrying the error", r)
	}
	if runTrace.Error != "" {
		t.Errorf("Error = %q, want empty for a successful run", runTrace.Error)
	}
}

func TestRun_SuccessfulExportRecordsReceipt(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	loop.Handoff = &fakeHandoff{}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	want := types.ExportReceipt{URI: "gs://bucket/runs/test-run-1/workspace.tar.gz", Status: types.ExportStatusUploaded, Required: true}
	if runTrace.Outcome != "success" || runTrace.WorkspaceExport == nil || *runTrace.WorkspaceExport != want {
		t.Errorf("outcome %q, WorkspaceExport %+v; want success and %+v", runTrace.Outcome, runTrace.WorkspaceExport, want)
	}
}

func TestRun_ExportFailureNeverReplacesEarlierFailure(t *testing.T) {
	loop := buildTestLoop(toolUseProvider())
	handoff := &fakeHandoff{err: errors.New("upload refused")}
	loop.Handoff = handoff
	config := exportConfig(nil)
	config.MaxTurns = 1

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "max_turns" {
		t.Fatalf("Outcome = %q, want max_turns", runTrace.Outcome)
	}
	if calls, _ := handoff.observed(); calls != 1 {
		t.Errorf("handoff calls = %d, want 1: a failed run still exports its workspace", calls)
	}
	if r := runTrace.WorkspaceExport; r == nil || r.Status != types.ExportStatusFailed || r.Error != "upload refused" {
		t.Errorf("WorkspaceExport = %+v, want the failed receipt recorded", r)
	}
	if runTrace.Error != "reached the turn limit (maxTurns 1)" {
		t.Errorf("Error = %q, want the earliest failure (the turn limit)", runTrace.Error)
	}
}

func TestRun_HandoffSkippedWithoutWorkspaceOrDestination(t *testing.T) {
	t.Run("executor none", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		handoff := &fakeHandoff{}
		loop.Handoff = handoff
		config := exportConfig(nil)
		config.Executor.Type = "none"
		runTrace, err := loop.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if calls, _ := handoff.observed(); calls != 0 || runTrace.WorkspaceExport != nil {
			t.Errorf("calls = %d, WorkspaceExport = %+v; want no handoff for an executor without a workspace", calls, runTrace.WorkspaceExport)
		}
	})
	t.Run("no destination", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		loop.Handoff = &fakeHandoff{noExport: true}
		runTrace, err := loop.Run(context.Background(), buildTestConfig())
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if runTrace.Outcome != "success" || runTrace.WorkspaceExport != nil {
			t.Errorf("outcome %q, WorkspaceExport %+v; want success with no receipt", runTrace.Outcome, runTrace.WorkspaceExport)
		}
	})
}

// orderLog records the terminal steps in the order they happen.
type orderLog struct {
	mu    sync.Mutex
	steps []string
}

func (o *orderLog) add(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, step)
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

type orderedEmitter struct {
	*ctxProbeEmitter
	log *orderLog
}

func (e *orderedEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	e.log.add("trace_finish")
	return e.ctxProbeEmitter.Finish(ctx, outcome)
}

type orderedTransport struct {
	cancellableTransport
	log *orderLog
}

func (t *orderedTransport) Emit(event types.HarnessEvent) error {
	if event.Type == "done" {
		t.log.add("done")
	}
	return t.cancellableTransport.Emit(event)
}

func TestRun_HandoffAndTraceFinishPrecedeDone(t *testing.T) {
	for name, earlyFailure := range map[string]bool{"normal path": false, "early failure path": true} {
		t.Run(name, func(t *testing.T) {
			log := &orderLog{}
			loop := buildTestLoop(endTurnProvider())
			loop.Transport = &orderedTransport{log: log}
			loop.Trace = &orderedEmitter{ctxProbeEmitter: newCtxProbeEmitter(), log: log}
			loop.Handoff = &fakeHandoff{onHandoff: func(context.Context) {
				time.Sleep(20 * time.Millisecond)
				log.add("handoff")
			}}
			if earlyFailure {
				loop.Hooks = &fakeHookRunner{preErr: errors.New("clone failed")}
			}

			if _, err := loop.Run(context.Background(), exportConfig(nil)); err != nil && !earlyFailure {
				t.Fatalf("Run() error: %v", err)
			}
			got := strings.Join(log.snapshot(), ",")
			if got != "handoff,trace_finish,done" {
				t.Errorf("terminal order = %s, want handoff,trace_finish,done", got)
			}
		})
	}
}

func TestRun_EarlyFailureStillExportsAndReportsError(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	handoff := &fakeHandoff{err: errors.New("upload refused")}
	loop.Handoff = handoff
	loop.Hooks = &fakeHookRunner{preErr: errors.New("clone failed")}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	if err == nil {
		t.Fatal("Run() returned no error for a fatal pre-run hook")
	}
	if runTrace.Outcome != "setup_failed" {
		t.Fatalf("Outcome = %q, want setup_failed kept over the export failure", runTrace.Outcome)
	}
	if calls, _ := handoff.observed(); calls != 1 {
		t.Errorf("handoff calls = %d, want 1", calls)
	}
	if runTrace.Error != "pre-run hooks: clone failed" {
		t.Errorf("Error = %q, want the pre-run hook failure", runTrace.Error)
	}
	done := doneEvent(t, tr.Events())
	if done.Trace == nil || done.Trace.Outcome != "setup_failed" || done.Trace.WorkspaceExport == nil {
		t.Errorf("done trace = %+v, want the setup_failed summary with the export receipt", done.Trace)
	}
}

func TestRun_ControlPlaneCancelDuringHandoffDoesNotInterruptIt(t *testing.T) {
	tr := &cancellableTransport{}
	loop := buildTestLoop(endTurnProvider())
	loop.Transport = tr
	loop.ensureControlRouting()
	t.Cleanup(func() { _ = loop.Close() })
	handoff := &fakeHandoff{onHandoff: func(ctx context.Context) {
		tr.FireControl(types.ControlEvent{Type: "cancel"})
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}}
	loop.Handoff = handoff

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	_, obs := handoff.observed()
	if obs.err != nil {
		t.Errorf("handoff ctx.Err() = %v after a cancel, want nil", obs.err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success (the run had finished before the cancel)", runTrace.Outcome)
	}
	if r := runTrace.WorkspaceExport; r == nil || r.Status != types.ExportStatusUploaded {
		t.Errorf("WorkspaceExport = %+v, want the completed upload", r)
	}
	if !loop.sessionCancelled.Load() {
		t.Error("cancel during the handoff did not end the session")
	}
}

func TestRun_HandoffSurvivesRunDeadline(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = &fireAndCloseProvider{
		onStream: func() { time.Sleep(150 * time.Millisecond) },
	}
	handoff := &fakeHandoff{}
	loop.Handoff = handoff

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	runTrace, err := loop.Run(ctx, exportConfig(nil))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "timeout" || runTrace.Error != "reached the run timeout" {
		t.Errorf("outcome/error = %q/%q, want timeout/%q", runTrace.Outcome, runTrace.Error, "reached the run timeout")
	}
	_, obs := handoff.observed()
	assertLiveDetached(t, "Handoff", obs, handoffBudget, start)
}

func TestRun_HandoffAbandonedOnShutdown(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	shutdown, shutdownCancel := context.WithCancel(context.Background())
	shutdownCancel()
	loop.Shutdown = shutdown
	handoff := &fakeHandoff{}
	loop.Handoff = handoff

	if _, err := loop.Run(context.Background(), exportConfig(nil)); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if _, obs := handoff.observed(); !errors.Is(obs.err, context.Canceled) {
		t.Errorf("handoff ctx.Err() = %v, want context.Canceled once Shutdown fired", obs.err)
	}
}

func TestRun_TraceFinishFailureReportedInErrorNotOutcome(t *testing.T) {
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	emitter := newCtxProbeEmitter()
	emitter.finishErr = errors.New("gcs upload: 503")
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err == nil || !strings.Contains(err.Error(), "finish trace") {
		t.Fatalf("Run() error = %v, want the trace finish failure", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success: a trace upload failure never yields handoff_failed", runTrace.Outcome)
	}
	if runTrace.Error != "finish trace: gcs upload: 503" {
		t.Errorf("Error = %q, want the trace finish failure", runTrace.Error)
	}
	if done := doneEvent(t, tr.Events()); done.Trace == nil || done.Trace.Error != runTrace.Error {
		t.Errorf("done trace = %+v, want it to carry the trace finish failure", done.Trace)
	}
}

func TestRun_ErrorTextNamesTheFailureForEveryCoveredOutcome(t *testing.T) {
	t.Run("verification_error", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		loop.Verifier = &errorVerifier{}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		if runTrace.Outcome != "verification_error" || runTrace.Error != "verifier: verifier transport failed" {
			t.Errorf("outcome/error = %q/%q, want verification_error/verifier: verifier transport failed", runTrace.Outcome, runTrace.Error)
		}
	})
	t.Run("verification_failed", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		loop.Verifier = &failingVerifier{}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		if runTrace.Outcome != "verification_failed" || runTrace.Error != "verification failed after the final retry" {
			t.Errorf("outcome/error = %q/%q, want verification_failed with its description", runTrace.Outcome, runTrace.Error)
		}
	})
	t.Run("hook_failed", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		loop.Hooks = &fakeHookRunner{postErr: errors.New("notify failed")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		if runTrace.Outcome != "hook_failed" || runTrace.Error != "post-run hooks: notify failed" {
			t.Errorf("outcome/error = %q/%q, want hook_failed/post-run hooks: notify failed", runTrace.Outcome, runTrace.Error)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		loop := buildTestLoop(nil)
		loop.Provider = &errorProvider{err: errors.New("HTTP 500")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		if runTrace.Outcome != "error" || !strings.Contains(runTrace.Error, "HTTP 500") {
			t.Errorf("outcome/error = %q/%q, want error naming the provider failure", runTrace.Outcome, runTrace.Error)
		}
	})
	t.Run("build system prompt", func(t *testing.T) {
		loop := buildTestLoop(endTurnProvider())
		loop.Prompt = &failingPromptBuilder{err: errors.New("template missing")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		if runTrace.Outcome != "error" || runTrace.Error != "build system prompt: template missing" {
			t.Errorf("outcome/error = %q/%q, want error naming the prompt build failure", runTrace.Outcome, runTrace.Error)
		}
	})
}

func TestRunErrorText(t *testing.T) {
	config := &types.RunConfig{MaxTurns: 7}
	loop := &AgenticLoop{}

	if got := loop.runErrorText(config, "success"); got != "" {
		t.Errorf("success: got %q, want empty", got)
	}
	if got := loop.runErrorText(config, "refusal"); got != `run ended with outcome "refusal"` {
		t.Errorf("unlisted outcome: got %q", got)
	}

	loop.noteFailure("verification_error", "verifier: boom")
	if got := loop.runErrorText(config, "cancelled"); got != "cancelled before the run completed" {
		t.Errorf("reclassified outcome: got %q, want the cancellation description", got)
	}

	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	loop.noteFailure("error", "provider: key "+awsKey+" "+strings.Repeat("x", 2*maxFailureTextBytes))
	got := loop.runErrorText(config, "error")
	if strings.Contains(got, awsKey) {
		t.Errorf("error text leaks a secret-shaped token: %q", got[:80])
	}
	if !strings.HasSuffix(got, "... [truncated by harness]") || len(got) > maxFailureTextBytes+len("... [truncated by harness]") {
		t.Errorf("error text length %d, want it capped at %d bytes plus the marker", len(got), maxFailureTextBytes)
	}
}
