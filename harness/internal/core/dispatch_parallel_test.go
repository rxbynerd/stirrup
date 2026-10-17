package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// stubDenyGuardRail is a GuardRail that returns VerdictDeny with a
// caller-supplied Reason for every Check. Used to drive the
// PhasePreTool deny branch in planAndDispatch without standing up a
// real guard adapter.
type stubDenyGuardRail struct {
	reason string
}

func (g *stubDenyGuardRail) Check(_ context.Context, _ guard.Input) (*guard.Decision, error) {
	return &guard.Decision{
		Verdict: guard.VerdictDeny,
		Reason:  g.reason,
		GuardID: "stub-deny",
	}, nil
}

// buildParallelDispatchLoop is a small adjacent helper that constructs a loop
// with multiple pre-built tools registered. buildAsyncTestLoop in
// loop_async_test.go only registers a single tool, which is insufficient for
// the mixed sync+async scenarios under test here. The shape mirrors that
// helper otherwise so the rest of the wiring stays consistent.
func buildParallelDispatchLoop(t *testing.T, tr transport.Transport, tools ...*tool.Tool) *AgenticLoop {
	t.Helper()
	registry := tool.NewRegistry()
	for _, tl := range tools {
		registry.Register(tl)
	}
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
		Trace:        tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
		Tracer:       sdktrace.NewTracerProvider().Tracer(""),
		TraceContext: context.Background(),
		Metrics:      observability.NewNoopMetrics(),
		Logger:       slog.Default(),
	}
}

// configWithMaxParallel returns a minimal RunConfig with the
// ToolDispatch MaxParallel knob set. Validation is not invoked here —
// these are unit tests of planAndDispatch — so the helper sets the
// field directly.
func configWithMaxParallel(n int) *types.RunConfig {
	return &types.RunConfig{
		RunID:        "test-dispatch",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: n},
	}
}

// fireResponseWhenEmitted polls the transport's event log for a
// tool_result_request matching the given tool_use ID and fires a matching
// tool_result_response after the supplied delay. Returns when the response
// has been delivered or the test deadline expires.
//
// Polling is bounded to a short total deadline so any wiring bug stalls the
// test loudly rather than silently extending its wall-time.
func fireResponseWhenEmitted(
	t *testing.T,
	tr *asyncTestTransport,
	toolUseID string,
	delay time.Duration,
	content string,
) {
	t.Helper()
	requestID := waitForRequestID(t, tr, toolUseID, 2*time.Second)
	time.Sleep(delay)
	tr.FireControl(types.ControlEvent{
		Type:      "tool_result_response",
		RequestID: requestID,
		Content:   content,
	})
}

