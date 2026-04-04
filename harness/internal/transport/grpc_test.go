package transport

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

const bufSize = 1024 * 1024

// testServer implements the HarnessService server side for testing.
// It records received events and can be configured to send control events.
type testServer struct {
	pb.UnimplementedHarnessServiceServer

	mu       sync.Mutex
	received []*pb.HarnessEvent
	toSend   []*pb.ControlEvent
	streamCh chan pb.HarnessService_RunTaskServer // exposes the stream for manual control
}

func newTestServer(toSend ...*pb.ControlEvent) *testServer {
	return &testServer{
		toSend:   toSend,
		streamCh: make(chan pb.HarnessService_RunTaskServer, 1),
	}
}

func (s *testServer) RunTask(stream pb.HarnessService_RunTaskServer) error {
	// Expose stream for tests that need manual control.
	select {
	case s.streamCh <- stream:
	default:
	}

	// Send any pre-configured control events.
	for _, ce := range s.toSend {
		if err := stream.Send(ce); err != nil {
			return err
		}
	}

	// Read all incoming harness events until the client closes.
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil // client closed or error — either way, done
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
	}
}

func (s *testServer) getReceived() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*pb.HarnessEvent, len(s.received))
	copy(cp, s.received)
	return cp
}

// setupTestTransport creates an in-process gRPC server+client pair using
// bufconn. Returns the transport, the test server, and a cleanup function.
func setupTestTransport(t *testing.T, srv *testServer) (*GRPCTransport, *testServer, func()) {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	ctx := context.Background()
	tr, err := NewGRPCTransport(ctx, "passthrough:///bufconn",
		WithDialOptions(
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

	cleanup := func() {
		_ = tr.Close()
		grpcServer.Stop()
	}

	return tr, srv, cleanup
}

func TestGRPCTransport_Emit(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}

	if err := tr.Emit(event); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Close send side so server's Recv loop finishes.
	_ = tr.stream.CloseSend()

	// Give the server goroutine a moment to process.
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	if len(received) != 1 {
		t.Fatalf("expected 1 received event, got %d", len(received))
	}
	if received[0].Type != "text_delta" {
		t.Errorf("got type %q, want text_delta", received[0].Type)
	}
	if received[0].Text != "hello world" {
		t.Errorf("got text %q, want hello world", received[0].Text)
	}
}

func TestGRPCTransport_EmitWithInput(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	input := json.RawMessage(`{"file":"test.go"}`)
	event := types.HarnessEvent{
		Type:  "tool_call",
		ID:    "tc_1",
		Name:  "read_file",
		Input: input,
	}

	if err := tr.Emit(event); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	if len(received) != 1 {
		t.Fatalf("expected 1 received event, got %d", len(received))
	}
	if received[0].Name != "read_file" {
		t.Errorf("got name %q, want read_file", received[0].Name)
	}
	if string(received[0].Input) != `{"file":"test.go"}` {
		t.Errorf("got input %q, want {\"file\":\"test.go\"}", string(received[0].Input))
	}
}

func TestGRPCTransport_OnControl(t *testing.T) {
	srv := newTestServer(&pb.ControlEvent{
		Type: "cancel",
	})
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		if ev.Type != "cancel" {
			t.Errorf("got type %q, want cancel", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control event")
	}
}

func TestGRPCTransport_OnControlTaskAssignment(t *testing.T) {
	timeout := int32(300)
	srv := newTestServer(&pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{
			RunId:    "run-123",
			Mode:     "execution",
			Prompt:   "fix the bug",
			MaxTurns: 10,
			Timeout:  &timeout,
			Provider: &pb.ProviderConfig{
				Type:      "anthropic",
				ApiKeyRef: "secret://ANTHROPIC_API_KEY",
			},
		},
	})
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		if ev.Type != "task_assignment" {
			t.Fatalf("got type %q, want task_assignment", ev.Type)
		}
		if ev.Task == nil {
			t.Fatal("expected Task to be non-nil")
		}
		if ev.Task.RunID != "run-123" {
			t.Errorf("got RunID %q, want run-123", ev.Task.RunID)
		}
		if ev.Task.Prompt != "fix the bug" {
			t.Errorf("got Prompt %q, want 'fix the bug'", ev.Task.Prompt)
		}
		if ev.Task.MaxTurns != 10 {
			t.Errorf("got MaxTurns %d, want 10", ev.Task.MaxTurns)
		}
		if ev.Task.Provider.Type != "anthropic" {
			t.Errorf("got Provider.Type %q, want anthropic", ev.Task.Provider.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task assignment")
	}
}

func TestGRPCTransport_OnControlPermissionResponse(t *testing.T) {
	allowed := true
	srv := newTestServer(&pb.ControlEvent{
		Type:      "permission_response",
		RequestId: "req-42",
		Allowed:   &pb.OptionalBool{Value: allowed},
		Reason:    "user approved",
	})
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		if ev.Type != "permission_response" {
			t.Fatalf("got type %q, want permission_response", ev.Type)
		}
		if ev.RequestID != "req-42" {
			t.Errorf("got RequestID %q, want req-42", ev.RequestID)
		}
		if ev.Allowed == nil || !*ev.Allowed {
			t.Error("expected Allowed to be true")
		}
		if ev.Reason != "user approved" {
			t.Errorf("got Reason %q, want 'user approved'", ev.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for permission response")
	}
}

func TestGRPCTransport_BidirectionalFlow(t *testing.T) {
	// Server sends a task assignment, then we emit events and receive more
	// control events interleaved.
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	// Get the server stream for manual control.
	var serverStream pb.HarnessService_RunTaskServer
	select {
	case serverStream = <-srv.streamCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server stream")
	}

	// Start receiving control events.
	controlEvents := make(chan types.ControlEvent, 10)
	tr.OnControl(func(event types.ControlEvent) {
		controlEvents <- event
	})

	// Server sends task_assignment.
	timeout := int32(300)
	if err := serverStream.Send(&pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{
			RunId:    "run-bidi",
			Mode:     "execution",
			Prompt:   "test bidi",
			MaxTurns: 5,
			Timeout:  &timeout,
		},
	}); err != nil {
		t.Fatalf("server send task_assignment: %v", err)
	}

	// Client receives it.
	select {
	case ev := <-controlEvents:
		if ev.Type != "task_assignment" {
			t.Fatalf("expected task_assignment, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task_assignment")
	}

	// Client emits a text_delta.
	if err := tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "working..."}); err != nil {
		t.Fatalf("Emit text_delta: %v", err)
	}

	// Client emits a tool_call.
	if err := tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc_1", Name: "bash"}); err != nil {
		t.Fatalf("Emit tool_call: %v", err)
	}

	// Server sends a user_response.
	if err := serverStream.Send(&pb.ControlEvent{
		Type:         "user_response",
		UserResponse: "yes, continue",
	}); err != nil {
		t.Fatalf("server send user_response: %v", err)
	}

	// Client receives user_response.
	select {
	case ev := <-controlEvents:
		if ev.Type != "user_response" {
			t.Fatalf("expected user_response, got %q", ev.Type)
		}
		if ev.UserResponse != "yes, continue" {
			t.Errorf("got UserResponse %q", ev.UserResponse)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for user_response")
	}

	// Verify server received the client events.
	time.Sleep(50 * time.Millisecond)
	received := srv.getReceived()
	if len(received) < 2 {
		t.Fatalf("expected at least 2 received events, got %d", len(received))
	}
	if received[0].Type != "text_delta" {
		t.Errorf("first event type %q, want text_delta", received[0].Type)
	}
	if received[1].Type != "tool_call" {
		t.Errorf("second event type %q, want tool_call", received[1].Type)
	}
}

func TestGRPCTransport_Close(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	_ = cleanup // we'll close manually

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestGRPCTransport_EmitScrubsSecrets(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:    "tool_result",
		Content: "key is sk-ant-abc123-secret",
		Message: "token ghp_abcdef1234567890",
	}

	if err := tr.Emit(event); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	if len(received) != 1 {
		t.Fatalf("expected 1 received event, got %d", len(received))
	}

	if strings.Contains(received[0].Content, "sk-ant-") {
		t.Error("Anthropic API key was not scrubbed")
	}
	if strings.Contains(received[0].Message, "ghp_") {
		t.Error("GitHub PAT was not scrubbed")
	}
	if !strings.Contains(received[0].Content, "[REDACTED]") {
		t.Error("expected [REDACTED] in Content")
	}
	if !strings.Contains(received[0].Message, "[REDACTED]") {
		t.Error("expected [REDACTED] in Message")
	}
}

func TestGRPCTransport_StreamErrorStopsReadLoop(t *testing.T) {
	srv := newTestServer()
	tr, _, _ := setupTestTransport(t, srv)

	tr.OnControl(func(event types.ControlEvent) {
		// Should not be called after stream error.
	})

	// Force-close the connection to trigger stream error.
	_ = tr.conn.Close()

	// The done channel should close within a reasonable time.
	select {
	case <-tr.Done():
		// Expected — read loop exited.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for read loop to exit after stream error")
	}
}
