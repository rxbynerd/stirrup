package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
