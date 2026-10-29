package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/health"
	"github.com/rxbynerd/stirrup/types"
)

// recordingTransport captures emitted HarnessEvents and optionally fails
// every Emit, so the terminal-signal helper can be exercised without a
// control plane.
type recordingTransport struct {
	events  []types.HarnessEvent
	emitErr error
}

func (r *recordingTransport) Emit(event types.HarnessEvent) error {
	r.events = append(r.events, event)
	return r.emitErr
}

func (r *recordingTransport) OnControl(func(types.ControlEvent)) {}

func (r *recordingTransport) Close() error { return nil }

// TestRunJob_MissingControlPlaneAddrIsPlain checks that the
// "CONTROL_PLANE_ADDR environment variable is required" error stays
// plain text with no ANSI escapes, since log aggregators ingest the
// job's stderr verbatim.
func TestRunJob_MissingControlPlaneAddrIsPlain(t *testing.T) {
	must := require.New(t)
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)
	must.ErrorContains(err, "CONTROL_PLANE_ADDR environment variable is required")
	must.NotContains(err.Error(), "\x1b[", "job error must not contain ANSI escapes (log aggregators ingest it verbatim)")
}

// fakeControlPlane implements just enough of HarnessServiceServer to drive
// runJob through the assignment wait and into a task_assignment carrying
// the given RunConfig. It then keeps the stream open until the harness
// half-closes it, as a control plane does while the task runs.
type fakeControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	readyRecv chan struct{}
	task      *pb.RunConfig

	// sendTask, if non-nil, gates the task_assignment send on the test
	// closing it. Without this, the assignment can reach runJob and clear
	// the readiness marker before the test observes it set, racing the
	// marker-scoped-to-assignment-wait assertion.
	sendTask chan struct{}

	// recordEvents, when true, keeps the stream open after the assignment
	// and appends every HarnessEvent the harness sends to received.
	recordEvents bool
	mu           sync.Mutex
	received     []*pb.HarnessEvent
}

func (s *fakeControlPlane) receivedEvents() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.received...)
}

func (s *fakeControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	close(s.readyRecv)

	if s.sendTask != nil {
		<-s.sendTask
	}

	if err := stream.Send(&pb.ControlEvent{Type: "task_assignment", Task: s.task}); err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		if !s.recordEvents {
			continue
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
	}
}

// startControlPlane serves srv over a real TCP listener (runJob only
// takes CONTROL_PLANE_ADDR as a string, so bufconn's dial-option injection
// isn't reachable here) and points CONTROL_PLANE_ADDR at it.
func startControlPlane(t *testing.T, srv pb.HarnessServiceServer) {
	t.Helper()
	must := require.New(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	t.Setenv("CONTROL_PLANE_ADDR", lis.Addr().String())
}

// useTempMarkerPaths points the package-level marker paths at t.TempDir()
// for the duration of the test, so it can't collide with a real
// /tmp/healthy or /tmp/ready on the host (or with another test/process
// sharing it).
func useTempMarkerPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	origLiveness, origReadiness := livenessMarkerPath, readinessMarkerPath
	livenessMarkerPath = dir + "/healthy"
	readinessMarkerPath = dir + "/ready"
	t.Cleanup(func() {
		livenessMarkerPath, readinessMarkerPath = origLiveness, origReadiness
	})
}

// The real-gRPC tests below run runJob on a background goroutine and wait
// on wall-clock deadlines. The budgets are sized for a contended CI runner,
// not a developer machine: a cold-cache GitHub Actions job compiles and
// runs package test binaries concurrently, and seconds of scheduling delay
// there say nothing about a regression. Every wait either succeeds in
// microseconds or has genuinely failed, so a generous budget costs nothing
// on a passing run. runJob prints phase timings on the terminal-failure
// path, which is what a recurrence should be diagnosed from.
const (
	probeWaitBudget    = 10 * time.Second
	eventWaitBudget    = 15 * time.Second
	terminalWaitBudget = 30 * time.Second
)

// waitForProbe polls CheckProbe until it matches want (present/absent) or
// the deadline elapses, absorbing the small delay between the client's
// WriteProbe/RemoveProbe call and this goroutine observing it — the two
// run concurrently with no happens-before edge between them.
func waitForProbe(t *testing.T, path string, wantPresent bool) {
	t.Helper()
	must := require.New(t)
	must.Eventually(func() bool {
		return (health.CheckProbe(path) == nil) == wantPresent
	}, probeWaitBudget, time.Millisecond, "marker %s did not reach present=%v", path, wantPresent)
}