// waitForRequestID returns the request_id the loop emitted for the given
// tool_use ID, blocking until the matching tool_result_request appears on
// the wire or the deadline expires.
func waitForRequestID(t *testing.T, tr *asyncTestTransport, toolUseID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range tr.Events() {
			if e.Type == "tool_result_request" && e.ToolUseID == toolUseID {
				return e.RequestID
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no tool_result_request emitted for tool_use_id=%q within %s", toolUseID, timeout)
	return ""
}

// TestParallelDispatch_FiveConcurrent_CompleteInMaxNotSum is the headline
// acceptance test: with MaxParallel=5, five 200ms async calls must complete
// in roughly max(durations) rather than the sequential sum of ~1s. A
// generous 500ms upper bound leaves room for scheduler jitter while still
// catching any accidental re-serialisation.
func TestParallelDispatch_FiveConcurrent_CompleteInMaxNotSum(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	config := configWithMaxParallel(5)

	const n = 5
	calls := make([]types.ToolCall, n)
	for i := 0; i < n; i++ {
		calls[i] = types.ToolCall{
			ID:    fmt.Sprintf("tc_par_%d", i),
			Name:  "async_echo",
			Input: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}
	}

	for i := 0; i < n; i++ {
		i := i
		go fireResponseWhenEmitted(t, tr, calls[i].ID, 200*time.Millisecond,
			fmt.Sprintf("result-%d", i))
	}

	start := time.Now()
	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	elapsed := time.Since(start)

	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("expected concurrent dispatch under 500ms, took %s (sequential path would be ~1s)", elapsed)
	}
	for i, r := range results {
		if r.IsError {
			t.Fatalf("result %d unexpectedly errored: %q", i, r.Content)
		}
		if r.ToolUseID != calls[i].ID {
			t.Errorf("result %d: ToolUseID=%q want %q", i, r.ToolUseID, calls[i].ID)
		}
		if r.Content != fmt.Sprintf("result-%d", i) {
			t.Errorf("result %d: content=%q want %q", i, r.Content, fmt.Sprintf("result-%d", i))
		}
	}
}

// TestParallelDispatch_OutOfOrderResolution_PreservesCallOrder verifies the
// correlator's out-of-order routing AND that the result slice is indexed by
// original call order, not resolution order. Critical because tool_use IDs
// are addressable by the model in the next turn — a swapped pairing would
// silently corrupt every multi-tool turn.
func TestParallelDispatch_OutOfOrderResolution_PreservesCallOrder(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	config := configWithMaxParallel(2)

	calls := []types.ToolCall{
		{ID: "tc_first", Name: "async_echo", Input: json.RawMessage(`{}`)},
		{ID: "tc_second", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	// Resolve the second call ~50ms before the first.
	go fireResponseWhenEmitted(t, tr, calls[1].ID, 0, "payload-for-second")
	go fireResponseWhenEmitted(t, tr, calls[0].ID, 50*time.Millisecond, "payload-for-first")

	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ToolUseID != calls[0].ID {
		t.Errorf("result[0].ToolUseID = %q, want %q", results[0].ToolUseID, calls[0].ID)
	}
	if results[1].ToolUseID != calls[1].ID {
		t.Errorf("result[1].ToolUseID = %q, want %q", results[1].ToolUseID, calls[1].ID)
	}
	if results[0].Content != "payload-for-first" {
		t.Errorf("result[0].Content = %q, want %q", results[0].Content, "payload-for-first")
	}
	if results[1].Content != "payload-for-second" {
		t.Errorf("result[1].Content = %q, want %q", results[1].Content, "payload-for-second")
	}
}

// TestParallelDispatch_MaxParallelOne_Serialises confirms the semaphore
// caps concurrency at MaxParallel=1: no two handlers may be in flight at
// the same time. The check walks through observed entry/exit timestamps
// and asserts they do not overlap.
func TestParallelDispatch_MaxParallelOne_Serialises(t *testing.T) {
	tr := newAsyncTestTransport()

	type span struct{ enter, exit time.Time }
	var (
		mu    sync.Mutex
		spans []span
	)

	asyncTool := &tool.Tool{
		Name:        "async_serial",
		Description: "async tool that records entry/exit timestamps",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			// "exit" is recorded in the response-firing goroutine right
			// before FireControl, so [enter, exit] reflects true occupancy.
			mu.Lock()
			spans = append(spans, span{enter: time.Now()})
			mu.Unlock()
			return tool.AsyncDispatch{}, nil
		},
	}

	loop := buildParallelDispatchLoop(t, tr, asyncTool)
	config := configWithMaxParallel(1)

	const n = 3
	calls := make([]types.ToolCall, n)
	for i := 0; i < n; i++ {
		calls[i] = types.ToolCall{
			ID:    fmt.Sprintf("tc_serial_%d", i),
			Name:  "async_serial",
			Input: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}
	}

	for i := 0; i < n; i++ {
		i := i
		go func() {
			requestID := waitForRequestID(t, tr, calls[i].ID, 2*time.Second)
			time.Sleep(80 * time.Millisecond)
			mu.Lock()
			spans[i].exit = time.Now()
			mu.Unlock()
			tr.FireControl(types.ControlEvent{
				Type:      "tool_result_response",
				RequestID: requestID,
				Content:   fmt.Sprintf("done-%d", i),
			})
		}()
	}

	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(spans) != n {
		t.Fatalf("expected %d recorded handler entries, got %d", n, len(spans))
	}
	for i := 1; i < n; i++ {
		if !spans[i].enter.After(spans[i-1].exit) {
			t.Errorf("handler %d entered at %s before handler %d exited at %s; semaphore did not serialise",
				i, spans[i].enter, i-1, spans[i-1].exit)
		}
	}
}

// TestParallelDispatch_CtxCancellation_DrainsInFlight covers the ctx-cancel
// path: planAndDispatch must return promptly when the parent ctx is
// cancelled, with every in-flight call marked as an error. The wall-time
// bound is well under DefaultAsyncToolTimeout (60s) so a hang here is
// instantly obvious. Wired with an in-memory OTel exporter to match the
// tracer setup TestParallelDispatch_OTelSpans_AreSiblingsUnderAgent uses.
func TestParallelDispatch_CtxCancellation_DrainsInFlight(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Tracer = tp.Tracer("test")
	// TraceContext (the OTel emitter's root) derives from
	// context.Background() and is NOT cancelled by the run ctx; span
	// parentage is separate from goroutine ctx, so cancel must still drain.
	loop.TraceContext = context.Background()
	config := configWithMaxParallel(3)

	const n = 3
	calls := make([]types.ToolCall, n)
	for i := 0; i < n; i++ {
		calls[i] = types.ToolCall{
			ID:    fmt.Sprintf("tc_cancel_%d", i),
			Name:  "async_echo",
			Input: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// A dedicated goroutine (rather than a fixed sleep) avoids racing the
	// loop's emission loop.
	go func() {
		for _, c := range calls {
			waitForRequestID(t, tr, c.ID, 2*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	results, _, outcome := loop.planAndDispatch(ctx, config, calls, &stallDetector{}, "", "")
	elapsed := time.Since(start)

	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel should drain in-flight quickly, took %s", elapsed)
	}
	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}
	for i, r := range results {
		if !r.IsError {
			t.Errorf("result %d: expected IsError=true under cancel, got false; content=%q", i, r.Content)
		}
		if !strings.Contains(r.Content, "cancelled") {
			t.Errorf("result %d: expected content to mention 'cancelled', got %q", i, r.Content)
		}
	}
	// Every call must have emitted a tool_result_request before the cancel
	// landed — i.e. the dispatch did not skip any goroutines.
	var reqCount int
	for _, e := range tr.Events() {
		if e.Type == "tool_result_request" {
			reqCount++
		}
	}
	if reqCount != n {
		t.Errorf("expected %d tool_result_request emissions, got %d", n, reqCount)
	}
}

// TestParallelDispatch_MixedSyncAndAsync_DispatchesSyncInline verifies the
// Phase-1 invariant that sync calls run inline in call order BEFORE any
// async goroutine starts. This is load-bearing for ordering-sensitive
// tools whose side effects must precede async fan-out (e.g. a sync tool
// that prepares a workspace consulted by subsequent async spawn_agent
// calls).
func TestParallelDispatch_MixedSyncAndAsync_DispatchesSyncInline(t *testing.T) {
	tr := newAsyncTestTransport()

	type evt struct {
		kind string // "sync" or "async"
		t    time.Time
	}
	var (
		mu     sync.Mutex
		events []evt
	)
	record := func(kind string) {
		mu.Lock()
		events = append(events, evt{kind: kind, t: time.Now()})
		mu.Unlock()
	}

	syncTool := &tool.Tool{
		Name:        "sync_tool",
		Description: "sync tool that records entry time",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			record("sync")
			return "sync-ok", nil
		},
	}
	asyncTool := &tool.Tool{
		Name:        "async_tool",
		Description: "async tool that records entry time",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			record("async")
			return tool.AsyncDispatch{}, nil
		},
	}

	loop := buildParallelDispatchLoop(t, tr, syncTool, asyncTool)
	config := configWithMaxParallel(4)

	calls := []types.ToolCall{
		{ID: "tc_s0", Name: "sync_tool", Input: json.RawMessage(`{}`)},
		{ID: "tc_a0", Name: "async_tool", Input: json.RawMessage(`{"k":0}`)},
		{ID: "tc_s1", Name: "sync_tool", Input: json.RawMessage(`{}`)},
		{ID: "tc_a1", Name: "async_tool", Input: json.RawMessage(`{"k":1}`)},
	}

	go fireResponseWhenEmitted(t, tr, calls[1].ID, 0, "async-result-0")
	go fireResponseWhenEmitted(t, tr, calls[3].ID, 0, "async-result-1")

	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != len(calls) {
		t.Fatalf("expected %d results, got %d", len(calls), len(results))
	}

	expectedContents := []string{"sync-ok", "async-result-0", "sync-ok", "async-result-1"}
	for i, r := range results {
		if r.IsError {
			t.Errorf("result %d unexpectedly errored: %q", i, r.Content)
		}
		if r.ToolUseID != calls[i].ID {
			t.Errorf("result %d: ToolUseID=%q want %q", i, r.ToolUseID, calls[i].ID)
		}
		if r.Content != expectedContents[i] {
			t.Errorf("result %d: content=%q want %q", i, r.Content, expectedContents[i])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("expected 4 recorded handler entries, got %d", len(events))
	}
	// First two entries must be the two sync handlers (in call order), and
	// every sync timestamp must precede every async timestamp.
	var lastSync, firstAsync time.Time
	syncCount := 0
	for _, e := range events {
		switch e.kind {
		case "sync":
			syncCount++
			lastSync = e.t
		case "async":
			if firstAsync.IsZero() {
				firstAsync = e.t
			}
		}
	}
	if syncCount != 2 {
		t.Fatalf("expected 2 sync handler entries, got %d", syncCount)
	}
	if firstAsync.Before(lastSync) {
		t.Errorf("first async handler entered at %s, before last sync handler at %s; sync must run inline first",
			firstAsync, lastSync)
	}
}

// TestParallelDispatch_PanicInOneHandler_DoesNotStopOthers verifies the
// per-goroutine panic recovery in Phase 2: a panicking AsyncHandler must
// not cancel siblings and must surface as a structured tool failure on
// only the affected index. The stable "panic" substring in the recovered
// message lets the model (and this test) detect the failure mode.
func TestParallelDispatch_PanicInOneHandler_DoesNotStopOthers(t *testing.T) {
	tr := newAsyncTestTransport()

	panicTool := &tool.Tool{
		Name:        "async_panic",
		Description: "async tool whose handler panics",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			panic("synthetic panic for test")
		},
	}
	goodTool := asyncEchoTool() // resolves normally via the test transport

	loop := buildParallelDispatchLoop(t, tr, panicTool, goodTool)
	config := configWithMaxParallel(2)

	calls := []types.ToolCall{
		{ID: "tc_panic", Name: "async_panic", Input: json.RawMessage(`{}`)},
		{ID: "tc_good", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	go fireResponseWhenEmitted(t, tr, calls[1].ID, 20*time.Millisecond, "good-result")

	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Index 0: panic. Must be marked as error and carry "panic" in content.
	if !results[0].IsError {
		t.Errorf("results[0].IsError = false, expected true on panic; content=%q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "panic") {
		t.Errorf("results[0].Content = %q, expected substring 'panic'", results[0].Content)
	}
	if results[0].ToolUseID != calls[0].ID {
		t.Errorf("results[0].ToolUseID = %q, want %q", results[0].ToolUseID, calls[0].ID)
	}

	// Index 1: successful resolution must be unaffected.
	if results[1].IsError {
		t.Errorf("results[1].IsError = true; sibling should be unaffected. content=%q", results[1].Content)
	}
	if results[1].Content != "good-result" {
		t.Errorf("results[1].Content = %q, want %q", results[1].Content, "good-result")
	}
	if results[1].ToolUseID != calls[1].ID {
		t.Errorf("results[1].ToolUseID = %q, want %q", results[1].ToolUseID, calls[1].ID)
	}
}

// TestParallelDispatch_StallDetector_ObservesCallOrder verifies the stall
// detector sees recordToolCall invocations in original call order even
// when async resolution happens out of order. Dispatches two identical
// calls plus one different call, resolved out of order (middle, last,
// first) to maximise the chance of an order-dependent bug surfacing.
func TestParallelDispatch_StallDetector_ObservesCallOrder(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool(),
		&tool.Tool{
			Name:        "async_other",
			Description: "second async tool for stall ordering test",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
				return tool.AsyncDispatch{}, nil
			},
		},
	)
	config := configWithMaxParallel(3)

	sameInput := json.RawMessage(`{"k":"v"}`)
	calls := []types.ToolCall{
		{ID: "tc_id_0", Name: "async_echo", Input: sameInput},
		{ID: "tc_id_1", Name: "async_echo", Input: sameInput},
		{ID: "tc_diff", Name: "async_other", Input: json.RawMessage(`{"different":true}`)},
	}

	// Resolve out of order: middle, last, first.
	go fireResponseWhenEmitted(t, tr, calls[1].ID, 0, "r1")
	go fireResponseWhenEmitted(t, tr, calls[2].ID, 30*time.Millisecond, "r2")
	go fireResponseWhenEmitted(t, tr, calls[0].ID, 60*time.Millisecond, "r0")

	stall := &stallDetector{}
	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, stall, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome %q: two identical calls + one different should not trip threshold=3", outcome)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	if len(stall.window) != len(calls) {
		t.Fatalf("stall window holds %d signatures, want %d", len(stall.window), len(calls))
	}
	for i, call := range calls {
		want := stallSignatureOf(call.Name, call.Input, results[i].Content, !results[i].IsError)
		if stall.window[i] != want {
			t.Errorf("stall window[%d] does not match call %s (records must run in call order, not resolution order)", i, call.ID)
		}
	}
}

// TestParallelDispatch_OTelSpans_AreSiblingsUnderAgent is the OTel shape
// regression: concurrent async dispatch must create execute_tool spans as
// SIBLINGS of one another (both children of the caller's trace context),
// not nested. Nesting would mean a later span's "duration" appears to
// include earlier siblings, breaking dashboards that aggregate tool
// durations.
func TestParallelDispatch_OTelSpans_AreSiblingsUnderAgent(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Tracer = tp.Tracer("test")
	config := configWithMaxParallel(2)

	// Open a synthetic agent span as the parent for the two execute_tool
	// spans. planAndDispatch reads its parent via l.traceCtx(ctx), so the
	// agent ctx is threaded through both ctx and TraceContext as the real
	// loop does with its run span.
	agentCtx, agentSpan := loop.Tracer.Start(context.Background(), "invoke_agent test")
	loop.TraceContext = agentCtx

	calls := []types.ToolCall{
		{ID: "tc_otel_0", Name: "async_echo", Input: json.RawMessage(`{}`)},
		{ID: "tc_otel_1", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	go fireResponseWhenEmitted(t, tr, calls[0].ID, 50*time.Millisecond, "r0")
	go fireResponseWhenEmitted(t, tr, calls[1].ID, 50*time.Millisecond, "r1")

	results, _, outcome := loop.planAndDispatch(agentCtx, config, calls, &stallDetector{}, "", "")
	agentSpan.End()
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	spans := exporter.GetSpans()
	var toolSpans []tracetest.SpanStub
	var agentStub tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "execute_tool async_echo":
			toolSpans = append(toolSpans, s)
		case "invoke_agent test":
			agentStub = s
		}
	}
	if len(toolSpans) != 2 {
		t.Fatalf("expected 2 execute_tool async_echo spans, got %d (spans=%v)", len(toolSpans), spanNames(spans))
	}
	if agentStub.Name == "" {
		t.Fatal("invoke_agent test span not found in exporter")
	}
	agentSpanID := agentStub.SpanContext.SpanID()
	for i, s := range toolSpans {
		if s.Parent.SpanID() != agentSpanID {
			t.Errorf("tool span %d parent=%s, want agent span %s (siblings under the agent span, not nested)",
				i, s.Parent.SpanID(), agentSpanID)
		}
	}
	// Cross-check: the two tool spans must not be parented under each other.
	if toolSpans[0].Parent.SpanID() == toolSpans[1].SpanContext.SpanID() ||
		toolSpans[1].Parent.SpanID() == toolSpans[0].SpanContext.SpanID() {
		t.Error("tool spans are nested under each other; expected siblings")
	}
}

// TestParallelDispatch_UnknownTool_SpanNameSentinel pins the span-name
// cardinality bound: an unknown (unresolved) tool must open its OTel span
// as the bare "execute_tool" rather than "execute_tool <model-controlled
// name>", so a hostile or buggy provider response cannot pollute trace
// storage with arbitrary span names. The model-supplied name is still
// preserved verbatim in the gen_ai.tool.name attribute for debuggability.
func TestParallelDispatch_UnknownTool_SpanNameSentinel(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	// Register no tools, so every call resolves to nil (unknown).
	loop := buildParallelDispatchLoop(t, tr)
	loop.Tracer = tp.Tracer("test")
	config := configWithMaxParallel(2)

	const hostileName = "../../etc/passwd; DROP TABLE spans;--"
	calls := []types.ToolCall{
		{ID: "tc_unknown_0", Name: hostileName, Input: json.RawMessage(`{}`)},
	}

	// An unknown tool fails fast inline (no transport round-trip), so no
	// FireControl is needed; planAndDispatch returns synchronously.
	results, _, _ := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{}, "", "")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	spans := exporter.GetSpans()
	var toolSpan *tracetest.SpanStub
	for i := range spans {
		if strings.HasPrefix(spans[i].Name, "execute_tool") {
			toolSpan = &spans[i]
			break
		}
	}
	if toolSpan == nil {
		t.Fatalf("no execute_tool span found (spans=%v)", spanNames(spans))
	}
	if toolSpan.Name != "execute_tool" {
		t.Errorf("unknown-tool span name = %q, want %q (model-controlled name must stay out of the span name)",
			toolSpan.Name, "execute_tool")
	}
	// The raw model-supplied name must still be recorded on the attribute.
	var sawName bool
	for _, attr := range toolSpan.Attributes {
		if string(attr.Key) == "gen_ai.tool.name" {
			sawName = true
			if attr.Value.AsString() != hostileName {
				t.Errorf("gen_ai.tool.name attribute = %q, want raw model name %q",
					attr.Value.AsString(), hostileName)
			}
		}
	}
	if !sawName {
		t.Error("gen_ai.tool.name attribute missing from unknown-tool span")
	}
}

func spanNames(spans []tracetest.SpanStub) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

// TestParallelDispatch_StallTrips_ClosesRemainingSpans pins the Phase-3
// stall-tail span-close loop in planAndDispatch: when stall trips at
// index i mid-iteration, every plan[j].span for j > i must have End()
// called so OTel spans for already-dispatched async calls beyond the
// stall point do not leak. With the nudge already spent, four identical
// calls with MaxParallel=4 trip the stall (threshold 3) at index 2,
// leaving index 3's span for the cleanup loop to close.
func TestParallelDispatch_StallTrips_ClosesRemainingSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Tracer = tp.Tracer("test")
	config := configWithMaxParallel(4)

	const n = 4
	sameInput := json.RawMessage(`{"k":"v"}`)
	calls := make([]types.ToolCall, n)
	for i := 0; i < n; i++ {
		calls[i] = types.ToolCall{
			ID:    fmt.Sprintf("tc_stall_%d", i),
			Name:  "async_echo",
			Input: sameInput,
		}
	}

	for i := 0; i < n; i++ {
		go fireResponseWhenEmitted(t, tr, calls[i].ID, 20*time.Millisecond, "result")
	}

	results, _, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{nudged: true}, "", "")
	if outcome != "stalled" {
		t.Fatalf("expected outcome 'stalled', got %q", outcome)
	}
	// Phase 3 returns toolResults[:i+1] on stall; only the first three
	// indices were observed by the trace/transport/stall pipeline.
	if len(results) != 3 {
		t.Fatalf("expected 3 truncated results, got %d", len(results))
	}

	spans := exporter.GetSpans()
	var toolSpans []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "execute_tool async_echo" {
			toolSpans = append(toolSpans, s)
		}
	}
	if len(toolSpans) != n {
		t.Fatalf("expected %d execute_tool async_echo spans (one per dispatched call), got %d (names=%v)",
			n, len(toolSpans), spanNames(spans))
	}
	// Every span must be closed: tracetest.SpanStub only appears in
	// GetSpans() after End() has fired, but defensively assert
	// EndTime is set to catch any future regression where exporter
	// semantics shift.
	for i, s := range toolSpans {
		if s.EndTime.IsZero() {
			t.Errorf("tool span %d has zero EndTime — stall-tail cleanup did not close it", i)
		}
	}
}

