package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)
	if err == nil {
		t.Fatal("runJob returned nil, want an error when CONTROL_PLANE_ADDR is unset")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CONTROL_PLANE_ADDR environment variable is required") {
		t.Errorf("error = %q, want the CONTROL_PLANE_ADDR-required message", msg)
	}
	if strings.Contains(msg, "\x1b[") {
		t.Errorf("job error must not contain ANSI escapes (log aggregators ingest it verbatim): %q", msg)
	}
}

// fakeControlPlane implements just enough of HarnessServiceServer to drive
// runJob through the assignment wait and into a task_assignment carrying
// the given RunConfig.
type fakeControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	readyRecv chan struct{}
	task      *pb.RunConfig

	// sendTask, if non-nil, gates the task_assignment send on the test
	// closing it. Without this, the assignment can reach runJob and clear
	// the readiness marker before the test observes it set, racing the
	// marker-scoped-to-assignment-wait assertion.
	sendTask chan struct{}
}

func (s *fakeControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	close(s.readyRecv)

	if s.sendTask != nil {
		<-s.sendTask
	}

	return stream.Send(&pb.ControlEvent{Type: "task_assignment", Task: s.task})
}

// startFakeControlPlane serves srv over a real TCP listener (runJob only
// takes CONTROL_PLANE_ADDR as a string, so bufconn's dial-option injection
// isn't reachable here) and points CONTROL_PLANE_ADDR at it.
func startFakeControlPlane(t *testing.T, srv *fakeControlPlane) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
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

// waitForProbe polls CheckProbe until it matches want (present/absent) or
// the deadline elapses, absorbing the small delay between the client's
// WriteProbe/RemoveProbe call and this goroutine observing it — the two
// run concurrently with no happens-before edge between them.
func waitForProbe(t *testing.T, path string, wantPresent bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := health.CheckProbe(path)
		if wantPresent && err == nil {
			return
		}
		if !wantPresent && err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %s present=%v after deadline, want present=%v", path, err == nil, wantPresent)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunJob_ReadinessMarkerScopedToAssignmentWait drives runJob against a
// fake control plane and asserts that the readiness marker exists only
// while the harness is waiting for a task assignment, while the liveness
// marker spans the whole call.
func TestRunJob_ReadinessMarkerScopedToAssignmentWait(t *testing.T) {
	useTempMarkerPaths(t)

	srv := &fakeControlPlane{
		readyRecv: make(chan struct{}),
		sendTask:  make(chan struct{}),
		// An invalid Mode fails types.ValidateRunConfig immediately inside
		// core.BuildLoopWithTransport, so runJob returns without any
		// provider/executor I/O.
		task: &pb.RunConfig{Mode: "bogus-mode"},
	}
	startFakeControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(5 * time.Second):
		t.Fatal("fake control plane never received the ready event")
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
		if err == nil || !strings.Contains(err.Error(), "building harness") {
			t.Fatalf("runJob() error = %v, want a building-harness failure from the invalid RunConfig", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJob did not return after the invalid task_assignment")
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
	startFakeControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(5 * time.Second):
		t.Fatal("fake control plane never received the ready event")
	}

	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never received a request; the loop did not reach execution")
	}

	// A turn is now actually in flight against the (blocked) provider:
	// the task was assigned, so readiness is gone, but liveness holds
	// through execution.
	waitForProbe(t, readinessMarkerPath, false)
	waitForProbe(t, livenessMarkerPath, true)

	close(release)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runJob() error = %v, want a successful single-turn run", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJob did not return after the provider responded")
	}

	waitForProbe(t, readinessMarkerPath, false)
	waitForProbe(t, livenessMarkerPath, false)
}