// TestRunJob_ReadinessMarkerScopedToAssignmentWait drives runJob against a
// fake control plane and asserts that the readiness marker exists only
// while the harness is waiting for a task assignment, while the liveness
// marker spans the whole call.
func TestRunJob_ReadinessMarkerScopedToAssignmentWait(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)

	srv := &fakeControlPlane{
		readyRecv: make(chan struct{}),
		sendTask:  make(chan struct{}),
		// An invalid Mode fails types.ValidateRunConfig immediately inside
		// core.BuildLoopWithTransport, so runJob returns without any
		// provider/executor I/O.
		task: &pb.RunConfig{Mode: "bogus-mode"},
	}
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(eventWaitBudget):
		must.FailNow("fake control plane never received the ready event")
	}

	// The harness is now blocked waiting for task_assignment: both
	// markers should be present.
	waitForProbe(t, livenessMarkerPath, true)
	waitForProbe(t, readinessMarkerPath, true)

	// Only now let the fake control plane send the assignment, so it
	// can't race the marker assertions above.
	close(srv.sendTask)

	select {
	case err := <-errCh:
		must.ErrorContains(err, "building harness")
	case <-time.After(eventWaitBudget):
		must.FailNow("runJob did not return after the invalid task_assignment")
	}

	// runJob returned, so its deferred cleanup (mirroring process exit in
	// the real "stirrup job" binary) has run: both markers are gone.
	waitForProbe(t, readinessMarkerPath, false)
	waitForProbe(t, livenessMarkerPath, false)
}

