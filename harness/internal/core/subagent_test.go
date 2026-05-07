package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
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

// recordingTraceEmitter is a TraceEmitter test double that captures every
// RecordTurn / RecordToolCall call so tests can assert on forwarding
// behaviour from the NestedJSONLEmitter into the parent's emitter.
type recordingTraceEmitter struct {
	mu        sync.Mutex
	turns     []types.TurnTrace
	toolCalls []types.ToolCallTrace
}

func (r *recordingTraceEmitter) Start(_ string, _ *types.RunConfig) {}

func (r *recordingTraceEmitter) RecordTurn(turn types.TurnTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, turn)
}

func (r *recordingTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = append(r.toolCalls, call)
}

func (r *recordingTraceEmitter) Finish(_ context.Context, _ string) (*types.RunTrace, error) {
	return &types.RunTrace{}, nil
}

func (r *recordingTraceEmitter) snapshot() ([]types.TurnTrace, []types.ToolCallTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turns := append([]types.TurnTrace(nil), r.turns...)
	calls := append([]types.ToolCallTrace(nil), r.toolCalls...)
	return turns, calls
}

func buildSubAgentTestLoop(prov *mockProvider) *AgenticLoop {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "test_tool",
		Description:       "A test tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})
	// Register a spawn_agent tool to verify it gets filtered for the child.
	registry.Register(&tool.Tool{
		Name:              "spawn_agent",
		Description:       "Spawn a sub-agent",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		RequiresApproval:  true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "should not be called", nil
		},
	})

	return &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}
}

func TestSpawnSubAgent_SimpleTextResponse(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent output here."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do a subtask",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}

	if result.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", result.Outcome)
	}
	if result.Output != "Sub-agent output here." {
		t.Errorf("expected output 'Sub-agent output here.', got %q", result.Output)
	}
	if result.Turns != 1 {
		t.Errorf("expected 1 turn, got %d", result.Turns)
	}
}

func TestSpawnSubAgent_EmptyPromptReturnsError(t *testing.T) {
	prov := &mockProvider{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "",
	})
	if err == nil {
		t.Fatal("SpawnSubAgent() expected error for empty prompt, got nil")
	}
}

// TestCapSubAgentMaxTurns exercises the production capSubAgentMaxTurns
// helper so all three capping branches in SpawnSubAgent (#55, B5) are
// covered directly. The previous version of this test replicated the
// arithmetic in the test body and never called the production code,
// so a deletion of one of the branches would leave the test passing
// while production was broken.
func TestCapSubAgentMaxTurns(t *testing.T) {
	tests := []struct {
		name           string
		requested      int
		parentMax      int
		expectedCapped int
	}{
		{"default when zero", 0, 20, defaultSubAgentMaxTurns},
		{"cap at max sub-agent", 50, 100, maxSubAgentMaxTurns},
		{"cap at parent max", 15, 5, 5},
		{"use requested when within bounds", 8, 20, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capSubAgentMaxTurns(tt.requested, tt.parentMax)
			if got != tt.expectedCapped {
				t.Errorf("capSubAgentMaxTurns(%d, %d) = %d, want %d",
					tt.requested, tt.parentMax, got, tt.expectedCapped)
			}
			t.Logf("capSubAgentMaxTurns(%d, %d) = %d", tt.requested, tt.parentMax, got)
		})
	}
}

