package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// asyncTestTransport captures emitted events and lets the test inject
// control events. It implements the same fan-out OnControl semantics that
// the production stdio/grpc transports do, so the loop's async correlator
// can attach normally. emitErr, when non-nil, is returned from Emit
// (simulating a transport disconnect during async dispatch).
type asyncTestTransport struct {
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
	events   []types.HarnessEvent
	emitErr  error
}

func newAsyncTestTransport() *asyncTestTransport {
	return &asyncTestTransport{}
}

func (t *asyncTestTransport) Emit(event types.HarnessEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.emitErr != nil {
		return t.emitErr
	}
	t.events = append(t.events, event)
	return nil
}

func (t *asyncTestTransport) OnControl(handler func(types.ControlEvent)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers = append(t.handlers, handler)
}

func (t *asyncTestTransport) Close() error { return nil }

func (t *asyncTestTransport) FireControl(event types.ControlEvent) {
	t.mu.Lock()
	hs := make([]func(types.ControlEvent), len(t.handlers))
	copy(hs, t.handlers)
	t.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

func (t *asyncTestTransport) Events() []types.HarnessEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]types.HarnessEvent, len(t.events))
	copy(out, t.events)
	return out
}

func (t *asyncTestTransport) lastRequestID(eventType string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.events) - 1; i >= 0; i-- {
		if t.events[i].Type == eventType {
			return t.events[i].RequestID
		}
	}
	return ""
}

// buildAsyncTestLoop constructs a loop with the supplied transport and the
// caller's tool registered. Other components are minimal/no-op so the test
// can call dispatchToolCall directly.
func buildAsyncTestLoop(t *testing.T, tr transport.Transport, asyncTool *tool.Tool) *AgenticLoop {
	t.Helper()
	registry := tool.NewRegistry()
	registry.Register(asyncTool)
	return &AgenticLoop{
		Provider:     nil,
		Router:       router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:       prompt.NewDefaultPromptBuilder(),
		Context:      contextpkg.NewSlidingWindowStrategy(),
		Tools:        registry,
		Edit:         edit.NewWholeFileStrategy(),
		Verifier:     verifier.NewNoneVerifier(),
		Permissions:  permission.NewAllowAll(),
		Git:          git.NewNoneGitStrategy(),
		Transport:    tr,
		Trace:        trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
		Tracer:       noop.NewTracerProvider().Tracer(""),
		TraceContext: context.Background(),
		Metrics:      observability.NewNoopMetrics(),
		Logger:       slog.Default(),
	}
}

// asyncEchoTool is a minimal async tool: its preflight does no work, just
// hands the request_id back to the loop. The control-plane response carries
// the actual payload.
func asyncEchoTool() *tool.Tool {
	return &tool.Tool{
		Name:        "async_echo",
		Description: "test async tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			return tool.AsyncDispatch{}, nil
		},
	}
}

func TestAsyncDispatch_ReturnsControlPlaneOutput(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_async_1",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	// Fire the matching tool_result_response after the loop has emitted
	// its tool_result_request and registered its pending entry.
	go func() {
		// Spin until the request event has been emitted so we know the
		// pending channel exists.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   "async output ok",
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
		assert.Fail(t, "tool_result_request was never emitted")
	}()

	output, success := loop.dispatchToolCall(context.Background(), call)
	require.True(t, success, "output %q", output)
	require.Equal(t, "async output ok", output)

	events := tr.Events()
	require.Len(t, events, 1)
	req := events[0]
	require.Equal(t, "tool_result_request", req.Type)
	require.Equal(t, "tc_async_1", req.ToolUseID)
	require.Equal(t, "async_echo", req.ToolName)
	require.NotEmpty(t, req.RequestID)

	require.Zero(t, loop.asyncCorrelatorForTest().PendingCount())
}

func TestAsyncDispatch_UpstreamErrorIsPrefixed(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_async_err",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				yes := true
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   "upstream said no",
					IsError:   &yes,
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	output, success := loop.dispatchToolCall(context.Background(), call)
	require.False(t, success, "output %q", output)
	// The is_error path must wrap control-plane content with a structured
	// prefix so the model can disambiguate upstream failures from
	// harness-side failures (transport_disconnect, timeout, internal error).
	require.Contains(t, output, "upstream_error:")
	require.Contains(t, output, "async_echo")
	require.Contains(t, output, "upstream said no")
}