// A call past the stall point has already run, so its span keeps the
// call's failure even though its trace and metrics are dropped.
func TestParallelDispatch_StallTrips_RemainingSpansKeepTheirOutcome(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	loop := buildParallelDispatchLoop(t, newAsyncTestTransport(), sleepingSyncTool("sync_tool", 0))
	loop.Tracer = tp.Tracer("test")
	sameInput := json.RawMessage(`{"k":"v"}`)
	stall := &stallDetector{nudged: true}
	calls := []types.ToolCall{
		{ID: "tc_1", Name: "sync_tool", Input: sameInput},
		{ID: "tc_2", Name: "sync_tool", Input: sameInput},
		{ID: "tc_stall", Name: "sync_tool", Input: sameInput},
		{ID: "tc_after", Name: "no_such_tool", Input: json.RawMessage(`{}`)},
	}

	_, _, outcome := loop.planAndDispatch(context.Background(), configWithMaxParallel(1), calls, stall, "", "")
	if outcome != "stalled" {
		t.Fatalf("expected outcome 'stalled', got %q", outcome)
	}
	var after []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name == "execute_tool" {
			after = append(after, s)
		}
	}
	if len(after) != 1 {
		t.Fatalf("found %d spans for the call past the stall point, want 1", len(after))
	}
	if after[0].Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", after[0].Status.Code)
	}
	var errorType string
	for _, kv := range after[0].Attributes {
		if kv.Key == "error.type" {
			errorType = kv.Value.AsString()
		}
	}
	if errorType != observability.ToolFailureUnknownTool.String() {
		t.Errorf("error.type = %q, want %q", errorType, observability.ToolFailureUnknownTool)
	}
}

