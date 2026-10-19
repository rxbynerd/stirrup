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
	require.NoError(t, err, "NewGRPCTransport")

	cleanup := func() {
		_ = tr.Close()
		grpcServer.Stop()
	}

	return tr, srv, cleanup
}

func TestGRPCTransport_Emit_DeliversEventToServer(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}

	require.NoError(t, tr.Emit(event), "Emit")

	// Close send side so server's Recv loop finishes.
	_ = tr.stream.CloseSend()

	// Give the server goroutine a moment to process.
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	assert.Equal(t, "text_delta", received[0].Type)
	assert.Equal(t, "hello world", received[0].Text)
}

func TestGRPCTransport_Emit_PreservesToolCallInput(t *testing.T) {
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

	require.NoError(t, tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	assert.Equal(t, "read_file", received[0].Name)
	assert.JSONEq(t, `{"file":"test.go"}`, string(received[0].Input))
}

func TestGRPCTransport_OnControl_DeliversControlEvent(t *testing.T) {
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
		require.Equal(t, "cancel", ev.Type)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for control event")
	}
}

func TestGRPCTransport_OnControl_DeliversTaskAssignment(t *testing.T) {
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
		require.Equal(t, "task_assignment", ev.Type)
		require.NotNil(t, ev.Task, "Task")
		assert.Equal(t, "run-123", ev.Task.RunID)
		assert.Equal(t, "fix the bug", ev.Task.Prompt)
		assert.Equal(t, 10, ev.Task.MaxTurns)
		assert.Equal(t, "anthropic", ev.Task.Provider.Type)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for task assignment")
	}
}

func TestGRPCTransport_OnControl_DeliversPermissionResponse(t *testing.T) {
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
		require.Equal(t, "permission_response", ev.Type)
		assert.Equal(t, "req-42", ev.RequestID)
		require.NotNil(t, ev.Allowed, "Allowed")
		assert.True(t, *ev.Allowed, "Allowed")
		assert.Equal(t, "user approved", ev.Reason)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for permission response")
	}
}

func TestGRPCTransport_BidirectionalFlow(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var serverStream pb.HarnessService_RunTaskServer
	select {
	case serverStream = <-srv.streamCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for server stream")
	}

	controlEvents := make(chan types.ControlEvent, 10)
	tr.OnControl(func(event types.ControlEvent) {
		controlEvents <- event
	})

	timeout := int32(300)
	require.NoError(t, serverStream.Send(&pb.ControlEvent{
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
		require.Equal(t, "task_assignment", ev.Type)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for task_assignment")
	}

	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "working..."}), "Emit text_delta")
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc_1", Name: "bash"}), "Emit tool_call")

	require.NoError(t, serverStream.Send(&pb.ControlEvent{
		Type:         "user_response",
		UserResponse: "yes, continue",
	}), "server send user_response")

	select {
	case ev := <-controlEvents:
		require.Equal(t, "user_response", ev.Type)
		assert.Equal(t, "yes, continue", ev.UserResponse)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for user_response")
	}

	time.Sleep(50 * time.Millisecond)
	received := srv.getReceived()
	require.GreaterOrEqual(t, len(received), 2, "received events")
	assert.Equal(t, "text_delta", received[0].Type)
	assert.Equal(t, "tool_call", received[1].Type)
}

func TestGRPCTransport_Close_ReturnsNoError(t *testing.T) {
	srv := newTestServer()
	tr, _, _ := setupTestTransport(t, srv)

	require.NoError(t, tr.Close(), "Close")
}

