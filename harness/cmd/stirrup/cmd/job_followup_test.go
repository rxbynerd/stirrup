package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// scriptedControlPlane assigns a task, then answers each "done" with
// the ControlEvent afterDone chooses for that run index (nil sends
// nothing). Every "done" stop_reason is published on dones so a test
// can follow the session run by run.
type scriptedControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	task      *pb.RunConfig
	afterDone func(n int) *pb.ControlEvent
	// onEvent, when set, may answer any HarnessEvent with a ControlEvent
	// (nil sends nothing) — the hook that sends input mid-run.
	onEvent func(ev *pb.HarnessEvent) *pb.ControlEvent

	dones chan string
	mu    sync.Mutex
	recv  []*pb.HarnessEvent
}

func newScriptedControlPlane(task *pb.RunConfig, afterDone func(n int) *pb.ControlEvent) *scriptedControlPlane {
	return &scriptedControlPlane{task: task, afterDone: afterDone, dones: make(chan string, 16)}
}

func (s *scriptedControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&pb.ControlEvent{Type: "task_assignment", Task: s.task}); err != nil {
		return err
	}
	n := 0
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.recv = append(s.recv, ev)
		s.mu.Unlock()
		if s.onEvent != nil {
			if ce := s.onEvent(ev); ce != nil {
				if err := stream.Send(ce); err != nil {
					return err
				}
			}
		}
		if ev.Type != "done" {
			continue
		}
		s.dones <- ev.StopReason
		if ce := s.afterDone(n); ce != nil {
			if err := stream.Send(ce); err != nil {
				return err
			}
		}
		n++
	}
}

func (s *scriptedControlPlane) events() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.recv...)
}

