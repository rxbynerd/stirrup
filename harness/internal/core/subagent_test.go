package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	mu                sync.Mutex
	turns             []types.TurnTrace
	turnRecords       []types.TurnRecord
	toolCalls         []types.ToolCallTrace
	permissionDenials int
}

func (r *recordingTraceEmitter) Start(_ string, _ *types.RunConfig) {}

func (r *recordingTraceEmitter) RecordTurn(turn types.TurnTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, turn)
}

func (r *recordingTraceEmitter) RecordTurnRecord(turn types.TurnRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnRecords = append(r.turnRecords, turn)
}

func (r *recordingTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = append(r.toolCalls, call)
}

func (r *recordingTraceEmitter) RecordPermissionDenial() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.permissionDenials++
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
	// A read-only built-in, so a read-only child still has a built-in tool.
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "A read-only built-in",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "file contents", nil
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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
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
	require.NoError(t, err)

	assert.Equal(t, "success", result.Outcome)
	assert.Equal(t, "Sub-agent output here.", result.Output)
	assert.Equal(t, 1, result.Turns)
}

// TestSpawnSubAgent_CacheKeyDiffersFromParent checks that a sub-agent's
// requests carry their own cache key, so the child's conversation is not
// routed as a continuation of the parent's.
func TestSpawnSubAgent_CacheKeyDiffersFromParent(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do a subtask",
	})
	require.NoError(t, err)

	child := prov.lastParams.CacheKey
	require.NotEmpty(t, child, "sub-agent request should carry a cache key")
	require.NotEqual(t, providerCacheKey(parentConfig.RunID), child,
		"sub-agent cache key should differ from the parent's")
}

func TestSpawnSubAgent_EmptyPromptReturnsError(t *testing.T) {
	prov := &mockProvider{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "",
	})
	require.Error(t, err)
}

// TestCapSubAgentMaxTurns_BoundsRequestedTurns covers each branch of
// capSubAgentMaxTurns: the default for zero, the sub-agent maximum, the
// parent maximum, and an in-bounds request.
func TestCapSubAgentMaxTurns_BoundsRequestedTurns(t *testing.T) {
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
			require.Equal(t, tt.expectedCapped, got)
			t.Logf("capSubAgentMaxTurns(%d, %d) = %d", tt.requested, tt.parentMax, got)
		})
	}
}

// TestSpawnSubAgent_MaxTurnsRespectedAtRuntime runs SpawnSubAgent with a
// provider that finishes in one turn and checks that a request above the
// sub-agent maximum still completes, with Turns bounded by the cap.
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
	require.NoError(t, err)

	assert.Equal(t, "success", result.Outcome, "run should complete with the cap applied")
	assert.GreaterOrEqual(t, result.Turns, 1)
	// The helper must return maxSubAgentMaxTurns for this input, or the
	// run above did not exercise the cap.
	assert.Equal(t, maxSubAgentMaxTurns, capSubAgentMaxTurns(999, parentConfig.MaxTurns),
		"capSubAgentMaxTurns(999, %d)", parentConfig.MaxTurns)
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

	require.Len(t, defs, 2)

	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	assert.NotContains(t, names, "spawn_agent")

	assert.Nil(t, filtered.Resolve("spawn_agent"), "spawn_agent should not resolve in the filtered registry")
	assert.NotNil(t, filtered.Resolve("read_file"))
	assert.NotNil(t, filtered.Resolve("run_command"))
}