// TestParallelDispatch_StallNudge_KeepsWholeBatch pins that a first
// repetition inside a batch does not truncate it: every call's result
// and record is returned, paired with its call, and the nudge waits for
// the caller.
func TestParallelDispatch_StallNudge_KeepsWholeBatch(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	config := configWithMaxParallel(4)

	const n = 5
	sameInput := json.RawMessage(`{"k":"v"}`)
	calls := make([]types.ToolCall, n)
	for i := 0; i < n; i++ {
		calls[i] = types.ToolCall{ID: fmt.Sprintf("tc_nudge_%d", i), Name: "async_echo", Input: sameInput}
		go fireResponseWhenEmitted(t, tr, calls[i].ID, 10*time.Millisecond, "same")
	}

	stall := &stallDetector{}
	results, records, outcome := loop.planAndDispatch(context.Background(), config, calls, stall, "", "")
	if outcome != "" {
		t.Fatalf("outcome = %q, want none: the first repetition nudges instead of ending the run", outcome)
	}
	if len(results) != n || len(records) != n {
		t.Fatalf("got %d results and %d records, want %d of each", len(results), len(records), n)
	}
	for i, call := range calls {
		if results[i].ToolUseID != call.ID || records[i].ID != call.ID {
			t.Errorf("index %d: result %q, record %q; want both paired with %q", i, results[i].ToolUseID, records[i].ID, call.ID)
		}
	}
	var toolResultEvents int
	for _, ev := range tr.Events() {
		if ev.Type == "tool_result" {
			toolResultEvents++
		}
	}
	if toolResultEvents != n {
		t.Errorf("tool_result events = %d, want %d", toolResultEvents, n)
	}
	if _, ok := stall.takeNudge(); !ok {
		t.Error("no pending nudge after the batch")
	}
}

