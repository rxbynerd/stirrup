package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
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
		Trace:        tracepkg.NewJSONLTraceEmitter(&bytes.Buffer{}),
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
	results, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{})
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

	results, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{})
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
			// Record the handler-entry timestamp. The "exit" timestamp is
			// captured in the response-firing goroutine immediately before
			// the FireControl call so the [enter, exit] interval reflects
			// the handler's true occupancy of its semaphore slot.
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

	// For each call, wait for its request to land then sleep ~80ms before
	// firing the response. The sleep is the handler's "in-flight" window;
	// the exit timestamp is recorded immediately before the response is
	// fired so the recorded [enter, exit] spans match the handler's true
	// occupancy.
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

	results, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{})
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
// instantly obvious.
//
// Wired with an in-memory OTel exporter so this exercises the same OTel
// configuration as TestParallelDispatch_OTelSpans_AreSiblingsUnderTurn —
// previous iterations of this test cleared TraceContext to dodge a real
// bug (the trace-emitter root ctx severed run-level cancellation); that
// fix landed in dispatch.go so the goroutine span ctx is now always
// rooted in the cancellable run ctx.
func TestParallelDispatch_CtxCancellation_DrainsInFlight(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Tracer = tp.Tracer("test")
	// Mirror the real loop: TraceContext is the OTel emitter's root,
	// which derives from context.Background() and is therefore NOT
	// cancelled by the run ctx. The fix in dispatch.go separates span
	// parentage from goroutine ctx, so cancel must still drain.
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
	// Cancel once all three requests have been emitted. Doing this with a
	// dedicated goroutine (rather than a fixed sleep) avoids racing the
	// loop's emission loop and keeps the test robust under load.
	go func() {
		for _, c := range calls {
			waitForRequestID(t, tr, c.ID, 2*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	results, outcome := loop.planAndDispatch(ctx, config, calls, &stallDetector{})
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

	results, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{})
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

	results, outcome := loop.planAndDispatch(context.Background(), config, calls, &stallDetector{})
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

// TestParallelDispatch_StallDetector_ObservesCallOrder verifies Phase 3
// invariant: the stall detector sees recordToolCall invocations in
// original call order even when async resolution happens out of order.
// Strategy: dispatch two identical calls + one different call (sandwich
// arrangement: identical, identical, different). If the detector saw
// them in resolution order rather than call order, two identical calls
// would land consecutively in either ordering — so we additionally
// resolve the middle one first to maximise the chance of an
// order-dependent bug surfacing. With correct in-order Phase-3
// iteration, the lastToolCall after dispatch is the "different" tool
// (the last call), and repeatCount is 1 (the "different" tool broke
// the identical streak). The maxRepeatedToolCalls threshold of 3 is
// not reached, so the outcome must be "" (no stall).
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
	results, outcome := loop.planAndDispatch(context.Background(), config, calls, stall)
	if outcome != "" {
		t.Fatalf("unexpected stall outcome %q: two identical calls + one different should not trip threshold=3", outcome)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// After in-order Phase-3 iteration:
	//   call 0 recorded → lastToolCall = "async_echo:..."        repeat=1
	//   call 1 recorded → lastToolCall = "async_echo:..." (same) repeat=2
	//   call 2 recorded → lastToolCall = "async_other:..." (new) repeat=1
	expectedKey := "async_other:" + string(calls[2].Input)
	if stall.lastToolCall != expectedKey {
		t.Errorf("stall.lastToolCall = %q, want %q (records must run in call order, not resolution order)",
			stall.lastToolCall, expectedKey)
	}
	if stall.repeatCount != 1 {
		t.Errorf("stall.repeatCount = %d, want 1 (the trailing 'different' call must reset the streak)", stall.repeatCount)
	}
}

// TestParallelDispatch_OTelSpans_AreSiblingsUnderTurn is the OTel shape
// regression: concurrent async dispatch must create tool.<name> spans as
// SIBLINGS of one another (both children of the caller's trace context),
// not nested. Nesting would mean a later span's "duration" appears to
// include earlier siblings, breaking dashboards that aggregate tool
// durations.
func TestParallelDispatch_OTelSpans_AreSiblingsUnderTurn(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.Tracer = tp.Tracer("test")
	config := configWithMaxParallel(2)

	// Open a synthetic "turn" span as the parent for the two tool.<name>
	// spans. planAndDispatch reads its parent via l.traceCtx(ctx); we
	// thread the turn ctx through both ctx and TraceContext to mirror the
	// real loop's turn boundary.
	turnCtx, turnSpan := loop.Tracer.Start(context.Background(), "turn[test]")
	loop.TraceContext = turnCtx

	calls := []types.ToolCall{
		{ID: "tc_otel_0", Name: "async_echo", Input: json.RawMessage(`{}`)},
		{ID: "tc_otel_1", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	go fireResponseWhenEmitted(t, tr, calls[0].ID, 50*time.Millisecond, "r0")
	go fireResponseWhenEmitted(t, tr, calls[1].ID, 50*time.Millisecond, "r1")

	results, outcome := loop.planAndDispatch(turnCtx, config, calls, &stallDetector{})
	turnSpan.End()
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	spans := exporter.GetSpans()
	var toolSpans []tracetest.SpanStub
	var turnStub tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "tool.async_echo":
			toolSpans = append(toolSpans, s)
		case "turn[test]":
			turnStub = s
		}
	}
	if len(toolSpans) != 2 {
		t.Fatalf("expected 2 tool.async_echo spans, got %d (spans=%v)", len(toolSpans), spanNames(spans))
	}
	if turnStub.Name == "" {
		t.Fatal("turn[test] span not found in exporter")
	}
	turnSpanID := turnStub.SpanContext.SpanID()
	for i, s := range toolSpans {
		if s.Parent.SpanID() != turnSpanID {
			t.Errorf("tool span %d parent=%s, want turn span %s (siblings under turn, not nested)",
				i, s.Parent.SpanID(), turnSpanID)
		}
	}
	// Cross-check: the two tool spans must not be parented under each other.
	if toolSpans[0].Parent.SpanID() == toolSpans[1].SpanContext.SpanID() ||
		toolSpans[1].Parent.SpanID() == toolSpans[0].SpanContext.SpanID() {
		t.Error("tool spans are nested under each other; expected siblings")
	}
}

func spanNames(spans []tracetest.SpanStub) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

// TestParallelDispatch_GuardDenyReason_ScrubbedBeforeTrace pins the R-5
// invariant: the PhasePreTool guard's adversary-influenceable Reason
// string is scrubbed by security.Scrub before it lands on the trace
// emitter's ToolCallTrace.ErrorReason field. Without the scrub, a
// classifier model that echoes secret-shaped fragments from an attacker's
// tool input back as its denial reason would leak those fragments into
// JSONL trace files on disk and any OTLP sink.
func TestParallelDispatch_GuardDenyReason_ScrubbedBeforeTrace(t *testing.T) {
	// sk-ant-FAKE_SECRET_SENTINEL matches the anthropic_api_key pattern
	// (security/logscrubber.go) so Scrub will redact it. The literal
	// "FAKE_SECRET_SENTINEL" survives only if Scrub was NOT applied —
	// hence the assertion below.
	const sentinel = "sk-ant-FAKE_SECRET_SENTINEL"

	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool())
	loop.GuardRail = &stubDenyGuardRail{reason: "blocked because of " + sentinel}
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	calls := []types.ToolCall{
		{ID: "tc_deny", Name: "async_echo", Input: json.RawMessage(`{}`)},
	}

	results, outcome := loop.planAndDispatch(context.Background(),
		configWithMaxParallel(1), calls, &stallDetector{})
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