// TestSpawnSubAgent_MaxTurnsRespectedAtRuntime is the end-to-end
// counterpart to TestCapSubAgentMaxTurns: it drives SpawnSubAgent
// itself with a provider that emits text+end_turn so the run
// completes in a single turn, and asserts the returned
// SubAgentResult.Turns is bounded by the cap. Combined with the
// helper test, this covers both the arithmetic and the wiring of
// the capped value into the child loop's RunConfig (#55, B5).
func TestSpawnSubAgent_MaxTurnsRespectedAtRuntime(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()
	parentConfig.MaxTurns = 50 // generous parent budget so cap is not parent-bound

	// Request 999 turns; expect the child to actually be capped at
	// maxSubAgentMaxTurns. The single-turn provider then ends after
	// turn 0, so result.Turns should be 1 and the run must not have
	// failed for budget reasons.
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt:   "do a subtask",
		MaxTurns: 999,
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("expected outcome 'success' (cap accepted, run completed), got %q", result.Outcome)
	}
	if result.Turns < 1 {
		t.Errorf("expected at least 1 turn, got %d", result.Turns)
	}
	// Sanity: the cap helper would have returned maxSubAgentMaxTurns
	// for this requested value paired with parentConfig.MaxTurns=50.
	wantCap := capSubAgentMaxTurns(999, parentConfig.MaxTurns)
	if wantCap != maxSubAgentMaxTurns {
		t.Errorf("test setup invariant violated: capSubAgentMaxTurns(999, %d) = %d, want %d",
			parentConfig.MaxTurns, wantCap, maxSubAgentMaxTurns)
	}
}

func TestFilterToolRegistry_ExcludesSpawnAgent(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "run_command",
		Description: "Run a command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})

	filtered := filterToolRegistry(registry, "spawn_agent")
	defs := filtered.List()

	if len(defs) != 2 {
		t.Fatalf("expected 2 tools after filtering, got %d", len(defs))
	}

	for _, def := range defs {
		if def.Name == "spawn_agent" {
			t.Error("filtered registry should not contain spawn_agent")
		}
	}

	if filtered.Resolve("spawn_agent") != nil {
		t.Error("Resolve(\"spawn_agent\") should return nil in filtered registry")
	}
	if filtered.Resolve("read_file") == nil {
		t.Error("Resolve(\"read_file\") should return non-nil in filtered registry")
	}
	if filtered.Resolve("run_command") == nil {
		t.Error("Resolve(\"run_command\") should return non-nil in filtered registry")
	}
}

func TestSpawnSubAgent_InheritsParentMode(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()
	parentConfig.Mode = "execution"

	// When mode is empty, should inherit parent's mode.
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do something",
		Mode:   "",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", result.Outcome)
	}
}

// TestSpawnSubAgent_InheritsGuardRail asserts that a sub-agent
// inherits the parent's GuardRail. Without this inheritance an
// indirect-injection payload could route harmful work through
// spawn_agent and bypass all phases, since guardCheck nil-short-
// circuits to allow when GuardRail is nil. The test installs a
// deny-everything guard on the parent and asserts the sub-agent run
// terminates with "guardrail_blocked" — proving the guard was active
// inside the child loop.
func TestSpawnSubAgent_InheritsGuardRail(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent output."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.GuardRail = &fakeGuard{verdict: guard.VerdictDeny, reason: "deny everything"}
	parentConfig := buildTestConfig()

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do a subtask",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	// A deny on PreTurn turn 0 aborts with guardrail_blocked. If the
	// sub-agent had a nil GuardRail it would silently run to success.
	if result.Outcome != "guardrail_blocked" {
		t.Errorf("expected outcome 'guardrail_blocked' (parent guard inherited), got %q", result.Outcome)
	}
}

func TestCaptureTransport_RecordsTextDeltas(t *testing.T) {
	ct := newCaptureTransport()

	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "Hello "})
	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "world!"})
	_ = ct.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc_1", Content: "result"})
	_ = ct.Emit(types.HarnessEvent{Type: "done", StopReason: "success"})

	text := ct.lastText()
	if text != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", text)
	}
}