// TestParallelDispatch_GuardDenyReason_ScrubbedBeforeTrace pins the
// invariant that the PhasePreTool guard's adversary-influenceable Reason
// string is scrubbed by security.Scrub before it lands on the trace
// emitter's ToolCallTrace.ErrorReason field. Without the scrub, a
// classifier model that echoes secret-shaped fragments from an attacker's
// tool input back as its denial reason would leak those fragments into
// JSONL trace files on disk and any OTLP sink.
func TestParallelDispatch_GuardDenyReason_ScrubbedBeforeTrace(t *testing.T) {
	// Matches the anthropic_api_key pattern (security/logscrubber.go), so
	// the literal "FAKE_SECRET_SENTINEL" survives only if Scrub was NOT
	// applied.
	const sentinel = "sk-ant-FAKE_SECRET_SENTINEL"

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.GuardRail = &stubDenyGuardRail{reason: "blocked because of " + sentinel}
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	calls := []types.ToolCall{
		{ID: "tc_deny", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	results, _, outcome := loop.planAndDispatch(context.Background(),
		configWithMaxParallel(1), calls, &stallDetector{}, "", "")
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Errorf("expected guard-deny to produce IsError, got success; content=%q", results[0].Content)
	}

	_, recordedCalls := rec.snapshot()
	if len(recordedCalls) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(recordedCalls))
	}
	if recordedCalls[0].ErrorReason == "" {
		t.Fatal("expected ErrorReason to be populated on guard deny")
	}
	if strings.Contains(recordedCalls[0].ErrorReason, "FAKE_SECRET_SENTINEL") {
		t.Errorf("ErrorReason leaked secret-shaped fragment: %q", recordedCalls[0].ErrorReason)
	}
	if !strings.Contains(recordedCalls[0].ErrorReason, "[REDACTED]") {
		t.Errorf("ErrorReason did not show evidence of scrubbing (expected [REDACTED]): %q", recordedCalls[0].ErrorReason)
	}
}

