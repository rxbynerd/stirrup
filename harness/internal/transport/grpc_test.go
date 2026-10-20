package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/security"
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
	must := require.New(t)

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
	}
	must.NoError(err, "NewGRPCTransport")

	cleanup := func() {
		_ = tr.Close()
		grpcServer.Stop()
	}

	return tr, srv, cleanup
}

func TestGRPCTransport_Emit_DeliversEventToServer(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}

	must.NoError(tr.Emit(event), "Emit")

	// Close send side so server's Recv loop finishes.
	_ = tr.stream.CloseSend()

	// Give the server goroutine a moment to process.
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	check.Equal("text_delta", received[0].Type)
	check.Equal("hello world", received[0].Text)
}

func TestGRPCTransport_Emit_PreservesToolCallInput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NoError(tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	check.Equal("read_file", received[0].Name)
	check.JSONEq(`{"file":"test.go"}`, string(received[0].Input))
}

func TestGRPCTransport_OnControl_DeliversControlEvent(t *testing.T) {
	must := require.New(t)
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
		must.Equal("cancel", ev.Type)
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for control event")
	}
}

func TestGRPCTransport_OnControl_DeliversTaskAssignment(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
		must.Equal("task_assignment", ev.Type)
		must.NotNil(ev.Task, "Task")
		check.Equal("run-123", ev.Task.RunID)
		check.Equal("fix the bug", ev.Task.Prompt)
		check.Equal(10, ev.Task.MaxTurns)
		check.Equal("anthropic", ev.Task.Provider.Type)
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for task assignment")
	}
}

func TestGRPCTransport_OnControl_DeliversPermissionResponse(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
		must.Equal("permission_response", ev.Type)
		check.Equal("req-42", ev.RequestID)
		must.NotNil(ev.Allowed, "Allowed")
		check.True(*ev.Allowed, "Allowed")
		check.Equal("user approved", ev.Reason)
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for permission response")
	}
}

func TestGRPCTransport_BidirectionalFlow(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var serverStream pb.HarnessService_RunTaskServer
	select {
	case serverStream = <-srv.streamCh:
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for server stream")
	}

	controlEvents := make(chan types.ControlEvent, 10)
	tr.OnControl(func(event types.ControlEvent) {
		controlEvents <- event
	})

	timeout := int32(300)
	must.NoError(serverStream.Send(&pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{
			RunId:    "run-bidi",
			Mode:     "execution",
			Prompt:   "test bidi",
			MaxTurns: 5,
			Timeout:  &timeout,
		},
	}), "server send task_assignment")

	select {
	case ev := <-controlEvents:
		must.Equal("task_assignment", ev.Type)
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for task_assignment")
	}

	must.NoError(tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "working..."}), "Emit text_delta")
	must.NoError(tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc_1", Name: "bash"}), "Emit tool_call")

	must.NoError(serverStream.Send(&pb.ControlEvent{
		Type:         "user_response",
		UserResponse: "yes, continue",
	}), "server send user_response")

	select {
	case ev := <-controlEvents:
		must.Equal("user_response", ev.Type)
		check.Equal("yes, continue", ev.UserResponse)
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for user_response")
	}

	time.Sleep(50 * time.Millisecond)
	received := srv.getReceived()
	must.GreaterOrEqual(len(received), 2, "received events")
	check.Equal("text_delta", received[0].Type)
	check.Equal("tool_call", received[1].Type)
}

func TestGRPCTransport_Close_ReturnsNoError(t *testing.T) {
	must := require.New(t)
	srv := newTestServer()
	tr, _, _ := setupTestTransport(t, srv)

	must.NoError(tr.Close(), "Close")
}