// TestGRPCTransport_CloseDeliversPendingEvents pins that events emitted
// immediately before Close reach the peer. Send only queues a frame for
// the writer goroutine, so the connection must not be torn down until
// the peer has acknowledged the terminal "error"/"done" pair a control
// plane needs to tell a rejected config from a crashed harness.
// Repeated because delivery depends on writer scheduling.
func TestGRPCTransport_CloseDeliversPendingEvents(t *testing.T) {
	const attempts = 50
	useShortStreamEndGrace(t)

	for attempt := range attempts {
		func() {
			srv := newTestServer()
			tr, _, cleanup := setupTestTransport(t, srv)
			defer cleanup()

			require.NoError(t, tr.Emit(types.HarnessEvent{Type: "error", Message: "config validation: bad"}), "attempt %d: Emit error event", attempt)
			require.NoError(t, tr.Emit(types.HarnessEvent{Type: "done", StopReason: "error"}), "attempt %d: Emit done event", attempt)

			start := time.Now()
			require.NoError(t, tr.Close(), "attempt %d: Close", attempt)
			// Close must return on stream end, not by waiting out the grace,
			// so a drain regression fails here instead of timing out the package.
			require.Less(t, time.Since(start), streamEndGrace, "attempt %d: Close should return on stream end", attempt)

			received := srv.getReceived()
			require.Len(t, received, 2, "attempt %d: server received events %v, want the error/done pair", attempt, received)
			require.Equal(t, "error", received[0].Type, "attempt %d", attempt)
			require.Equal(t, "done", received[1].Type, "attempt %d", attempt)
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
	require.NoError(t, err, "NewGRPCTransport")

	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "error", Message: "boom"}), "Emit error event")
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "done", StopReason: "error"}), "Emit done event")

	// Fires once the server has seen the half-close, i.e. while Close is
	// inside its wait for stream end.
	emitErrCh := make(chan error, 1)
	go func() {
		<-srv.halfClosed
		emitErrCh <- tr.Emit(types.HarnessEvent{Type: "heartbeat"})
	}()

	start := time.Now()
	require.NoError(t, tr.Close(), "Close")
	elapsed := time.Since(start)

	require.ErrorIs(t, <-emitErrCh, ErrTransportClosed, "Emit during Close must return ErrTransportClosed so it never reaches SendMsg")

	// Short-circuiting the drain is the failure this guards: the abort
	// would return Close in microseconds instead of waiting out linger.
	assert.GreaterOrEqual(t, elapsed, linger, "Close should wait for stream end")

	received := srv.getReceived()
	require.Len(t, received, 2, "server received events %v, want the error/done pair", received)
	assert.Equal(t, "done", received[1].Type, "last event")
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

	require.NoError(t, tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)

	assert.NotContains(t, received[0].Content, "sk-ant-", "Anthropic API key was not scrubbed")
	assert.NotContains(t, received[0].Message, "ghp_", "GitHub PAT was not scrubbed")
	assert.Contains(t, received[0].Content, "[REDACTED]", "Content should carry the redaction marker")
	assert.Contains(t, received[0].Message, "[REDACTED]", "Message should carry the redaction marker")
}

func TestGRPCTransport_EmitScrubsThinkingDeltaAndUserMessageText(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	events := []types.HarnessEvent{
		{Type: "thinking_delta", Text: "the key is sk-ant-abc123-secret and ghp_abcdef1234567890"},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: "r1", Text: "use sk-ant-abc123-secret and ghp_abcdef1234567890"},
	}
	for _, ev := range events {
		require.NoError(t, tr.Emit(ev), "Emit %s", ev.Type)
	}

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, len(events))
	for _, ev := range received {
		assert.NotContains(t, ev.Text, "sk-ant-", "%s text was not scrubbed", ev.Type)
		assert.NotContains(t, ev.Text, "ghp_", "%s text was not scrubbed", ev.Type)
		assert.Contains(t, ev.Text, "[REDACTED]", "%s text was not scrubbed", ev.Type)
	}
	assert.Equal(t, types.UserMessageSourceUserResponse, received[1].Source, "user_message source should be unchanged")
	assert.Equal(t, "r1", received[1].RequestId, "user_message request_id should be unchanged")
}

func TestGRPCTransport_EmitCapsScrubbedUserMessageText(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	text := "sk-ant-abc123-secret " + strings.Repeat("u", maxEventFieldBytes)
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "user_message", Source: types.UserMessageSourceInitialPrompt, Text: text}), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	got := received[0].Text
	assert.LessOrEqual(t, len(got), maxEventFieldBytes, "user_message text should be capped")
	assert.Contains(t, got, "bytes shown]", "capped user_message text should carry the truncation marker")
	assert.True(t, strings.HasPrefix(got, "[REDACTED]"), "capped text should start with the scrubbed secret, got %.40q", got)
}