// TestRunJob_LivenessOutlivesReadinessDuringExecution drives runJob through
// a valid task_assignment against a fake provider that blocks mid-request,
// and asserts that — while a turn is actually in flight — the readiness
// marker is already gone but the liveness marker is not: readiness reflects
// only the idle assignment wait, liveness spans execution too.
func TestRunJob_LivenessOutlivesReadinessDuringExecution(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	requestReceived := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+
			"\n\ndata: "+
			`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+
			"\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()

	timeout := int32(30)
	enforce := false
	srv := &fakeControlPlane{
		readyRecv: make(chan struct{}),
		task: &pb.RunConfig{
			RunId:            "job-test-mid-execution",
			Mode:             "execution",
			Prompt:           "Say hello.",
			Provider:         &pb.ProviderConfig{Type: "openai-compatible", ApiKeyRef: "secret://TEST_OPENAI_KEY", BaseUrl: provider.URL},
			ModelRouter:      &pb.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
			PromptBuilder:    &pb.PromptBuilderConfig{Type: "default"},
			ContextStrategy:  &pb.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
			Executor:         &pb.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
			EditStrategy:     &pb.EditStrategyConfig{Type: "whole-file"},
			Verifier:         &pb.VerifierConfig{Type: "none"},
			PermissionPolicy: &pb.PermissionPolicyConfig{Type: "allow-all"},
			GitStrategy:      &pb.GitStrategyConfig{Type: "none"},
			TraceEmitter:     &pb.TraceEmitterConfig{Type: "jsonl"},
			RuleOfTwo:        &pb.RuleOfTwoConfig{Enforce: &enforce},
			MaxTurns:         1,
			Timeout:          &timeout,
		},
	}
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(eventWaitBudget):
		must.FailNow("fake control plane never received the ready event")
	}

	select {
	case <-requestReceived:
	case <-time.After(eventWaitBudget):
		must.FailNow("provider never received a request; the loop did not reach execution")
	}

	// A turn is now actually in flight against the (blocked) provider:
	// the task was assigned, so readiness is gone, but liveness holds
	// through execution.
	waitForProbe(t, readinessMarkerPath, false)
	waitForProbe(t, livenessMarkerPath, true)

	close(release)

	select {
	case err := <-errCh:
		must.NoError(err, "single-turn run should succeed")
	case <-time.After(eventWaitBudget):
		must.FailNow("runJob did not return after the provider responded")
	}

	waitForProbe(t, readinessMarkerPath, false)
	waitForProbe(t, livenessMarkerPath, false)
}

// TestEmitTerminalFailure_EmitsErrorThenDone pins the terminal pair a
// control plane relies on when a post-assignment failure happens before
// the loop exists: "error" carrying the cause, then "done" with
// stop_reason "error".
func TestEmitTerminalFailure_EmitsErrorThenDone(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tp := &recordingTransport{}

	emitTerminalFailure(tp, errors.New("building harness: config validation: bad"), "error")

	must.Len(tp.events, 2, "want exactly error then done, got %v", tp.events)
	check.Equal("error", tp.events[0].Type)
	check.Contains(tp.events[0].Message, "config validation: bad", "error event should carry the cause")
	check.Equal("done", tp.events[1].Type)
	check.Equal("error", tp.events[1].StopReason)
}

// TestEmitTerminalFailure_EmitFailureStillAttemptsDone checks that a
// failed "error" emit does not abandon the "done" event: "done" is the
// terminal signal, so a control plane that missed the diagnostic still
// needs the run closed out.
func TestEmitTerminalFailure_EmitFailureStillAttemptsDone(t *testing.T) {
	must := require.New(t)
	tp := &recordingTransport{emitErr: errors.New("stream closed")}

	emitTerminalFailure(tp, errors.New("building harness: boom"), "error")

	must.Len(tp.events, 2, "want both events attempted despite the emit failure, got %v", tp.events)
	must.Equal("done", tp.events[1].Type)
}

// emitTerminalFailure is safe when no transport was established, so the
// caller never has to guard it.
func TestEmitTerminalFailure_NilTransportDoesNotPanic(t *testing.T) {
	emitTerminalFailure(nil, errors.New("boom"), "error")
}

// recordingControlPlane is the minimal correct control plane from
// docs/integration-guide.md: wait for "ready", assign a task, then
// consume events until the terminal "done". It records every
// HarnessEvent the harness sends back so a test can assert on the
// terminal contract.
type recordingControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	task *pb.RunConfig
	// traceContext, when set, rides on the task_assignment.
	traceContext *pb.TraceContext

	mu       sync.Mutex
	ready    *pb.HarnessEvent
	received []*pb.HarnessEvent
	doneCh   chan struct{}
	// With follow-ups enabled each run emits its own "done", so the
	// close must tolerate more than one.
	doneOnce sync.Once
}

func newRecordingControlPlane(task *pb.RunConfig) *recordingControlPlane {
	return &recordingControlPlane{task: task, doneCh: make(chan struct{})}
}

func (s *recordingControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	ready, err := stream.Recv()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
	if err := stream.Send(&pb.ControlEvent{Type: "task_assignment", Task: s.task, TraceContext: s.traceContext}); err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
		if ev.Type == "done" {
			s.doneOnce.Do(func() { close(s.doneCh) })
			return nil
		}
	}
}

func (s *recordingControlPlane) events() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.received...)
}

// findEvent returns the first recorded event of the given type.
func (s *recordingControlPlane) findEvent(eventType string) *pb.HarnessEvent {
	for _, ev := range s.events() {
		if ev.Type == eventType {
			return ev
		}
	}
	return nil
}

func TestRunJob_ReadyAdvertisesCapabilitiesAndConfigFields(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	srv := newRecordingControlPlane(&pb.RunConfig{Mode: "execution"})
	startControlPlane(t, srv)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("control plane never received a done event")
	}
	select {
	case runErr := <-errCh:
		must.Error(runErr, "runJob should fail for a rejected config")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return")
	}

	srv.mu.Lock()
	ready := srv.ready
	srv.mu.Unlock()
	must.Equal("ready", ready.GetType(), "first event sent to the control plane")
	want := []string{"live_transcript", "reasoning_summary", "follow_up_history", "turn_abandoned", "lazy_tool_loading"}
	must.Equal(want, ready.GetCapabilities())
	must.Equal(types.ConfigFieldPaths(), ready.GetConfigFields())
	must.Subset(ready.GetConfigFields(), []string{"provider.endpointProfile", "tools.lazyLoading.eager", "providers.*.type"})
	must.Empty(ready.GetHarnessSemver(), "a dev build advertises no semantic version")
}

// TestRunJob_InvalidAssignedConfigSignalsControlPlane drives `stirrup
// job` against a real gRPC control plane and pins that a RunConfig
// rejected on arrival still produces the error/done pair. Stream closure
// alone is indistinguishable from a crashed pod, so the control plane
// must be told the config was the problem.
func TestRunJob_InvalidAssignedConfigSignalsControlPlane(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	useTempMarkerPaths(t)

	// Missing provider, maxTurns and timeout: rejected by
	// types.ValidateRunConfig before any component is built.
	srv := newRecordingControlPlane(&pb.RunConfig{Mode: "execution"})
	startControlPlane(t, srv)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("control plane never received a done event for the rejected config")
	}

	select {
	case runErr := <-errCh:
		must.ErrorContains(runErr, "building harness")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after signalling the control plane")
	}

	var errMessages []string
	for _, ev := range srv.events() {
		if ev.Type == "error" {
			errMessages = append(errMessages, ev.Message)
		}
	}
	must.NotEmpty(errMessages, "no error event received; got %v", srv.events())
	for _, msg := range errMessages {
		check.Contains(msg, "config validation", "error event should carry the validation failure")
	}
}