// TestGRPCTransport_CloseDeliversPendingEvents pins that events emitted
// immediately before Close reach the peer. Send only queues a frame for
// the writer goroutine, so the connection must not be torn down until
// the peer has acknowledged the terminal "error"/"done" pair a control
// plane needs to tell a rejected config from a crashed harness.
// Repeated because delivery depends on writer scheduling.
func TestGRPCTransport_CloseDeliversPendingEvents(t *testing.T) {
	must := require.New(t)
	const attempts = 50
	useShortStreamEndGrace(t)

	for attempt := range attempts {
		func() {
			srv := newTestServer()
			tr, _, cleanup := setupTestTransport(t, srv)
			defer cleanup()

			must.NoError(tr.Emit(types.HarnessEvent{Type: "error", Message: "config validation: bad"}), "attempt %d: Emit error event", attempt)
			must.NoError(tr.Emit(types.HarnessEvent{Type: "done", StopReason: "error"}), "attempt %d: Emit done event", attempt)

			start := time.Now()
			must.NoError(tr.Close(), "attempt %d: Close", attempt)
			// Close must return on stream end, not by waiting out the grace,
			// so a drain regression fails here instead of timing out the package.
			must.Less(time.Since(start), streamEndGrace, "attempt %d: Close should return on stream end", attempt)

			received := srv.getReceived()
			must.Len(received, 2, "attempt %d: server received events %v, want the error/done pair", attempt, received)
			must.Equal("error", received[0].Type, "attempt %d", attempt)
			must.Equal("done", received[1].Type, "attempt %d", attempt)
		}()
	}
}

// useShortStreamEndGrace shrinks the close grace for the duration of the
// test so a drain regression fails an assertion instead of running the
// package past its timeout.
func useShortStreamEndGrace(t *testing.T) {
	t.Helper()
	prev := streamEndGrace
	streamEndGrace = 2 * time.Second
	t.Cleanup(func() { streamEndGrace = prev })
}

// slowFinishServer keeps the RPC open for a beat after the client
// half-closes, widening the window in which Close is waiting for stream
// end so a concurrent Emit can be landed inside it deterministically.
type slowFinishServer struct {
	pb.UnimplementedHarnessServiceServer

	mu         sync.Mutex
	received   []*pb.HarnessEvent
	halfClosed chan struct{}
	linger     time.Duration
}

func (s *slowFinishServer) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			close(s.halfClosed)
			time.Sleep(s.linger)
			return nil
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
	}
}

func (s *slowFinishServer) getReceived() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.received...)
}

// TestGRPCTransport_EmitDuringCloseDoesNotAbortDrain pins that an emitter
// still running at shutdown — the loop's heartbeat goroutine is stopped
// by an asynchronous context cancellation, so it can reach Emit at any
// point during Close — cannot cost the run its terminal events. grpc-go
// aborts the whole RPC when SendMsg follows CloseSend, which would end
// Close's wait early and discard anything still queued.
func TestGRPCTransport_EmitDuringCloseDoesNotAbortDrain(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	useShortStreamEndGrace(t)

	const linger = 300 * time.Millisecond
	srv := &slowFinishServer{halfClosed: make(chan struct{}), linger: linger}

	lis := bufconn.Listen(bufSize)
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	tr, err := NewGRPCTransport(context.Background(), "passthrough:///bufconn",
		WithDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		),
	)
	must.NoError(err, "NewGRPCTransport")

	must.NoError(tr.Emit(types.HarnessEvent{Type: "error", Message: "boom"}), "Emit error event")
	must.NoError(tr.Emit(types.HarnessEvent{Type: "done", StopReason: "error"}), "Emit done event")

	// Fires once the server has seen the half-close, i.e. while Close is
	// inside its wait for stream end.
	emitErrCh := make(chan error, 1)
	go func() {
		<-srv.halfClosed
		emitErrCh <- tr.Emit(types.HarnessEvent{Type: "heartbeat"})
	}()

	start := time.Now()
	must.NoError(tr.Close(), "Close")
	elapsed := time.Since(start)

	must.ErrorIs(<-emitErrCh, ErrTransportClosed, "Emit during Close must return ErrTransportClosed so it never reaches SendMsg")

	// Short-circuiting the drain is the failure this guards: the abort
	// would return Close in microseconds instead of waiting out linger.
	check.GreaterOrEqual(elapsed, linger, "Close should wait for stream end")

	received := srv.getReceived()
	must.Len(received, 2, "server received events %v, want the error/done pair", received)
	check.Equal("done", received[1].Type, "last event")
}

