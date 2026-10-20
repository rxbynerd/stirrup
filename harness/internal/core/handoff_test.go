package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	status    string
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
	if h.status != "" {
		receipt.Status = h.status
	}
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
	must := require.New(t)
	var dones []types.HarnessEvent
	for _, e := range events {
		if e.Type == "done" {
			dones = append(dones, e)
		}
	}
	must.Len(dones, 1, "want exactly one done event in %+v", events)
	return dones[0]
}

func TestRun_RequiredExportFailureYieldsHandoffFailed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	loop.Handoff = &fakeHandoff{err: errors.New("upload refused")}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)
	must.Equal("handoff_failed", runTrace.Outcome)
	want := types.ExportReceipt{
		URI:      "gs://bucket/runs/test-run-1/workspace.tar.gz",
		Status:   types.ExportStatusFailed,
		Required: true,
		Error:    "upload refused",
	}
	check.Equal(&want, runTrace.WorkspaceExport)
	check.Equal("workspace export: upload refused", runTrace.Error)

	done := doneEvent(t, tr.Events())
	check.Equal("handoff_failed", done.StopReason)
	must.NotNil(done.Trace, "done event carries a trace summary")
	check.Equal("handoff_failed", done.Trace.Outcome)
	check.Equal(runTrace.Error, done.Trace.Error)
	check.Equal(&want, done.Trace.WorkspaceExport)
}

func TestRun_ExportErrorIsScrubbedAndCapped(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	long := "upload refused for " + spanProbeKey + " " + strings.Repeat("x", 2*maxFailureTextBytes)
	loop.Handoff = &fakeHandoff{err: errors.New(long)}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)
	must.NotNil(runTrace.WorkspaceExport, "want a failed receipt")
	done := doneEvent(t, tr.Events())
	must.NotNil(done.Trace, "done event carries a trace summary")
	must.NotNil(done.Trace.WorkspaceExport, "done trace carries the export receipt")

	maxLen := maxFailureTextBytes + len("... [truncated by harness]")
	texts := []struct {
		name string
		text string
	}{
		{name: "run trace export error", text: runTrace.WorkspaceExport.Error},
		{name: "run trace error", text: runTrace.Error},
		{name: "done trace export error", text: done.Trace.WorkspaceExport.Error},
	}
	for _, tc := range texts {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(tc.text, spanProbeKey, "text carries the unscrubbed key")
			must.NotEmpty(tc.text)
			must.LessOrEqual(len(tc.text), maxLen, "text is capped near %d bytes", maxFailureTextBytes)
		})
	}
}

func TestRun_EmptyRequiredExportIsDeliveredSuccess(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	loop.Handoff = &fakeHandoff{status: types.ExportStatusEmpty}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)
	must.Equal("success", runTrace.Outcome, "an empty archive that reached the destination is a success")

	done := doneEvent(t, tr.Events())
	must.NotNil(done.Trace, "done event carries a trace summary")
	receipts := []struct {
		name    string
		receipt *types.ExportReceipt
	}{
		{name: "run trace", receipt: runTrace.WorkspaceExport},
		{name: "done trace", receipt: done.Trace.WorkspaceExport},
	}
	for _, tc := range receipts {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotNil(tc.receipt)
			must.Equal(types.ExportStatusEmpty, tc.receipt.Status)
			must.True(tc.receipt.Delivered())
			must.True(tc.receipt.Required)
			must.Empty(tc.receipt.Error)
		})
	}
}

func TestRun_OptionalExportFailureKeepsSuccess(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(endTurnProvider())
	loop.Handoff = &fakeHandoff{err: errors.New("upload refused")}
	optional := false

	runTrace, err := loop.Run(context.Background(), exportConfig(&optional))
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	r := runTrace.WorkspaceExport
	must.NotNil(r, "an optional failed receipt is recorded")
	check.Equal(types.ExportStatusFailed, r.Status)
	check.False(r.Required)
	check.Equal("upload refused", r.Error)
	check.Empty(runTrace.Error, "a successful run has no error")
}

func TestRun_SuccessfulExportRecordsReceipt(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(endTurnProvider())
	loop.Handoff = &fakeHandoff{}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)
	want := types.ExportReceipt{URI: "gs://bucket/runs/test-run-1/workspace.tar.gz", Status: types.ExportStatusUploaded, Required: true}
	check.Equal("success", runTrace.Outcome)
	check.Equal(&want, runTrace.WorkspaceExport)
}