// TestParallelDispatch_ToolCallEventsPrecedeAsyncToolResults drives a full
// AgenticLoop.Run() with three calls against an AsyncHandler-backed tool,
// so Phase 2's goroutine fan-out genuinely runs (dispatch.go's
// asyncIndices path) — unlike TestLoop_ToolCallEventsPrecedeMatchingToolResults
// in loop_test.go, whose synchronous test_tool never leaves Phase 1. This
// is the regression test for issue #593's ordering guarantee under real
// concurrent dispatch, not just the inline path.
func TestParallelDispatch_ToolCallEventsPrecedeAsyncToolResults(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Provider = &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "async_echo", Input: map[string]any{"n": 1}},
				{Type: "tool_call", ID: "tc_2", Name: "async_echo", Input: map[string]any{"n": 2}},
				{Type: "tool_call", ID: "tc_3", Name: "async_echo", Input: map[string]any{"n": 3}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	config := buildTestConfig()
	config.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: 3}

	ids := []string{"tc_1", "tc_2", "tc_3"}
	for _, id := range ids {
		id := id
		go fireResponseWhenEmitted(t, tr, id, 20*time.Millisecond, "result-for-"+id)
	}

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	events := tr.Events()

	requestIdx := map[string]int{}
	toolCallIdx := map[string]int{}
	toolResultIdx := map[string]int{}
	for i, e := range events {
		switch e.Type {
		case "tool_call":
			toolCallIdx[e.ID] = i
		case "tool_result_request":
			requestIdx[e.ToolUseID] = i
		case "tool_result":
			toolResultIdx[e.ToolUseID] = i
		}
	}

	for _, id := range ids {
		// tool_result_request only fires from dispatch.go's AsyncHandler
		// path (dispatchAsyncToolCall), so its presence is the proof this
		// test exercised Phase 2's fan-out rather than Phase 1's inline
		// dispatch.
		if _, ok := requestIdx[id]; !ok {
			t.Fatalf("missing tool_result_request for %q; async fan-out did not run", id)
		}
		callIdx, ok := toolCallIdx[id]
		if !ok {
			t.Fatalf("missing tool_call event for %q", id)
		}
		resultIdx, ok := toolResultIdx[id]
		if !ok {
			t.Fatalf("missing tool_result event for %q", id)
		}
		if callIdx >= resultIdx {
			t.Errorf("tool_call for %q at index %d did not precede its tool_result at index %d", id, callIdx, resultIdx)
		}
	}
}