func TestAsyncDispatch_UpstreamError_ScrubsSecrets(t *testing.T) {
	// The control plane is partially trusted: a misbehaving or
	// compromised control plane could embed secret-shaped strings in the
	// error payload, which reaches the JSONL trace via
	// RecordToolCall.ErrorReason without going through the transport's
	// outbound scrub, so scrubbing must happen at the point of entry.
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	const rawSecret = "sk-ant-api03-1234567890abcdefXYZ"
	call := types.ToolCall{
		ID:    "tc_async_err_secret",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				yes := true
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   "auth failed: token=" + rawSecret + " is invalid",
					IsError:   &yes,
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	output, success := loop.dispatchToolCall(context.Background(), call)
	require.False(t, success, "output %q", output)
	require.NotContains(t, output, rawSecret)
	require.Contains(t, output, "[REDACTED]")
	require.Contains(t, output, "upstream_error:")
}

func TestAsyncDispatch_TruncatesOversizeContent(t *testing.T) {
	// The control plane supplies tool result content as an unbounded
	// string; the harness must cap it before the value flows into tool
	// output, message history, or the wire — same discipline as the
	// 1MB run_command output cap.
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	const maxResultBytes = 1 << 20 // matches maxAsyncToolResultBytes
	oversize := strings.Repeat("a", maxResultBytes+512)

	call := types.ToolCall{
		ID:    "tc_async_oversize",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   oversize,
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	output, success := loop.dispatchToolCall(context.Background(), call)
	require.True(t, success, "output length %d", len(output))

	const suffix = "... [truncated by harness]"
	tail := output
	if len(tail) > len(suffix)+16 {
		tail = tail[len(tail)-len(suffix)-16:]
	}
	require.True(t, strings.HasSuffix(output, suffix), "output tail %q", tail)
	require.Len(t, output, maxResultBytes+len(suffix))
}

func TestAsyncDispatch_TimeoutReturnsErrorAndClearsPending(t *testing.T) {
	tr := newAsyncTestTransport()
	tl := asyncEchoTool()
	tl.AsyncHandler = func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
		return tool.AsyncDispatch{Timeout: 25 * time.Millisecond}, nil
	}
	loop := buildAsyncTestLoop(t, tr, tl)

	call := types.ToolCall{
		ID:    "tc_async_timeout",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	start := time.Now()
	output, success := loop.dispatchToolCall(context.Background(), call)
	elapsed := time.Since(start)
	require.False(t, success, "output %q", output)
	require.Contains(t, output, "timeout")
	require.Contains(t, output, "async_echo")
	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond, "timeout did not fire")
	require.LessOrEqual(t, elapsed, 2*time.Second, "per-call timeout override was ignored")
	require.Zero(t, loop.asyncCorrelatorForTest().PendingCount())
}

func TestAsyncDispatch_ContextCancelReportsCancelled(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_async_cancel",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the request is emitted so we exercise the
	// ctx.Done branch of correlator.Await.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if tr.lastRequestID("tool_result_request") != "" {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	output, success := loop.dispatchToolCall(ctx, call)
	require.False(t, success, "output %q", output)
	require.Contains(t, output, "cancelled")
	require.Zero(t, loop.asyncCorrelatorForTest().PendingCount())
}

func TestAsyncDispatch_EmitFailureReportsTransportDisconnect(t *testing.T) {
	tr := newAsyncTestTransport()
	tr.emitErr = errors.New("simulated transport disconnect")
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_async_emit",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	require.False(t, success, "output %q", output)
	require.Contains(t, output, "transport_disconnect")
	// The correlator must drop the pending entry when the emit fails.
	require.Zero(t, loop.asyncCorrelatorForTest().PendingCount())
}

func TestAsyncDispatch_OutOfOrderResolution(t *testing.T) {
	// The dispatch loop is sequential, so two async tool calls cannot be
	// in flight simultaneously via the loop itself. This test exercises
	// the correlator's out-of-order routing directly: register two
	// pending Awaits via two concurrent dispatchToolCall invocations,
	// then resolve them in reverse order. Each should return its own
	// payload — proof that correlation is by request_id, not order of
	// arrival.
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call1 := types.ToolCall{ID: "tc1", Name: "async_echo", Input: json.RawMessage(`{}`)}
	call2 := types.ToolCall{ID: "tc2", Name: "async_echo", Input: json.RawMessage(`{}`)}

	type res struct {
		output  string
		success bool
	}
	r1 := make(chan res, 1)
	r2 := make(chan res, 1)

	go func() {
		out, ok := loop.dispatchToolCall(context.Background(), call1)
		r1 <- res{out, ok}
	}()
	go func() {
		out, ok := loop.dispatchToolCall(context.Background(), call2)
		r2 <- res{out, ok}
	}()

	// Poll the count of emitted tool_result_request events as a proxy for
	// both pending awaits being registered: reading loop.asyncCorrelator
	// directly here would race with the dispatch goroutines writing it
	// under sync.Once.
	require.Eventually(t, func() bool {
		count := 0
		for _, e := range tr.Events() {
			if e.Type == "tool_result_request" {
				count++
			}
		}
		return count == 2
	}, 2*time.Second, time.Millisecond)

	correlator := loop.ensureAsyncCorrelator()
	require.NotNil(t, correlator, "async correlator should be constructed by now")
	require.Equal(t, 2, correlator.PendingCount())

	events := tr.Events()
	idByToolUseID := map[string]string{}
	for _, e := range events {
		if e.Type == "tool_result_request" {
			idByToolUseID[e.ToolUseID] = e.RequestID
		}
	}
	id1, id2 := idByToolUseID["tc1"], idByToolUseID["tc2"]
	require.NotEmpty(t, id1)
	require.NotEmpty(t, id2)
	require.NotEqual(t, id1, id2)

	// Resolve in reverse order: tc2 first, then tc1.
	tr.FireControl(types.ControlEvent{
		Type:      "tool_result_response",
		RequestID: id2,
		Content:   "result-for-tc2",
	})
	tr.FireControl(types.ControlEvent{
		Type:      "tool_result_response",
		RequestID: id1,
		Content:   "result-for-tc1",
	})

	res1 := <-r1
	res2 := <-r2
	require.True(t, res1.success, "call1 output %q", res1.output)
	require.Equal(t, "result-for-tc1", res1.output)
	require.True(t, res2.success, "call2 output %q", res2.output)
	require.Equal(t, "result-for-tc2", res2.output)
	require.Zero(t, loop.asyncCorrelatorForTest().PendingCount())
}

func TestAsyncDispatch_NullTransportFailsFast(t *testing.T) {
	// Sub-agents run with NullTransport. An async tool there must not
	// block, must not register a pending entry, and must surface a
	// recoverable error to the model.
	loop := buildAsyncTestLoop(t, transport.NewNullTransport(), asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_null",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	start := time.Now()
	output, success := loop.dispatchToolCall(context.Background(), call)
	elapsed := time.Since(start)
	require.False(t, success, "output %q", output)
	require.Contains(t, output, "unavailable")
	require.LessOrEqual(t, elapsed, 200*time.Millisecond, "dispatch should fail fast")
	require.Nil(t, loop.asyncCorrelatorForTest(), "no correlator should be constructed for a NullTransport")
}

func TestAsyncDispatch_PreflightErrorEmitsNothing(t *testing.T) {
	tr := newAsyncTestTransport()
	tl := asyncEchoTool()
	tl.AsyncHandler = func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
		return tool.AsyncDispatch{}, errors.New("preflight blew up")
	}
	loop := buildAsyncTestLoop(t, tr, tl)

	call := types.ToolCall{
		ID:    "tc_pre",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}
	output, success := loop.dispatchToolCall(context.Background(), call)
	require.False(t, success, "output %q", output)
	require.Contains(t, output, "internal error")
	require.Empty(t, tr.Events())
	if correlator := loop.asyncCorrelatorForTest(); correlator != nil {
		require.Zero(t, correlator.PendingCount())
	}
}

func TestSyncToolDispatch_SkipsControlPlane(t *testing.T) {
	// A purely synchronous tool (no AsyncHandler) must still work. This
	// guards the "async-handler-nil = sync" contract.
	tr := newAsyncTestTransport()
	syncTool := &tool.Tool{
		Name:        "sync_test",
		Description: "purely synchronous test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "sync result", nil
		},
	}
	loop := buildAsyncTestLoop(t, tr, syncTool)

	call := types.ToolCall{
		ID:    "tc_sync",
		Name:  "sync_test",
		Input: json.RawMessage(`{}`),
	}
	output, success := loop.dispatchToolCall(context.Background(), call)
	require.True(t, success, "output %q", output)
	require.Equal(t, "sync result", output)
	for _, e := range tr.Events() {
		require.NotEqual(t, "tool_result_request", e.Type, "sync tool emitted %+v", e)
	}
	require.Nil(t, loop.asyncCorrelatorForTest(), "no correlator should be constructed for a sync tool")
}