// fatalPreRunHookJobConfig mirrors fatalPreRunHookConfig in proto form:
// a valid RunConfig whose single preRun hook exits non-zero without
// continueOnError. preRun hooks run before Git.Setup and before any
// turn, so the loop fails setup without ever calling the provider.
func fatalPreRunHookJobConfig(t *testing.T) *pb.RunConfig {
	t.Helper()
	t.Setenv("TEST_JOB_HOOKS_KEY", "unused-never-called")
	timeout := int32(30)
	return &pb.RunConfig{
		RunId:            "job-hooks-fatal-test",
		Mode:             "planning",
		Prompt:           "irrelevant, never reached",
		Provider:         &pb.ProviderConfig{Type: "anthropic", ApiKeyRef: "secret://TEST_JOB_HOOKS_KEY"},
		ModelRouter:      &pb.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    &pb.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  &pb.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         &pb.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     &pb.EditStrategyConfig{Type: "multi"},
		Verifier:         &pb.VerifierConfig{Type: "none"},
		PermissionPolicy: &pb.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      &pb.GitStrategyConfig{Type: "none"},
		TraceEmitter:     &pb.TraceEmitterConfig{Type: "jsonl"},
		Tools:            &pb.ToolsConfig{BuiltIn: []string{"read_file"}},
		MaxTurns:         2,
		Timeout:          &timeout,
		Hooks: &pb.HooksConfig{
			PreRun: []*pb.HookConfig{{Name: "boom", Command: "exit 1"}},
		},
	}
}

// TestRunJob_FatalPreRunHookSignalsControlPlaneAndSink drives a fatal
// setup failure through `stirrup job` against a real gRPC control plane
// and pins the terminal contract for a run that started: the control
// plane receives done{stop_reason:"setup_failed"}, and the RunResult
// still reaches the resultSink even though loop.Run returned an error.
// TestRunWithConfig_FatalPreRunHookFailure_StillEmitsRunResult covers
// the same contract on the harness.go entrypoint; this is the job.go
// path, whose events travel over the gRPC transport instead.
func TestRunJob_FatalPreRunHookSignalsControlPlaneAndSink(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	useTempMarkerPaths(t)

	sink := &stubResultSink{}
	installStubResultSink(t, sink)

	srv := newRecordingControlPlane(fatalPreRunHookJobConfig(t))
	startControlPlane(t, srv)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("control plane never received a done event", "got %v", srv.events())
	}

	select {
	case runErr := <-errCh:
		must.ErrorContains(runErr, "running harness")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after signalling the control plane")
	}

	done := srv.findEvent("done")
	must.NotNil(done, "no done event received; got %v", srv.events())
	check.Equal("setup_failed", done.StopReason)
	// "done" follows trace finalisation, so it carries the run summary
	// with the failure reason.
	must.NotNil(done.Trace, "done.trace should carry the run summary")
	check.Equal("setup_failed", done.Trace.Outcome)
	check.Equal("job-hooks-fatal-test", done.Trace.RunId)
	check.Regexp("^pre-run hooks: ", done.Trace.Error)

	// A run that started emits its RunResult even though loop.Run
	// returned a fatal error; a control plane reading only the sink
	// must still learn the outcome.
	must.Len(sink.calls, 1, "resultSink should receive exactly one result")
	result := sink.calls[0]
	check.Equal("setup_failed", result.Outcome)
	check.Equal("job-hooks-fatal-test", result.RunID, "RunResult should mirror RunConfig.runId")
	check.NotZero(result.HookFailures, "the failed preRun hook should be counted")
	check.Zero(result.Turns, "setup fails before the first turn")
}