func TestGRPCTransport_EmitScrubsSecrets(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:    "tool_result",
		Content: "key is sk-ant-abc123-secret",
		Message: "token ghp_abcdef1234567890",
	}

	must.NoError(tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)

	check.NotContains(received[0].Content, "sk-ant-", "Anthropic API key was not scrubbed")
	check.NotContains(received[0].Message, "ghp_", "GitHub PAT was not scrubbed")
	check.Contains(received[0].Content, "[REDACTED]", "Content should carry the redaction marker")
	check.Contains(received[0].Message, "[REDACTED]", "Message should carry the redaction marker")
}

func TestGRPCTransport_EmitScrubsThinkingDeltaAndUserMessageText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	events := []types.HarnessEvent{
		{Type: "thinking_delta", Text: "the key is sk-ant-abc123-secret and ghp_abcdef1234567890"},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: "r1", Text: "use sk-ant-abc123-secret and ghp_abcdef1234567890"},
	}
	for _, ev := range events {
		must.NoError(tr.Emit(ev), "Emit %s", ev.Type)
	}

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, len(events))
	for _, ev := range received {
		check.NotContains(ev.Text, "sk-ant-", "%s text was not scrubbed", ev.Type)
		check.NotContains(ev.Text, "ghp_", "%s text was not scrubbed", ev.Type)
		check.Contains(ev.Text, "[REDACTED]", "%s text was not scrubbed", ev.Type)
	}
	check.Equal(types.UserMessageSourceUserResponse, received[1].Source, "user_message source should be unchanged")
	check.Equal("r1", received[1].RequestId, "user_message request_id should be unchanged")
}

func TestGRPCTransport_EmitCapsScrubbedUserMessageText(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	text := "sk-ant-abc123-secret " + strings.Repeat("u", maxEventFieldBytes)
	must.NoError(tr.Emit(types.HarnessEvent{Type: "user_message", Source: types.UserMessageSourceInitialPrompt, Text: text}), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	got := received[0].Text
	check.LessOrEqual(len(got), maxEventFieldBytes, "user_message text should be capped")
	check.Contains(got, "bytes shown]", "capped user_message text should carry the truncation marker")
	check.True(strings.HasPrefix(got, "[REDACTED]"), "capped text should start with the scrubbed secret, got %.40q", got)
}

// TestGRPCTransport_EmitScrubsInput mirrors the stdio Input-scrub fix on
// the gRPC transport: a permission_request forwarding a tool call's raw
// input must not leak a latch-tripping secret to the control plane.
func TestGRPCTransport_EmitScrubsInput(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:  "permission_request",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	must.NoError(tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	check.NotContains(string(received[0].Input), "AKIAQWERTYUIOPASDFGH", "live-shaped AWS key should be scrubbed from event Input")
	check.True(json.Valid(received[0].Input), "scrubbed Input should be valid JSON: %q", received[0].Input)
}

// TestGRPCTransport_EmitScrubsToolCallInput is the regression test for
// issue #593: now that the core loop emits "tool_call" on the transport,
// its Input must go through the same scrub path as permission_request and
// tool_result_request rather than a new, unscrubbed seam.
func TestGRPCTransport_EmitScrubsToolCallInput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:  "tool_call",
		ID:    "tc_1",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	must.NoError(tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	check.NotContains(string(received[0].Input), "AKIAQWERTYUIOPASDFGH", "live-shaped AWS key should be scrubbed from tool_call Input")
	check.True(json.Valid(received[0].Input), "scrubbed Input should be valid JSON: %q", received[0].Input)
	check.Equal("tc_1", received[0].Id, "scrubbing must not disturb other fields")
}

func TestGRPCTransport_Emit_LogsSecretRedactedInOutput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	must.NoError(tr.Emit(types.HarnessEvent{
		Type:    "tool_result",
		Content: "key=sk-ant-abc123-secret",
	}), "Emit")
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	got := secBuf.String()
	check.Contains(got, `"event":"secret_redacted_in_output"`)
	check.Contains(got, `"location":"transport.grpc.event.content"`)
	check.Contains(got, `"pattern":"anthropic_api_key"`)
}

func TestGRPCTransport_Emit_LogsNoSecurityEventForCleanText(t *testing.T) {
	must := require.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	must.NoError(tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "hello"}), "Emit")
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)
	must.Empty(secBuf.String(), "expected no security events")
}