func TestAsyncDispatch_RepeatedResolutionsTriggerStallNudge(t *testing.T) {
	// Three identical async resolutions should still trigger the
	// stallDetector nudge — the dispatch path is still synchronous from
	// the stall detector's point of view (one record per call).
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	stall := &stallDetector{}
	input := json.RawMessage(`{"x":1}`)

	for i := 0; i < 3; i++ {
		// Each iteration: register a one-shot resolver that fires as
		// soon as this iteration's request lands on the wire. Counting
		// emitted events == iteration index disambiguates from earlier
		// iterations' (already resolved) requests.
		expectedIndex := i + 1
		done := make(chan struct{})
		go func() {
			defer close(done)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				events := tr.Events()
				if len(events) >= expectedIndex {
					req := events[expectedIndex-1]
					if req.Type == "tool_result_request" && req.RequestID != "" {
						tr.FireControl(types.ControlEvent{
							Type:      "tool_result_response",
							RequestID: req.RequestID,
							Content:   "repeat",
						})
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
		}()

		output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
			ID:    fmt.Sprintf("tc_stall_%d", i),
			Name:  "async_echo",
			Input: input,
		})
		<-done
		require.True(t, success, "turn %d: dispatch failed: %s", i, output)
		outcome := stall.recordToolCall("async_echo", input, output, success)
		require.Empty(t, outcome, "turn %d: unexpected stall outcome", i)
		_, nudged := stall.takeNudge()
		require.Equal(t, i == 2, nudged, "turn %d: nudge expected only on the third identical call", i)
	}
}