func TestJobSessionBudget_FollowUpWindowAndSessionDeadline(t *testing.T) {
	run := 30 * time.Second

	t.Run("run alone without a follow-up window", func(t *testing.T) {
		must := require.New(t)
		t.Setenv("STIRRUP_SESSION_DEADLINE", "")
		must.Equal(run, jobSessionBudget(run, 0))
	})

	t.Run("follow-up allowance when a window is configured", func(t *testing.T) {
		must := require.New(t)
		t.Setenv("STIRRUP_SESSION_DEADLINE", "")
		want := run + cliSessionBudget(run, 60)
		must.Equal(want, jobSessionBudget(run, 60))
	})

	t.Run("session deadline overrides the estimate", func(t *testing.T) {
		must := require.New(t)
		t.Setenv("STIRRUP_SESSION_DEADLINE", "7200")
		must.Equal(2*time.Hour, jobSessionBudget(run, 60))
	})

	t.Run("invalid session deadline is ignored", func(t *testing.T) {
		must := require.New(t)
		t.Setenv("STIRRUP_SESSION_DEADLINE", "soon")
		must.Equal(run, jobSessionBudget(run, 0))
	})
}

// findErrorAndDone returns the last "error" and "done" events in events, or
// nil for whichever is absent.
func findErrorAndDone(events []*pb.HarnessEvent) (errEvent, doneEvent *pb.HarnessEvent) {
	for _, ev := range events {
		switch ev.Type {
		case "error":
			errEvent = ev
		case "done":
			doneEvent = ev
		}
	}
	return errEvent, doneEvent
}

// TestRunJob_RequiredMCPServerUnreachableSendsErrorThenDone drives the job
// path with a task whose only MCP server is required and unreachable, and
// asserts the control plane receives an "error" naming the server followed
// by "done" with stop_reason "error".
func TestRunJob_RequiredMCPServerUnreachableSendsErrorThenDone(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	timeout := int32(30)
	enforce := false
	srv := &fakeControlPlane{
		readyRecv:    make(chan struct{}),
		recordEvents: true,
		task: &pb.RunConfig{
			RunId:            "job-test-required-mcp",
			Mode:             "execution",
			Prompt:           "Say hello.",
			Provider:         &pb.ProviderConfig{Type: "openai-compatible", ApiKeyRef: "secret://TEST_OPENAI_KEY", BaseUrl: "http://127.0.0.1:1"},
			ModelRouter:      &pb.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
			PromptBuilder:    &pb.PromptBuilderConfig{Type: "default"},
			ContextStrategy:  &pb.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
			Executor:         &pb.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
			EditStrategy:     &pb.EditStrategyConfig{Type: "whole-file"},
			Verifier:         &pb.VerifierConfig{Type: "none"},
			PermissionPolicy: &pb.PermissionPolicyConfig{Type: "allow-all"},
			GitStrategy:      &pb.GitStrategyConfig{Type: "none"},
			TraceEmitter:     &pb.TraceEmitterConfig{Type: "jsonl"},
			RuleOfTwo:        &pb.RuleOfTwoConfig{Enforce: &enforce},
			Tools:            &pb.ToolsConfig{McpServers: []*pb.MCPServerConfig{{Name: "docs", Uri: deadURL}}},
			MaxTurns:         1,
			Timeout:          &timeout,
		},
	}
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case err := <-errCh:
		must.ErrorContains(err, `required MCP server "docs"`)
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after the unreachable required MCP server")
	}

	must.EventuallyWithT(func(c *assert.CollectT) {
		errEvent, doneEvent := findErrorAndDone(srv.receivedEvents())
		assert.True(c, errEvent != nil && doneEvent != nil,
			"control plane did not receive error and done; got %v", srv.receivedEvents())
	}, eventWaitBudget, 10*time.Millisecond)

	errEvent, doneEvent := findErrorAndDone(srv.receivedEvents())
	must.NotNil(errEvent)
	must.NotNil(doneEvent)
	check.Contains(errEvent.Message, `required MCP server "docs"`)
	check.Equal("error", doneEvent.StopReason)
}
