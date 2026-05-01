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
		Trace:        trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
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

func TestAsyncDispatch_HappyPath(t *testing.T) {
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
		t.Errorf("tool_result_request was never emitted")
	}()

	output, success := loop.dispatchToolCall(context.Background(), call)
	if !success {
		t.Fatalf("expected success=true, got false; output=%q", output)
	}
	if output != "async output ok" {
		t.Fatalf("expected output 'async output ok', got %q", output)
	}

	// Verify the emitted request event carries tool_use_id, tool name,
	// and request_id.
	events := tr.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(events))
	}
	req := events[0]
	if req.Type != "tool_result_request" {
		t.Fatalf("expected tool_result_request, got %q", req.Type)
	}
	if req.ToolUseID != "tc_async_1" {
		t.Fatalf("expected ToolUseID tc_async_1, got %q", req.ToolUseID)
	}
	if req.ToolName != "async_echo" {
		t.Fatalf("expected ToolName async_echo, got %q", req.ToolName)
	}
	if req.RequestID == "" {
		t.Fatalf("expected non-empty RequestID")
	}

	// Pending entry must be cleaned up after resolution.
	if loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits, got %d", loop.asyncCorrelatorForTest().PendingCount())
	}
}

func TestAsyncDispatch_UpstreamError(t *testing.T) {
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
	if success {
		t.Fatalf("expected success=false on is_error response")
	}
	if output != "upstream said no" {
		t.Fatalf("expected upstream content verbatim, got %q", output)
	}
}

func TestAsyncDispatch_Timeout(t *testing.T) {
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
	if success {
		t.Fatalf("expected success=false on timeout, got true; output=%q", output)
	}
	if !strings.Contains(output, "timeout") {
		t.Fatalf("expected output to mention 'timeout', got %q", output)
	}
	if !strings.Contains(output, "async_echo") {
		t.Fatalf("expected output to mention tool name, got %q", output)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("returned too quickly (%s); timeout did not fire", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned too slowly (%s); per-call timeout override was ignored", elapsed)
	}
	// Cleanup: pending entry must be removed.
	if loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits after timeout, got %d", loop.asyncCorrelatorForTest().PendingCount())
	}
}

func TestAsyncDispatch_CtxCancellation(t *testing.T) {
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
	if success {
		t.Fatalf("expected success=false on ctx cancel")
	}
	if !strings.Contains(output, "cancelled") {
		t.Fatalf("expected output to mention 'cancelled', got %q", output)
	}
	if loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits after ctx cancel, got %d", loop.asyncCorrelatorForTest().PendingCount())
	}
}

func TestAsyncDispatch_TransportEmitFailure(t *testing.T) {
	tr := newAsyncTestTransport()
	tr.emitErr = errors.New("simulated transport disconnect")
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	call := types.ToolCall{
		ID:    "tc_async_emit",
		Name:  "async_echo",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	if success {
		t.Fatalf("expected success=false on emit failure")
	}
	if !strings.Contains(output, "transport_disconnect") {
		t.Fatalf("expected output to contain 'transport_disconnect', got %q", output)
	}
	// Correlator must clean up the pending entry on emit failure.
	if loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits after emit failure, got %d", loop.asyncCorrelatorForTest().PendingCount())
	}
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

	// Wait until both pending awaits are registered. We poll the count of
	// emitted tool_result_request events as a proxy: each in-flight async
	// dispatch emits exactly one such event before blocking on the
	// correlator. Reading loop.asyncCorrelator directly from this
	// goroutine would race with the dispatch goroutines writing it under
	// sync.Once; the correlator's own state is fine to inspect via
	// PendingCount() once construction is established.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, e := range tr.Events() {
			if e.Type == "tool_result_request" {
				count++
			}
		}
		if count == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	correlator := loop.ensureAsyncCorrelator()
	if correlator == nil {
		t.Fatalf("expected async correlator to be constructed by now")
	}
	if got := correlator.PendingCount(); got != 2 {
		t.Fatalf("expected 2 pending awaits, got %d", got)
	}

	// Pull request IDs out of the emitted events. Map them to the tool
	// use IDs so we know which dispatch returned which payload.
	events := tr.Events()
	idByToolUseID := map[string]string{}
	for _, e := range events {
		if e.Type == "tool_result_request" {
			idByToolUseID[e.ToolUseID] = e.RequestID
		}
	}
	id1, id2 := idByToolUseID["tc1"], idByToolUseID["tc2"]
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected two distinct request IDs, got %q and %q", id1, id2)
	}

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
	if !res1.success || res1.output != "result-for-tc1" {
		t.Fatalf("call1 wrong: success=%v output=%q", res1.success, res1.output)
	}
	if !res2.success || res2.output != "result-for-tc2" {
		t.Fatalf("call2 wrong: success=%v output=%q", res2.success, res2.output)
	}
	if loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits after both resolutions, got %d",
			loop.asyncCorrelatorForTest().PendingCount())
	}
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
	if success {
		t.Fatalf("expected success=false on NullTransport, got true; output=%q", output)
	}
	if !strings.Contains(output, "unavailable") {
		t.Fatalf("expected output to mention 'unavailable', got %q", output)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("dispatch should fail fast, but took %s", elapsed)
	}
	if loop.asyncCorrelatorForTest() != nil {
		t.Fatalf("async correlator should not have been constructed for a NullTransport")
	}
}

func TestAsyncDispatch_AsyncHandlerInternalError(t *testing.T) {
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
	if success {
		t.Fatalf("expected success=false on AsyncHandler error")
	}
	if !strings.Contains(output, "internal error") {
		t.Fatalf("expected 'internal error' in output, got %q", output)
	}
	// No event should have been emitted, and no pending entry registered.
	if len(tr.Events()) != 0 {
		t.Fatalf("expected no events emitted on preflight error, got %d", len(tr.Events()))
	}
	if loop.asyncCorrelatorForTest() != nil && loop.asyncCorrelatorForTest().PendingCount() != 0 {
		t.Fatalf("expected 0 pending awaits, got %d", loop.asyncCorrelatorForTest().PendingCount())
	}
}

func TestSyncToolPath_Unchanged(t *testing.T) {
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
	if !success || output != "sync result" {
		t.Fatalf("sync tool: success=%v output=%q", success, output)
	}
	// No tool_result_request event should have been emitted.
	for _, e := range tr.Events() {
		if e.Type == "tool_result_request" {
			t.Fatalf("sync tool should not emit tool_result_request, got %+v", e)
		}
	}
	// Async correlator must not be constructed for a sync-only call.
	if loop.asyncCorrelatorForTest() != nil {
		t.Fatalf("async correlator should not have been constructed for a sync tool")
	}
}

func TestAsyncDispatch_StallDetectionStillFires(t *testing.T) {
	// Three identical async resolutions should still trigger
	// stallDetector — the dispatch path is still synchronous from the
	// stall detector's point of view (one record per call).
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
		go func(turn int) {
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
							Content:   fmt.Sprintf("repeat-%d", turn),
						})
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
		}(i)

		output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
			ID:    fmt.Sprintf("tc_stall_%d", i),
			Name:  "async_echo",
			Input: input,
		})
		<-done
		if !success {
			t.Fatalf("turn %d: dispatch failed: %s", i, output)
		}
		outcome := stall.recordToolCall("async_echo", input, success)
		if i < 2 && outcome != "" {
			t.Fatalf("turn %d: unexpected stall outcome %q", i, outcome)
		}
		if i == 2 && outcome != "stalled" {
			t.Fatalf("turn 2: expected 'stalled' outcome, got %q", outcome)
		}
	}
}