func TestRun_ExportFailureNeverReplacesEarlierFailure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(toolUseProvider())
	handoff := &fakeHandoff{err: errors.New("upload refused")}
	loop.Handoff = handoff
	config := exportConfig(nil)
	config.MaxTurns = 1

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("max_turns", runTrace.Outcome)
	calls, _ := handoff.observed()
	check.Equal(1, calls, "a failed run still exports its workspace")
	r := runTrace.WorkspaceExport
	must.NotNil(r, "the failed receipt is recorded")
	check.Equal(types.ExportStatusFailed, r.Status)
	check.Equal("upload refused", r.Error)
	check.Equal("reached the turn limit (maxTurns 1)", runTrace.Error, "the earliest failure (the turn limit) is kept")
}

func TestRun_HandoffSkippedWithoutWorkspaceOrDestination(t *testing.T) {
	t.Run("executor none", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		handoff := &fakeHandoff{}
		loop.Handoff = handoff
		config := exportConfig(nil)
		config.Executor.Type = "none"
		runTrace, err := loop.Run(context.Background(), config)
		must.NoError(err)
		calls, _ := handoff.observed()
		must.Zero(calls, "an executor without a workspace is not handed off")
		must.Nil(runTrace.WorkspaceExport)
	})
	t.Run("no destination", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		loop.Handoff = &fakeHandoff{noExport: true}
		runTrace, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.Equal("success", runTrace.Outcome)
		must.Nil(runTrace.WorkspaceExport)
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
	cases := []struct {
		name         string
		earlyFailure bool
	}{
		{name: "normal path"},
		{name: "early failure path", earlyFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			log := &orderLog{}
			loop := buildTestLoop(endTurnProvider())
			loop.Transport = &orderedTransport{log: log}
			loop.Trace = &orderedEmitter{ctxProbeEmitter: newCtxProbeEmitter(), log: log}
			loop.Handoff = &fakeHandoff{onHandoff: func(context.Context) {
				time.Sleep(20 * time.Millisecond)
				log.add("handoff")
			}}
			if tc.earlyFailure {
				loop.Hooks = &fakeHookRunner{preErr: errors.New("clone failed")}
			}

			_, err := loop.Run(context.Background(), exportConfig(nil))
			if !tc.earlyFailure {
				must.NoError(err)
			}
			must.Equal([]string{"handoff", "trace_finish", "done"}, log.snapshot(), "terminal steps run in this order")
		})
	}
}

func TestRun_EarlyFailureStillExportsAndReportsError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	handoff := &fakeHandoff{err: errors.New("upload refused")}
	loop.Handoff = handoff
	loop.Hooks = &fakeHookRunner{preErr: errors.New("clone failed")}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.Error(err, "a fatal pre-run hook fails the run")
	must.Equal("setup_failed", runTrace.Outcome, "setup_failed is kept over the export failure")
	calls, _ := handoff.observed()
	check.Equal(1, calls, "the workspace is still handed off")
	check.Equal("pre-run hooks: clone failed", runTrace.Error)

	done := doneEvent(t, tr.Events())
	must.NotNil(done.Trace, "done event carries a trace summary")
	check.Equal("setup_failed", done.Trace.Outcome)
	check.NotNil(done.Trace.WorkspaceExport, "done trace carries the export receipt")
}

func TestRun_ControlPlaneCancelDuringHandoffDoesNotInterruptIt(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	_, obs := handoff.observed()
	must.NoError(obs.err, "the handoff context stays live after a cancel")
	check.Equal("success", runTrace.Outcome, "the run had finished before the cancel")
	r := runTrace.WorkspaceExport
	must.NotNil(r, "the completed upload is recorded")
	check.Equal(types.ExportStatusUploaded, r.Status)
	check.True(loop.sessionCancelled.Load(), "a cancel during the handoff ends the session")
}

func TestRun_HandoffSurvivesRunDeadline(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	check.Equal("timeout", runTrace.Outcome)
	check.Equal("reached the run timeout", runTrace.Error)
	_, obs := handoff.observed()
	assertLiveDetached(t, "Handoff", obs, handoffBudget, start)
}

func TestRun_HandoffAbandonedOnShutdown(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(endTurnProvider())
	shutdown, shutdownCancel := context.WithCancel(context.Background())
	shutdownCancel()
	loop.Shutdown = shutdown
	handoff := &fakeHandoff{}
	loop.Handoff = handoff

	_, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)
	_, obs := handoff.observed()
	check.ErrorIs(obs.err, context.Canceled, "the handoff context is cancelled once Shutdown fires")
}