// serveControlPlane serves srv over a loopback TCP listener and points
// CONTROL_PLANE_ADDR at it for the duration of the test.
func serveControlPlane(t *testing.T, srv pb.HarnessServiceServer) {
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

// waitDone blocks for the next "done" and returns its stop_reason.
func (s *scriptedControlPlane) waitDone(t *testing.T, what string) string {
	t.Helper()
	select {
	case sr := <-s.dones:
		return sr
	case <-time.After(15 * time.Second):
		t.Fatalf("no done for %s", what)
		return ""
	}
}

// startOpenAIStub serves a minimal Chat Completions SSE stream for
// every request and counts them.
func startOpenAIStub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+
			"\n\ndata: "+
			`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+
			"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// followUpJobConfig builds a wire RunConfig for a single-turn
// openai-compatible run with a follow-up grace window.
func followUpJobConfig(t *testing.T, providerURL string, timeoutSecs, graceSecs int32) *pb.RunConfig {
	t.Helper()
	enforce := false
	return &pb.RunConfig{
		RunId:            "job-followup-primary",
		Mode:             "execution",
		Prompt:           "Say hello.",
		Provider:         &pb.ProviderConfig{Type: "openai-compatible", ApiKeyRef: "secret://TEST_OPENAI_KEY", BaseUrl: providerURL},
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
		Timeout:          &timeoutSecs,
		FollowUpGrace:    &graceSecs,
	}
}

// TestRunJob_FollowUpGetsFreshTimeout pins the budget contract end to
// end: a follow-up that arrives after the primary run's timeout would
// have expired still gets its own full budget and completes with
// stop_reason "success" rather than "timeout".
func TestRunJob_FollowUpGetsFreshTimeout(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider, requests := startOpenAIStub(t)

	const timeoutSecs = 2
	srv := newScriptedControlPlane(followUpJobConfig(t, provider.URL, timeoutSecs, 30), func(n int) *pb.ControlEvent {
		switch n {
		case 0:
			// Outlive the primary run's deadline before asking for more.
			time.Sleep((timeoutSecs + 1) * time.Second)
			return &pb.ControlEvent{Type: "user_response", UserResponse: "And again."}
		default:
			return &pb.ControlEvent{Type: "cancel"}
		}
	})
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	if sr := srv.waitDone(t, "primary run"); sr != "success" {
		t.Fatalf("primary done stop_reason = %q, want success", sr)
	}
	if sr := srv.waitDone(t, "follow-up run"); sr != "success" {
		t.Fatalf("follow-up done stop_reason = %q, want success (a fresh budget, not the primary's remainder)", sr)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runJob() error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runJob did not return after cancel")
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (one per run)", got)
	}
}

// TestRunJob_FollowUpsFinaliseEachRun pins per-run finalisation end to
// end: the primary run and two follow-ups each emit exactly one
// RunResult and one workspace export, with the follow-ups' tarballs at
// run-scoped object paths.
func TestRunJob_FollowUpsFinaliseEachRun(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider, requests := startOpenAIStub(t)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exporter := &recordingExporter{}
	installRecordingExporter(t, exporter)

	task := followUpJobConfig(t, provider.URL, 30, 30)
	task.Executor.WorkspaceExportTo = "gs://bucket/runs/primary/workspace.tar.gz"
	srv := newScriptedControlPlane(task, func(n int) *pb.ControlEvent {
		if n < 2 {
			return &pb.ControlEvent{Type: "user_response", UserResponse: fmt.Sprintf("follow-up %d", n+1)}
		}
		return &pb.ControlEvent{Type: "cancel"}
	})
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	for _, what := range []string{"primary", "follow-up 1", "follow-up 2"} {
		if sr := srv.waitDone(t, what); sr != "success" {
			t.Fatalf("%s done stop_reason = %q, want success", what, sr)
		}
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runJob() error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runJob did not return after cancel")
	}

	if got := requests.Load(); got != 3 {
		t.Errorf("provider requests = %d, want 3 (one per run)", got)
	}
	if len(sink.calls) != 3 {
		t.Fatalf("result sink emitted %d results, want exactly one per run (3)", len(sink.calls))
	}
	ids := []string{sink.calls[0].RunID, sink.calls[1].RunID, sink.calls[2].RunID}
	if ids[0] != "job-followup-primary" {
		t.Errorf("first result RunID = %q, want the primary run's", ids[0])
	}
	if ids[1] == ids[0] || ids[2] == ids[0] || ids[1] == ids[2] {
		t.Errorf("run IDs are not distinct: %v", ids)
	}
	for i, r := range sink.calls {
		if r.Outcome != "success" {
			t.Errorf("result %d outcome = %q, want success", i, r.Outcome)
		}
	}
	want := []string{
		"gs://bucket/runs/primary/workspace.tar.gz",
		"gs://bucket/runs/primary/" + ids[1] + "/workspace.tar.gz",
		"gs://bucket/runs/primary/" + ids[2] + "/workspace.tar.gz",
	}
	if got := exporter.destinations(); !equalStrings(got, want) {
		t.Errorf("export destinations = %v, want %v", got, want)
	}
}

// TestRunJob_MidRunUserResponseIsInjected drives a user_response into an
// active run over the real gRPC transport: it must reach the model as
// the next user turn rather than being dropped.
func TestRunJob_MidRunUserResponseIsInjected(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	inputSent := make(chan struct{})
	var mu sync.Mutex
	var bodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if n == 1 {
			// Hold the first turn open until the control plane has sent
			// its input, so the event is unambiguously mid-run.
			select {
			case <-inputSent:
			case <-time.After(10 * time.Second):
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "data: "+`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)

	task := followUpJobConfig(t, provider.URL, 30, 0)
	task.MaxTurns = 2
	var once sync.Once
	srv := newScriptedControlPlane(task, func(int) *pb.ControlEvent { return nil })
	srv.onEvent = func(ev *pb.HarnessEvent) *pb.ControlEvent {
		if ev.Type != "text_delta" {
			return nil
		}
		var ce *pb.ControlEvent
		once.Do(func() {
			ce = &pb.ControlEvent{Type: "user_response", UserResponse: "And again.", RequestId: "mid-run-1"}
			close(inputSent)
		})
		return ce
	}
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	if sr := srv.waitDone(t, "the run"); sr != "success" {
		t.Fatalf("done stop_reason = %q, want success", sr)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runJob() error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runJob did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("provider requests = %d, want 2 (the queued input continued the run past end_turn)", len(bodies))
	}
	second := bodies[1]
	assistant := strings.Index(second, `"content":"hi"`)
	injected := strings.Index(second, "And again.")
	if assistant < 0 || injected < 0 || injected < assistant {
		t.Errorf("second request did not carry the assistant turn followed by the injected input:\n%s", second)
	}
	for _, ev := range srv.events() {
		if ev.Type == "warning" && ev.RequestId == "mid-run-1" {
			t.Errorf("the accepted user_response was reported dropped: %s", ev.Message)
		}
	}
}