// TestEmitTerminalFailure_EmitsErrorThenDone pins the terminal pair a
// control plane relies on when a post-assignment failure happens before
// the loop exists: "error" carrying the cause, then "done" with
// stop_reason "error".
func TestEmitTerminalFailure_EmitsErrorThenDone(t *testing.T) {
	tp := &recordingTransport{}

	emitTerminalFailure(tp, errors.New("building harness: config validation: bad"))

	if len(tp.events) != 2 {
		t.Fatalf("emitted %d events (%v), want exactly error then done", len(tp.events), tp.events)
	}
	if tp.events[0].Type != "error" {
		t.Errorf("first event type = %q, want %q", tp.events[0].Type, "error")
	}
	if !strings.Contains(tp.events[0].Message, "config validation: bad") {
		t.Errorf("error event message = %q, want it to carry the cause", tp.events[0].Message)
	}
	if tp.events[1].Type != "done" {
		t.Errorf("second event type = %q, want %q", tp.events[1].Type, "done")
	}
	if tp.events[1].StopReason != "error" {
		t.Errorf("done stop_reason = %q, want %q", tp.events[1].StopReason, "error")
	}
}

// TestEmitTerminalFailure_EmitFailureStillAttemptsDone checks that a
// failed "error" emit does not abandon the "done" event: "done" is the
// terminal signal, so a control plane that missed the diagnostic still
// needs the run closed out.
func TestEmitTerminalFailure_EmitFailureStillAttemptsDone(t *testing.T) {
	tp := &recordingTransport{emitErr: errors.New("stream closed")}

	emitTerminalFailure(tp, errors.New("building harness: boom"))

	if len(tp.events) != 2 {
		t.Fatalf("emitted %d events (%v), want both attempted despite emit failure", len(tp.events), tp.events)
	}
	if tp.events[1].Type != "done" {
		t.Errorf("second event type = %q, want %q", tp.events[1].Type, "done")
	}
}

// TestEmitTerminalFailure_NilTransport confirms the helper is safe when
// no transport was established, so the caller never has to guard it.
func TestEmitTerminalFailure_NilTransport(t *testing.T) {
	emitTerminalFailure(nil, errors.New("boom"))
}

// rejectingControlPlane assigns a RunConfig that cannot pass validation
// and records every HarnessEvent the harness sends back, returning once
// it has seen the terminal "done".
type rejectingControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	mu       sync.Mutex
	received []*pb.HarnessEvent
	doneCh   chan struct{}
}

func (s *rejectingControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if err := stream.Send(&pb.ControlEvent{
		Type: "task_assignment",
		// Missing provider, maxTurns and timeout: rejected by
		// types.ValidateRunConfig before any component is built.
		Task: &pb.RunConfig{Mode: "execution"},
	}); err != nil {
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
			close(s.doneCh)
			return nil
		}
	}
}

func (s *rejectingControlPlane) events() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.received...)
}

// TestRunJob_InvalidAssignedConfigSignalsControlPlane drives `stirrup
// job` against a real gRPC control plane and pins that a RunConfig
// rejected on arrival still produces the error/done pair. Stream closure
// alone is indistinguishable from a crashed pod, so the control plane
// must be told the config was the problem.
func TestRunJob_InvalidAssignedConfigSignalsControlPlane(t *testing.T) {
	useTempMarkerPaths(t)

	srv := &rejectingControlPlane{doneCh: make(chan struct{})}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	t.Setenv("CONTROL_PLANE_ADDR", lis.Addr().String())
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.doneCh:
	case <-time.After(30 * time.Second):
		t.Fatal("control plane never received a done event for the rejected config")
	}

	select {
	case runErr := <-errCh:
		if runErr == nil {
			t.Error("runJob returned nil, want the build failure to fail the process")
		} else if !strings.Contains(runErr.Error(), "building harness") {
			t.Errorf("runJob error = %v, want it to report the build failure", runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runJob did not return after signalling the control plane")
	}

	var sawError bool
	for _, ev := range srv.events() {
		if ev.Type != "error" {
			continue
		}
		sawError = true
		if !strings.Contains(ev.Message, "config validation") {
			t.Errorf("error event message = %q, want the validation failure", ev.Message)
		}
	}
	if !sawError {
		t.Errorf("no error event received; got %v", srv.events())
	}
}
