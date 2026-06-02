package core

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
)

// stubControlPlane is a minimal HarnessService server for session
// integration tests. When respond is set it echoes every tool_result_request
// back as a tool_result_response carrying replyContent; it records the
// request ids of any session_terminate events it receives.
type stubControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	respond      bool
	replyContent string

	mu          sync.Mutex
	requestIDs  []string
	terminateCh chan string
}

func (s *stubControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		switch ev.Type {
		case "tool_result_request":
			s.mu.Lock()
			s.requestIDs = append(s.requestIDs, ev.RequestId)
			s.mu.Unlock()
			if s.respond {
				_ = stream.Send(&pb.ControlEvent{
					Type:      "tool_result_response",
					RequestId: ev.RequestId,
					Content:   s.replyContent,
				})
			}
		case "session_terminate":
			select {
			case s.terminateCh <- ev.RequestId:
			default:
			}
		}
	}
}

func newSessionIntegrationTransport(t *testing.T, srv *stubControlPlane) *transport.GRPCTransport {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()

	tr, err := transport.NewGRPCTransport(context.Background(), "passthrough:///bufconn",
		transport.WithDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		),
	)
	if err != nil {
		grpcServer.Stop()
		t.Fatalf("NewGRPCTransport: %v", err)
	}
	t.Cleanup(func() {
		_ = tr.Close()
		grpcServer.Stop()
	})
	return tr
}

// TestSessionIntegration_BlockingSpawn exercises the #54 transport spawner
// end-to-end over a real gRPC transport: SpawnAndWait dispatches a
// spawn-shaped tool_result_request and blocks on the control plane's
// serialised SubAgentResult.
func TestSessionIntegration_BlockingSpawn(t *testing.T) {
	body, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "from control plane", Turns: 2})
	srv := &stubControlPlane{respond: true, replyContent: string(body), terminateCh: make(chan string, 1)}
	tr := newSessionIntegrationTransport(t, srv)

	mgr, err := NewSessionManager(SessionManagerOptions{
		UseTransport: true,
		Transport:    tr,
		MaxConcurrent: 4,
		DefaultWait:   5 * time.Second,
		MaxLifetime:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	res, err := mgr.SpawnAndWait(context.Background(), SubAgentConfig{Prompt: "audit the redaction path", Mode: "research"}, 5*time.Second)
	if err != nil {
		t.Fatalf("SpawnAndWait: %v", err)
	}
	if res.Outcome != "completed" || res.Output != "from control plane" || res.Turns != 2 {
		t.Fatalf("result = %#v, want {completed, from control plane, 2}", res)
	}
}

// TestSessionIntegration_DetachedLifecycle exercises the #71 start-and-detach
// flow end-to-end: start returns immediately, the control plane later
// responds, and wait collects the result.
func TestSessionIntegration_DetachedLifecycle(t *testing.T) {
	body, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "findings packet", Turns: 6})
	srv := &stubControlPlane{respond: true, replyContent: string(body), terminateCh: make(chan string, 1)}
	tr := newSessionIntegrationTransport(t, srv)

	mgr, err := NewSessionManager(SessionManagerOptions{
		UseTransport: true,
		Transport:    tr,
		MaxConcurrent: 4,
		DefaultWait:   5 * time.Second,
		MaxLifetime:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	id, err := mgr.Start(SubAgentConfig{Prompt: "investigate flaky test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait collects the result the control plane returned. (The stub
	// responds immediately, so by the time Wait returns the session is
	// terminal — the precise running->done transition is covered by the
	// fake-transport unit tests, which can gate the response.)
	st := mgr.Wait(context.Background(), id, 5*time.Second)
	if st.State != SessionDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if st.Result == nil || st.Result.Output != "findings packet" {
		t.Fatalf("result = %#v, want findings packet", st.Result)
	}

	// A subsequent check still observes the cached terminal result.
	if again := mgr.Status(id); again.State != SessionDone {
		t.Fatalf("re-check state = %q, want done", again.State)
	}
}

// TestSessionIntegration_Terminate confirms that terminating a detached
// session emits a session_terminate event the control plane receives.
func TestSessionIntegration_Terminate(t *testing.T) {
	// respond=false: the session stays running so we can terminate it.
	srv := &stubControlPlane{respond: false, terminateCh: make(chan string, 1)}
	tr := newSessionIntegrationTransport(t, srv)

	mgr, err := NewSessionManager(SessionManagerOptions{
		UseTransport: true,
		Transport:    tr,
		MaxConcurrent: 4,
		DefaultWait:   2 * time.Second,
		MaxLifetime:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	id, err := mgr.Start(SubAgentConfig{Prompt: "long running"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mgr.Terminate(id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if st := mgr.Status(id); st.State != SessionTerminated {
		t.Fatalf("state = %q, want terminated", st.State)
	}

	select {
	case got := <-srv.terminateCh:
		if got != id {
			t.Fatalf("session_terminate requestID = %q, want %q", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control plane did not receive session_terminate")
	}
}