// TestGRPCTransport_EmitScrubsInput mirrors the stdio Input-scrub fix on
// the gRPC transport: a permission_request forwarding a tool call's raw
// input must not leak a latch-tripping secret to the control plane.
func TestGRPCTransport_EmitScrubsInput(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:  "permission_request",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	require.NoError(t, tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	assert.NotContains(t, string(received[0].Input), "AKIAQWERTYUIOPASDFGH", "live-shaped AWS key should be scrubbed from event Input")
	assert.True(t, json.Valid(received[0].Input), "scrubbed Input should be valid JSON: %q", received[0].Input)
}

// TestGRPCTransport_EmitScrubsToolCallInput is the regression test for
// issue #593: now that the core loop emits "tool_call" on the transport,
// its Input must go through the same scrub path as permission_request and
// tool_result_request rather than a new, unscrubbed seam.
func TestGRPCTransport_EmitScrubsToolCallInput(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	event := types.HarnessEvent{
		Type:  "tool_call",
		ID:    "tc_1",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	require.NoError(t, tr.Emit(event), "Emit")

	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	assert.NotContains(t, string(received[0].Input), "AKIAQWERTYUIOPASDFGH", "live-shaped AWS key should be scrubbed from tool_call Input")
	assert.True(t, json.Valid(received[0].Input), "scrubbed Input should be valid JSON: %q", received[0].Input)
	assert.Equal(t, "tc_1", received[0].Id, "scrubbing must not disturb other fields")
}

func TestGRPCTransport_Emit_LogsSecretRedactedInOutput(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	require.NoError(t, tr.Emit(types.HarnessEvent{
		Type:    "tool_result",
		Content: "key=sk-ant-abc123-secret",
	}), "Emit")
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	got := secBuf.String()
	assert.Contains(t, got, `"event":"secret_redacted_in_output"`)
	assert.Contains(t, got, `"location":"transport.grpc.event.content"`)
	assert.Contains(t, got, `"pattern":"anthropic_api_key"`)
}

func TestGRPCTransport_Emit_LogsNoSecurityEventForCleanText(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "hello"}), "Emit")
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, secBuf.String(), "expected no security events")
}

// TestGRPCTransport_OnControl_TaskAssignmentKeepsAzureProviderFields pins that
// a task_assignment carrying APIKeyHeader and QueryParams round-trips through
// the proto wire format without losing data.
func TestGRPCTransport_OnControl_TaskAssignmentKeepsAzureProviderFields(t *testing.T) {
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
		require.NotNil(t, ev.Task, "Task")
		assert.Equal(t, "api-key", ev.Task.Provider.APIKeyHeader, "Provider.APIKeyHeader")
		assert.Equal(t, "preview", ev.Task.Provider.QueryParams["api-version"], "Provider.QueryParams[api-version]")
		assert.Equal(t, "https://example.openai.azure.com/openai/v1", ev.Task.Provider.BaseURL, "Provider.BaseURL")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for task assignment")
	}
}

func TestGRPCTransport_StreamError_StopsReadLoop(t *testing.T) {
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
		require.FailNow(t, "timed out waiting for read loop to exit after stream error")
	}
}

func TestGRPCTransport_OnControl_ToolResultResponse_Success(t *testing.T) {
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
		require.Equal(t, "tool_result_response", ev.Type)
		assert.Equal(t, "async-tool-7", ev.RequestID)
		assert.Equal(t, "result body for the model", ev.Content)
		assert.Nil(t, ev.IsError, "IsError should be nil for a success response")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for tool_result_response")
	}
}

func TestGRPCTransport_OnControl_ToolResultResponse_IsError(t *testing.T) {
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
		require.Equal(t, "tool_result_response", ev.Type)
		assert.Equal(t, "async-tool-9", ev.RequestID)
		assert.Equal(t, "upstream said no", ev.Content)
		require.NotNil(t, ev.IsError, "IsError should be set for an error response")
		assert.True(t, *ev.IsError, "IsError should be true for an error response")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for tool_result_response")
	}
}

func TestGRPCTransport_Emit_ToolResultRequestWireFields(t *testing.T) {
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

	require.NoError(t, tr.Emit(event), "Emit")

	// Close send side so the server's Recv loop completes.
	_ = tr.stream.CloseSend()
	time.Sleep(50 * time.Millisecond)

	received := srv.getReceived()
	require.Len(t, received, 1)
	got := received[0]
	assert.Equal(t, "tool_result_request", got.Type)
	assert.Equal(t, "async-tool-1", got.RequestId)
	assert.Equal(t, "tc_1", got.ToolUseId)
	assert.Equal(t, "async_echo", got.ToolName)
	assert.JSONEq(t, `{"prompt":"hello"}`, string(got.Input))
}