// mcpTraceServer is an MCP server whose single "echo" tool returns its "id"
// argument. It records the traceparent each tools/call carried, both as the
// HTTP header and in params._meta, keyed by that id.
func mcpTraceServer(t *testing.T) (*httptest.Server, func() map[string][2]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string][2]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					ID string `json:"id"`
				} `json:"arguments"`
				Meta map[string]string `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result string
		switch req.Method {
		case "tools/list":
			result = `{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			mu.Lock()
			seen[req.Params.Arguments.ID] = [2]string{r.Header.Get("traceparent"), req.Params.Meta["traceparent"]}
			mu.Unlock()
			result = fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, req.Params.Arguments.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string][2]string {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string][2]string, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func spanIDFromTraceparent(t *testing.T, traceparent string) oteltrace.SpanContext {
	t.Helper()
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent})
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatalf("traceparent %q does not decode to a valid span context", traceparent)
	}
	return sc
}

// TestDispatch_MCPCallsCarryTheirOwnToolSpan pins that each MCP call in a
// batch, dispatched inline one after another, reaches the server parented on
// its own execute_tool span through both the traceparent header and
// tools/call _meta.
func TestDispatch_MCPCallsCarryTheirOwnToolSpan(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Permissions = permission.NewAllowAll()

	srv, seen := mcpTraceServer(t)
	client := mcp.NewClient(loop.Tools.(*tool.Registry), srv.Client())
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Connect(context.Background(), types.MCPServerConfig{Name: "srv", URI: srv.URL}, nil); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_a", Name: "mcp_srv_echo", Input: map[string]any{"id": "a"}},
			{Type: "tool_call", ID: "tc_b", Name: "mcp_srv_echo", Input: map[string]any{"id": "b"}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")[0]
	toolSpanByCall := map[string]oteltrace.SpanContext{}
	for _, s := range spansNamed(spans, "execute_tool mcp_srv_echo") {
		id, _ := spanAttr(s, "gen_ai.tool.call.id")
		toolSpanByCall[id] = s.SpanContext
	}
	if len(toolSpanByCall) != 2 {
		t.Fatalf("found execute_tool spans for calls %v, want tc_a and tc_b", toolSpanByCall)
	}

	got := seen()
	for arg, callID := range map[string]string{"a": "tc_a", "b": "tc_b"} {
		want, ok := toolSpanByCall[callID]
		if !ok {
			t.Fatalf("no execute_tool span for %s", callID)
		}
		carried, ok := got[arg]
		if !ok {
			t.Fatalf("server saw no tools/call for %q", arg)
		}
		for i, where := range []string{"traceparent header", "_meta.traceparent"} {
			sc := spanIDFromTraceparent(t, carried[i])
			if sc.SpanID() != want.SpanID() {
				t.Errorf("call %s %s span = %s, want its execute_tool span %s", callID, where, sc.SpanID(), want.SpanID())
			}
			if sc.TraceID() != root.SpanContext.TraceID() {
				t.Errorf("call %s %s trace = %s, want the run trace %s", callID, where, sc.TraceID(), root.SpanContext.TraceID())
			}
		}
	}
	if toolSpanByCall["tc_a"].SpanID() == toolSpanByCall["tc_b"].SpanID() {
		t.Fatal("both MCP calls ran under the same execute_tool span")
	}
}