// The recursion guard must exclude spawn_agent by its internal id even
// when a Presenter aliases it to a different model-facing name. Filtering
// on the alias would let the child loop inherit spawn_agent.
func TestFilterToolRegistry_ExcludesAliasedSpawnAgent(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     func(_ context.Context, _ json.RawMessage) (string, error) { return "", nil },
	})
	registry.Register(&tool.Tool{
		Name:        "run_command",
		Description: "Run a command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     func(_ context.Context, _ json.RawMessage) (string, error) { return "", nil },
	})

	// Profile aliases spawn_agent to "agent" (and run_command to "bash").
	profile := tool.NewProfile("alias-spawn", map[string]string{
		"spawn_agent": "agent",
		"run_command": "bash",
	}, nil)
	presenter, err := tool.NewPresenter(registry, profile)
	require.NoError(t, err)

	presented := make([]string, 0)
	for _, d := range presenter.List() {
		presented = append(presented, d.Name)
	}
	require.Contains(t, presented, "agent", "presenter should alias spawn_agent to agent")

	filtered := filterToolRegistry(presenter, "spawn_agent")

	assert.Nil(t, filtered.Resolve("spawn_agent"), "aliased spawn_agent leaked past the recursion guard by internal name")
	assert.Nil(t, filtered.Resolve("agent"), "aliased spawn_agent leaked past the recursion guard by alias name")
	assert.NotNil(t, filtered.Resolve("run_command"), "run_command should survive filtering")
	assert.Len(t, filtered.List(), 1, "only run_command should remain")
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

	// An empty Mode inherits the parent's mode.
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do something",
		Mode:   "",
	})
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome)
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
	require.NoError(t, err)
	// A deny on PreTurn turn 0 aborts with guardrail_blocked. A sub-agent
	// without the parent's guard would run to success.
	require.Equal(t, "guardrail_blocked", result.Outcome, "parent guard should be inherited")
}

// TestSpawnSubAgent_HooksNeverInvokedOnChild checks that lifecycle hooks
// run only for the parent run. Hooks configured on the parent loop or the
// parent RunConfig must not fire during a sub-agent spawn. subagent.go
// clears the child's hook config and gives the child loop a no-op runner.
// The fake runner counts calls, so a non-zero count means the child's
// hook phase ran.
func TestSpawnSubAgent_HooksNeverInvokedOnChild(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent output."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentHooks := &fakeHookRunner{}
	parentLoop.Hooks = parentHooks

	parentConfig := buildTestConfig()
	parentConfig.Hooks = &types.HooksConfig{
		PreRun: []types.HookConfig{{Name: "clone", Command: "git clone . ."}},
	}

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do a subtask",
	})
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome)
	assert.Zero(t, parentHooks.preCalls, "parent pre-run hook should not run during sub-agent spawn")
	assert.Empty(t, parentHooks.postCalls, "parent post-run hook should not run during sub-agent spawn")
}

func TestCaptureTransport_RecordsTextDeltas(t *testing.T) {
	ct := newCaptureTransport()

	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "Hello "})
	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "world!"})
	_ = ct.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc_1", Content: "result"})
	_ = ct.Emit(types.HarnessEvent{Type: "done", StopReason: "success"})

	require.Equal(t, "Hello world!", ct.lastText())
}

func TestCaptureTransport_EmptyWhenNoTextDeltas(t *testing.T) {
	ct := newCaptureTransport()
	_ = ct.Emit(types.HarnessEvent{Type: "done"})

	require.Empty(t, ct.lastText())
}

// TestCaptureTransport_ToolCallEventIsNoOp checks that a tool_call event
// passes through captureTransport without an error and without changing
// the accumulated text.
func TestCaptureTransport_ToolCallEventIsNoOp(t *testing.T) {
	ct := newCaptureTransport()

	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "before "})
	require.NoError(t, ct.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: json.RawMessage(`{}`)}))
	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "after"})

	assert.Equal(t, "before after", ct.lastText())
}

// TestSpawnSubAgent_TraceEventsForwardedToParent checks that the child's
// turn traces reach the parent's trace emitter. Each forwarded turn carries
// the parent's run ID as ParentRunID and the child's own run ID as RunID.
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

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)

	turns, _ := parentEmitter.snapshot()
	require.NotEmpty(t, turns, "no turn was forwarded to the parent emitter")
	for i, turn := range turns {
		assert.Equal(t, parentConfig.RunID, turn.ParentRunID, "turn %d ParentRunID", i)
		assert.NotEmpty(t, turn.RunID, "turn %d RunID", i)
		assert.NotEqual(t, parentConfig.RunID, turn.RunID, "turn %d RunID should be the child's", i)
		assert.Equal(t, "claude-sonnet-4-6", turn.Model, "turn %d Model should be the router's selected model", i)
	}
}