func TestRun_TraceFinishFailureReportedInErrorNotOutcome(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	emitter := newCtxProbeEmitter()
	emitter.finishErr = errors.New("gcs upload: 503")
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.ErrorContains(err, "finish trace", "Run() reports the trace finish failure")
	check.Equal("success", runTrace.Outcome, "a trace upload failure never yields handoff_failed")
	check.Equal("finish trace: gcs upload: 503", runTrace.Error)

	done := doneEvent(t, tr.Events())
	must.NotNil(done.Trace, "done event carries a trace summary")
	check.Equal(runTrace.Error, done.Trace.Error, "the done trace carries the trace finish failure")
}

func TestRun_ErrorTextNamesTheFailureForEveryCoveredOutcome(t *testing.T) {
	t.Run("verification error", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		loop.Verifier = &errorVerifier{}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		must.Equal("verification_error", runTrace.Outcome)
		must.Equal("verifier: verifier transport failed", runTrace.Error)
	})
	t.Run("verification failed", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		loop.Verifier = &failingVerifier{}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		must.Equal("verification_failed", runTrace.Outcome)
		must.Equal("verification failed after the final retry", runTrace.Error)
	})
	t.Run("hook failed", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		loop.Hooks = &fakeHookRunner{postErr: errors.New("notify failed")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		must.Equal("hook_failed", runTrace.Outcome)
		must.Equal("post-run hooks: notify failed", runTrace.Error)
	})
	t.Run("provider error", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(nil)
		loop.Provider = &errorProvider{err: errors.New("HTTP 500")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		must.Equal("error", runTrace.Outcome)
		must.Contains(runTrace.Error, "HTTP 500")
	})
	t.Run("build system prompt", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(endTurnProvider())
		loop.Prompt = &failingPromptBuilder{err: errors.New("template missing")}
		runTrace, _ := loop.Run(context.Background(), buildTestConfig())
		must.Equal("error", runTrace.Outcome)
		must.Equal("build system prompt: template missing", runTrace.Error)
	})
}

func TestRunErrorText_ForOutcome(t *testing.T) {
	check := assert.New(t)
	config := &types.RunConfig{MaxTurns: 7}
	loop := &AgenticLoop{}

	check.Empty(loop.runErrorText(config, "success"), "a successful run has no error text")
	check.Equal(`run ended with outcome "refusal"`, loop.runErrorText(config, "refusal"), "an outcome without a description")

	loop.noteFailure("verification_error", "verifier: boom")
	check.Equal("cancelled before the run completed", loop.runErrorText(config, "cancelled"), "a cancellation after a recorded failure")

	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const truncationMarker = "... [truncated by harness]"
	loop.noteFailure("error", "provider: key "+awsKey+" "+strings.Repeat("x", 2*maxFailureTextBytes))
	got := loop.runErrorText(config, "error")
	check.NotContains(got, awsKey, "error text must not leak a secret-shaped token")
	check.True(strings.HasSuffix(got, truncationMarker), "over-long error text ends with the truncation marker")
	check.LessOrEqual(len(got), maxFailureTextBytes+len(truncationMarker), "error text is capped at %d bytes plus the marker", maxFailureTextBytes)
}

func TestDoneSummary_CarriesTraceReference(t *testing.T) {
	t.Run("passes the trace reference through", func(t *testing.T) {
		must := require.New(t)
		ref := &types.TraceReference{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", RunSpanID: "00f067aa0ba902b7", Sampled: true}
		got := doneSummary(&types.RunTrace{Outcome: "success", TraceReference: ref})
		must.Same(ref, got.TraceReference)
	})
	t.Run("leaves the reference nil when the trace has none", func(t *testing.T) {
		must := require.New(t)
		got := doneSummary(&types.RunTrace{Outcome: "success"})
		must.Nil(got.TraceReference)
	})
}

func TestRun_SetupFailureReportsReferenceOnlyOnDone(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop, _ := otelLoop(t, endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	loop.emitReady = true
	loop.Hooks = &fakeHookRunner{preErr: errors.New("clone failed")}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.Error(err, "a fatal pre-run hook fails the run")
	for _, e := range tr.Events() {
		check.NotEqual("run_started", e.Type, "run_started is not sent for a run that failed setup")
	}
	must.NotNil(runTrace.TraceReference, "the run span starts before setup")
	done := doneEvent(t, tr.Events())
	must.NotNil(done.Trace, "done event carries a trace summary")
	check.Equal(runTrace.TraceReference, done.Trace.TraceReference)
}