// TestGRPCTransport_OnControl_TaskAssignmentKeepsAzureProviderFields pins that
// a task_assignment carrying APIKeyHeader and QueryParams round-trips through
// the proto wire format without losing data.
func TestGRPCTransport_OnControl_TaskAssignmentKeepsAzureProviderFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	timeout := int32(300)
	srv := newTestServer(&pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{
			RunId:    "run-azure",
			Mode:     "execution",
			Prompt:   "test",
			MaxTurns: 10,
			Timeout:  &timeout,
			Provider: &pb.ProviderConfig{
				Type:         "openai-responses",
				ApiKeyRef:    "secret://AZURE_OPENAI_KEY",
				BaseUrl:      "https://example.openai.azure.com/openai/v1",
				ApiKeyHeader: "api-key",
				QueryParams: map[string]string{
					"api-version": "preview",
				},
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
		must.NotNil(ev.Task, "Task")
		check.Equal("api-key", ev.Task.Provider.APIKeyHeader, "Provider.APIKeyHeader")
		check.Equal("preview", ev.Task.Provider.QueryParams["api-version"], "Provider.QueryParams[api-version]")
		check.Equal("https://example.openai.azure.com/openai/v1", ev.Task.Provider.BaseURL, "Provider.BaseURL")
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for task assignment")
	}
}

func TestGRPCTransport_StreamError_StopsReadLoop(t *testing.T) {
	must := require.New(t)
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
		must.FailNow("timed out waiting for read loop to exit after stream error")
	}
}

func TestGRPCTransport_OnControl_ToolResultResponse_Success(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	// IsError must be omitted (unset) when the proto wire field is nil,
	// and Content must round-trip verbatim.
	srv := newTestServer(&pb.ControlEvent{
		Type:      "tool_result_response",
		RequestId: "async-tool-7",
		Content:   "result body for the model",
	})
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		must.Equal("tool_result_response", ev.Type)
		check.Equal("async-tool-7", ev.RequestID)
		check.Equal("result body for the model", ev.Content)
		check.Nil(ev.IsError, "IsError should be nil for a success response")
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for tool_result_response")
	}
}

func TestGRPCTransport_OnControl_ToolResultResponse_IsError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	// IsError is an OptionalBool wrapper on the wire; controlEventFromProto
	// must dereference it into a *bool rather than dropping true to nil.
	yes := true
	srv := newTestServer(&pb.ControlEvent{
		Type:      "tool_result_response",
		RequestId: "async-tool-9",
		Content:   "upstream said no",
		IsError:   &pb.OptionalBool{Value: yes},
	})
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		must.Equal("tool_result_response", ev.Type)
		check.Equal("async-tool-9", ev.RequestID)
		check.Equal("upstream said no", ev.Content)
		must.NotNil(ev.IsError, "IsError should be set for an error response")
		check.True(*ev.IsError, "IsError should be true for an error response")
	case <-time.After(2 * time.Second):
		must.FailNow("timed out waiting for tool_result_response")
	}
}

func TestGRPCTransport_Emit_ToolResultRequestWireFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Pins the four wire fields the control plane needs to route an
	// async tool_result_request and emit the matching response.
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	input := json.RawMessage(`{"prompt":"hello"}`)
	event := types.HarnessEvent{
		Type:      "tool_result_request",
		RequestID: "async-tool-1",
		ToolUseID: "tc_1",
		ToolName:  "async_echo",
		Input:     input,
	}

	must.NoError(tr.Emit(event), "Emit")

	// Close send side so the server's Recv loop completes.
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	must.Len(received, 1)
	got := received[0]
	check.Equal("tool_result_request", got.Type)
	check.Equal("async-tool-1", got.RequestId)
	check.Equal("tc_1", got.ToolUseId)
	check.Equal("async_echo", got.ToolName)
	check.JSONEq(`{"prompt":"hello"}`, string(got.Input))
}