// TestSpawnSubAgent_TraceToolCallsForwardedToParent checks that tool calls
// made by a sub-agent reach the parent's trace emitter. The path is
// SpawnSubAgent, then child tool dispatch, then
// NestedJSONLEmitter.RecordToolCall, then the parent emitter. Forwarded
// calls carry the child's RunID and the parent's run ID as ParentRunID.
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

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)

	_, calls := parentEmitter.snapshot()
	require.NotEmpty(t, calls, "no tool call was forwarded to the parent emitter")
	for i, c := range calls {
		assert.Equal(t, parentConfig.RunID, c.ParentRunID, "call %d ParentRunID", i)
		assert.NotEmpty(t, c.RunID, "call %d RunID", i)
		assert.NotEqual(t, parentConfig.RunID, c.RunID, "call %d RunID should be the child's", i)
		assert.Equal(t, "test_tool", c.Name, "call %d Name", i)
		assert.Equal(t, "tc_1", c.ID, "call %d ID should be the provider's tool_use ID", i)
	}
}

// Failed tool calls forwarded to the parent trace are written with
// json.Marshal, which bypasses the slog scrub handler, so their error text
// must be scrubbed before it is recorded.
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

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt:   "do a subtask",
		MaxTurns: 1,
	})
	require.NoError(t, err)

	_, calls := parentEmitter.snapshot()
	require.NotEmpty(t, calls, "expected a forwarded tool call on the parent emitter")

	var failed []types.ToolCallTrace
	for _, c := range calls {
		if !c.Success {
			failed = append(failed, c)
		}
	}
	require.NotEmpty(t, failed, "expected a failed forwarded ToolCallTrace")
	for i, c := range failed {
		require.NotEmpty(t, c.ErrorReason, "failed forwarded tool call %d has empty ErrorReason", i)
		require.NotContains(t, c.ErrorReason, fakeKey, "forwarded ErrorReason leaked an unscrubbed key")
	}
}

// leakErr's Error() embeds a fake API key, standing in for an upstream error
// that legitimately contains secret-shaped text such as a request URL with
// an Authorization header.
type leakErr struct {
	fakeKey string
}

func (e *leakErr) Error() string {
	return "upstream auth failed for request token=" + e.fakeKey
}

// Metrics emitted by a sub-agent carry run.subagent=true and run.parent_id,
// so dashboards can separate sub-agent work from top-level runs.
func TestSpawnSubAgent_MetricsTaggedAsSubAgent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)

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

	_, err = SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	// check reports whether the named sum has a data point tagged as a
	// sub-agent run, and whether one carries the parent's run ID.
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

	// runs is incremented once per run and turns once per turn, so checking
	// both covers the per-turn instruments as well as the per-run one.
	for _, name := range []string{"stirrup.harness.runs", "stirrup.harness.turns"} {
		t.Run(name, func(t *testing.T) {
			sawSubAgent, sawParentRunID := check(name)
			require.True(t, sawSubAgent, "no data point with run.subagent=true")
			require.True(t, sawParentRunID, "no data point with run.parent_id=%q", parentConfig.RunID)
		})
	}
}

// TestSpawnSubAgent_RecordsSubagentMetrics asserts that one
// SpawnSubAgent invocation records exactly one stirrup.subagent.spawns
// observation (with parent.mode + success), at least one
// stirrup.subagent.duration observation, and the input/output
// token counters are populated from the child's RunTrace.
func TestSpawnSubAgent_RecordsSubagentMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Metrics = metrics

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-subagent-metrics-1"
	parentConfig.Mode = "execution"

	_, err = SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	spawns := findSubagentCounter(t, rm, "stirrup.subagent.spawns")
	assert.EqualValues(t, 1, spawns.total, "stirrup.subagent.spawns total")
	assert.Equal(t, "execution", spawns.attrs["parent.mode"], "stirrup.subagent.spawns parent.mode")
	assert.Equal(t, "true", spawns.attrs["success"], "stirrup.subagent.spawns success")

	assert.True(t, subagentHistogramRecorded(t, rm, "stirrup.subagent.duration"),
		"stirrup.subagent.duration recorded no observations")

	// The mock provider reports no tokens, so the values may be zero, but
	// the data points must exist and carry the sub-agent's labels.
	for _, name := range []string{"stirrup.subagent.tokens.input", "stirrup.subagent.tokens.output"} {
		t.Run(name, func(t *testing.T) {
			attrs := findSubagentCounter(t, rm, name).attrs
			require.Equal(t, "execution", attrs["parent.mode"])
			require.Equal(t, "anthropic", attrs["gen_ai.provider.name"])
			require.Equal(t, "claude-sonnet-4-6", attrs["gen_ai.request.model"])
		})
	}
}