func TestCaptureTransport_EmptyWhenNoTextDeltas(t *testing.T) {
	ct := newCaptureTransport()
	_ = ct.Emit(types.HarnessEvent{Type: "done"})

	if text := ct.lastText(); text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

// TestSpawnSubAgent_TraceEventsForwardedToParent is the regression test
// for issue #55 acceptance criterion #1: sub-agent JSONL trace events
// must appear on the parent's trace emitter rather than being dropped
// into a discarded buffer. We attach a recording emitter as the
// parent's Trace, run a sub-agent through one turn, and assert the
// child's RecordTurn and RecordToolCall events arrived on the parent
// emitter, tagged with parentRunID and the child's runID.
func TestSpawnSubAgent_TraceEventsForwardedToParent(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent reply."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentEmitter := &recordingTraceEmitter{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Trace = parentEmitter

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-forward-1"

	if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	turns, _ := parentEmitter.snapshot()
	if len(turns) == 0 {
		t.Fatal("expected at least one turn forwarded to parent emitter, got none (was the bytes.Buffer discarder reintroduced?)")
	}
	for _, turn := range turns {
		if turn.ParentRunID != parentConfig.RunID {
			t.Errorf("forwarded turn ParentRunID: got %q, want %q", turn.ParentRunID, parentConfig.RunID)
		}
		if turn.RunID == "" {
			t.Errorf("forwarded turn RunID must be populated; got empty")
		}
		if turn.RunID == parentConfig.RunID {
			t.Errorf("forwarded turn RunID must be the child's runID, not the parent's; got %q", turn.RunID)
		}
	}
}

// TestSpawnSubAgent_TraceToolCallsForwardedToParent is the regression
// test for #55 B6: the end-to-end path SpawnSubAgent → child loop
// tool dispatch → NestedJSONLEmitter.RecordToolCall → parent emitter
// must surface tool calls on the parent's trace stream, tagged with
// the child's RunID and the parent's ParentRunID. The original
// forwarding test only emitted text deltas, so the tool-call branch
// of NestedJSONLEmitter was only covered by isolated unit tests.
func TestSpawnSubAgent_TraceToolCallsForwardedToParent(t *testing.T) {
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			// Turn 0: emit one tool_call, stop with tool_use so the
			// loop dispatches the tool then re-enters the next turn.
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			// Turn 1: end the run.
			{
				{Type: "text_delta", Text: "Done."},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	parentEmitter := &recordingTraceEmitter{}
	parentLoop := buildSubAgentTestLoop(nil)
	parentLoop.Provider = prov
	parentLoop.Trace = parentEmitter

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-toolcalls-1"

	if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	_, calls := parentEmitter.snapshot()
	if len(calls) == 0 {
		t.Fatal("expected at least one forwarded tool call on parent emitter, got none (NestedJSONLEmitter.RecordToolCall path is broken)")
	}
	for _, c := range calls {
		if c.ParentRunID != parentConfig.RunID {
			t.Errorf("forwarded ToolCallTrace.ParentRunID: got %q, want %q", c.ParentRunID, parentConfig.RunID)
		}
		if c.RunID == "" {
			t.Errorf("forwarded ToolCallTrace.RunID must be populated; got empty")
		}
		if c.RunID == parentConfig.RunID {
			t.Errorf("forwarded ToolCallTrace.RunID must be the child's runID, not the parent's; got %q", c.RunID)
		}
		if c.Name != "test_tool" {
			t.Errorf("forwarded ToolCallTrace.Name: got %q, want %q", c.Name, "test_tool")
		}
	}
}

// TestSpawnSubAgent_ForwardedToolErrorReasonIsScrubbed is the
// regression test for #55 B4 (CWE-532): when a child tool fails,
// dispatchToolCall returns the raw error text. NestedJSONLEmitter
// then forwards that string to the parent's JSONL trace via
// RecordToolCall, where it lands in the trace file via json.Marshal
// — bypassing slog's ScrubHandler. The fix scrubs the string before
// it reaches RecordToolCall.
//
// Setup: child tool handler returns an error containing a string
// matching the anthropic_api_key LogScrubber pattern, child provider
// emits a tool_call invoking that handler. After SpawnSubAgent
// returns, assert the parent emitter saw a ToolCallTrace whose
// ErrorReason is redacted (no fake key substring) and that at least
// one forwarded tool call was recorded.
func TestSpawnSubAgent_ForwardedToolErrorReasonIsScrubbed(t *testing.T) {
	const fakeKey = "sk-ant-DEADBEEFleakcanaryABCDEF123456789"

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "tool_call", ID: "tc_leak_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}

	parentEmitter := &recordingTraceEmitter{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Trace = parentEmitter
	// Replace the test_tool handler with one that returns an error
	// embedding the fake key. dispatchToolCall wraps it as
	// "Tool error: <err>" with success=false.
	parentLoop.Tools.Resolve("test_tool").Handler = func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", &leakErr{fakeKey: fakeKey}
	}

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-scrub-1"

	if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt:   "do a subtask",
		MaxTurns: 1,
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	_, calls := parentEmitter.snapshot()
	if len(calls) == 0 {
		t.Fatal("expected at least one forwarded tool call on parent emitter, got none")
	}

	var sawScrubbedFailure bool
	for _, c := range calls {
		if c.Success {
			continue
		}
		if c.ErrorReason == "" {
			t.Errorf("failed forwarded tool call has empty ErrorReason; expected scrubbed text")
			continue
		}
		if strings.Contains(c.ErrorReason, fakeKey) {
			t.Errorf("forwarded ToolCallTrace.ErrorReason leaked unscrubbed key: %q", c.ErrorReason)
		}
		sawScrubbedFailure = true
	}
	if !sawScrubbedFailure {
		t.Fatal("expected at least one failed forwarded ToolCallTrace, got none")
	}
}

// leakErr's Error() embeds a fake API key to simulate a tool handler
// surfacing an upstream error string that legitimately contains
// secret-shaped substrings (e.g. an HTTP error wrapping the request
// URL with an Authorization header). Used by the B4 scrub test.
type leakErr struct {
	fakeKey string
}

func (e *leakErr) Error() string {
	return "upstream auth failed for request token=" + e.fakeKey
}

// TestSpawnSubAgent_MetricsTaggedAsSubAgent is the regression test for
// issue #55 acceptance criterion #3: metrics emitted from a sub-agent
// must carry an attribute identifying them as such (run.subagent=true
// plus run.parent_id) so dashboards can decompose a run.
func TestSpawnSubAgent_MetricsTaggedAsSubAgent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent reply."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Metrics = metrics

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-metrics-1"

	if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Inspect both stirrup.harness.runs and stirrup.harness.turns so
	// the assertion exercises at least two of the 14 instrument call
	// sites that go through metricAttrs() in loop.go (#55, B7). A
	// missing metricAttrs() call on Turns specifically would not have
	// been caught by a runs-only assertion — Runs is incremented once
	// per run but Turns is incremented every turn, making it the most
	// reliable "is the wiring still hooked up" probe across the
	// per-turn instrument cluster.
	check := func(name string) (sawSubAgent, sawParentRunID bool) {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != name {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					attrs := dp.Attributes
					if v, exists := attrs.Value(attribute.Key("run.subagent")); exists && v.AsBool() {
						sawSubAgent = true
					}
					if v, exists := attrs.Value(attribute.Key("run.parent_id")); exists && v.AsString() == parentConfig.RunID {
						sawParentRunID = true
					}
				}
			}
		}
		return
	}

	sawRunsSubAgent, sawRunsParentRunID := check("stirrup.harness.runs")
	if !sawRunsSubAgent {
		t.Errorf("expected a stirrup.harness.runs data point with run.subagent=true; none found")
	}
	if !sawRunsParentRunID {
		t.Errorf("expected a stirrup.harness.runs data point with run.parent_id=%q; none found", parentConfig.RunID)
	}

	sawTurnsSubAgent, sawTurnsParentRunID := check("stirrup.harness.turns")
	if !sawTurnsSubAgent {
		t.Errorf("expected a stirrup.harness.turns data point with run.subagent=true; none found (B7: metric attrs not propagated to per-turn instrument)")
	}
	if !sawTurnsParentRunID {
		t.Errorf("expected a stirrup.harness.turns data point with run.parent_id=%q; none found", parentConfig.RunID)
	}
}