// The sub-agent token counters are labelled with the router's selection for
// the sub-agent's mode, so they are correct when modelRouter.provider and
// modelRouter.model are unset and when per-mode routing picks another model.
func TestSpawnSubAgent_TokenCountersLabelledWithRoutedSelection(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok."},
			{Type: "message_complete", StopReason: "end_turn", InputTokens: 10, OutputTokens: 2},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Metrics = metrics
	parentLoop.Router = router.NewPerModeRouter(
		router.ModelSelection{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		map[string]router.ModelSelection{"research": {Provider: "anthropic", Model: "claude-haiku-5-5"}},
	)

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-subagent-routed-1"
	parentConfig.Mode = "execution"
	parentConfig.ModelRouter = types.ModelRouterConfig{}

	_, err = SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "research a thing",
		Mode:   "research",
	})
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	attrs := findSubagentCounter(t, rm, "stirrup.subagent.tokens.input").attrs
	assert.Equal(t, "anthropic", attrs["gen_ai.provider.name"])
	assert.Equal(t, "claude-haiku-5-5", attrs["gen_ai.request.model"])
}

// Cache read and write tokens of the child run are recorded alongside its
// input and output tokens.
func TestSpawnSubAgent_RecordsSubagentCacheMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok."},
			{Type: "message_complete", StopReason: "end_turn", InputTokens: 4000, OutputTokens: 20, CacheReadTokens: 3000, CacheWriteTokens: 700},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Metrics = metrics

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-subagent-cache-metrics-1"
	parentConfig.Mode = "execution"

	_, err = SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for name, want := range map[string]int64{
		"stirrup.subagent.tokens.input":       4000,
		"stirrup.subagent.tokens.output":      20,
		"stirrup.subagent.tokens.cache_read":  3000,
		"stirrup.subagent.tokens.cache_write": 700,
	} {
		t.Run(name, func(t *testing.T) {
			got := findSubagentCounter(t, rm, name)
			require.Equal(t, want, got.total)
			require.Equal(t, "execution", got.attrs["parent.mode"])
		})
	}
}

// subagentCounterDP is the first data point of an int64 counter, with its
// attributes flattened to strings.
type subagentCounterDP struct {
	total int64
	attrs map[string]string
}

func findSubagentCounter(t *testing.T, rm metricdata.ResourceMetrics, name string) subagentCounterDP {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %q is not a Sum[int64]", name)
			if len(sum.DataPoints) == 0 {
				return subagentCounterDP{}
			}
			dp := sum.DataPoints[0]
			out := subagentCounterDP{total: dp.Value, attrs: make(map[string]string)}
			for _, kv := range dp.Attributes.ToSlice() {
				out.attrs[string(kv.Key)] = kv.Value.String()
			}
			return out
		}
	}
	require.Failf(t, "metric not found", "metric %q not found", name)
	return subagentCounterDP{}
}

func subagentHistogramRecorded(t *testing.T, rm metricdata.ResourceMetrics, name string) bool {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				return false
			}
			for _, dp := range h.DataPoints {
				if dp.Count > 0 {
					return true
				}
			}
		}
	}
	return false
}

// The parent loop's base MetricAttrs must reach the spawn observation even
// when the child run fails, so spawn metrics from nested sub-agents keep
// their run.subagent and run.parent_id attributes.
func TestSpawnSubAgent_RecordsSubagentMetrics_Failure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)

	// The provider streams an error event, so the child run itself fails.
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "error", Error: &providerUnavailableError{}},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Metrics = metrics
	// Attributes that a nested sub-agent's loop carries.
	parentLoop.MetricAttrs = []attribute.KeyValue{
		attribute.Bool("run.subagent", true),
		attribute.String("run.parent_id", "grandparent-run-1"),
	}

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-subagent-failure-1"
	parentConfig.Mode = "execution"

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask",
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	spawns := findSubagentCounter(t, rm, "stirrup.subagent.spawns")
	assert.EqualValues(t, 1, spawns.total, "stirrup.subagent.spawns total")
	assert.Equal(t, "execution", spawns.attrs["parent.mode"], "stirrup.subagent.spawns parent.mode")
	assert.Equal(t, "true", spawns.attrs["run.subagent"], "base attribute run.subagent")
	assert.Equal(t, "grandparent-run-1", spawns.attrs["run.parent_id"], "base attribute run.parent_id")
}

// providerUnavailableError is a canned error streamed through a child
// loop's StreamEvent.Error field so SpawnSubAgent's failure path can
// be exercised deterministically.
type providerUnavailableError struct{}

func (e *providerUnavailableError) Error() string { return "provider unavailable" }

// parallelToolCallProvider answers every request but the last with two
// parallel tool calls and records the messages each request carried.
type parallelToolCallProvider struct {
	mu        sync.Mutex
	toolTurns int
	requests  [][]types.Message
}

func (p *parallelToolCallProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := len(p.requests)
	p.requests = append(p.requests, append([]types.Message(nil), params.Messages...))

	var events []types.StreamEvent
	if turn < p.toolTurns {
		for call := range 2 {
			events = append(events, types.StreamEvent{
				Type:  "tool_call",
				ID:    fmt.Sprintf("tc_%d_%d", turn, call),
				Name:  "big_tool",
				Input: map[string]any{"turn": turn, "call": call},
			})
		}
		events = append(events, types.StreamEvent{Type: "message_complete", StopReason: "tool_use"})
	} else {
		events = []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent done."},
			{Type: "message_complete", StopReason: "end_turn"},
		}
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestSpawnSubAgent_CompactionNeverOrphansToolResults pins that a sub-agent
// whose history outgrows its context budget sends every request with its
// task first and each tool_result right after the assistant turn that
// called it, including results of parallel calls.
func TestSpawnSubAgent_CompactionNeverOrphansToolResults(t *testing.T) {
	const subTask = "Investigate the repository layout"
	prov := &parallelToolCallProvider{toolTurns: 5}
	parentLoop := buildSubAgentTestLoop(nil)
	parentLoop.Provider = prov
	registry, ok := parentLoop.Tools.(*tool.Registry)
	require.True(t, ok, "parent loop Tools is %T, want *tool.Registry", parentLoop.Tools)
	registry.Register(&tool.Tool{
		Name:        "big_tool",
		Description: "Returns a large output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return strings.Repeat("output line\n", 1700), nil
		},
	})

	parentConfig := buildTestConfig()
	// effectiveReserveForResponse keeps 64000 of 80000 tokens, leaving
	// room for about one exchange of two ~5000-token results.
	parentConfig.ContextStrategy.MaxTokens = 80_000

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{Prompt: subTask})
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome)
	require.Len(t, prov.requests, prov.toolTurns+1)

	// precededByToolUse reports whether msgs[j] is a tool_result that answers
	// a tool_use in the assistant message directly before it.
	precededByToolUse := func(msgs []types.Message, j int, toolUseID string) bool {
		if j == 0 || msgs[j-1].Role != "assistant" {
			return false
		}
		return slices.ContainsFunc(msgs[j-1].Content, func(b types.ContentBlock) bool {
			return b.Type == "tool_use" && b.ID == toolUseID
		})
	}

	compacted := false
	for i, msgs := range prov.requests {
		if want := 2*i + 1; len(msgs) < want {
			compacted = true
		}
		require.NotEmpty(t, msgs, "request %d", i)
		require.Equal(t, "user", msgs[0].Role, "request %d should open with the sub-agent task", i)
		require.Equal(t, subTask, msgs[0].Content[0].Text, "request %d should open with the sub-agent task", i)
		for j, msg := range msgs {
			for _, block := range msg.Content {
				if block.Type != "tool_result" {
					continue
				}
				require.True(t, precededByToolUse(msgs, j, block.ToolUseID),
					"request %d message %d: tool_result %q has no tool_use in the previous message", i, j, block.ToolUseID)
			}
		}
	}
	require.True(t, compacted, "no request was compacted; the budget does not exercise compaction")
}
