package core

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/ruleoftwo"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// fakeLiveAWSKey is a live-shaped AKIA key (16 uppercase chars, not
// EXAMPLE-suffixed) so the detector's doc-example allowlist does not
// reject it.
const fakeLiveAWSKey = "AKIAQWERTYUIOPASDFGH"

func defaultRuleOfTwoTestMonitor() *ruleoftwo.PatternMonitor {
	return ruleoftwo.NewPatternMonitor(true, "block-external", []string{"sensitive_data", "pii"}, false)
}

// --- arming matrix ---

func TestResolveRuleOfTwoArming_Matrix(t *testing.T) {
	sensitive := true
	enforceFalse := false

	dynamicContextAndCommand := func() *types.RunConfig {
		return &types.RunConfig{
			Mode:             "execution",
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
			DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		}
	}
	commandOnly := func() *types.RunConfig {
		return &types.RunConfig{
			Mode:             "execution",
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
		}
	}
	dynamicContextOnly := func() *types.RunConfig {
		return &types.RunConfig{
			Mode:             "execution",
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
			DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		}
	}

	cases := []struct {
		name          string
		cfg           *types.RunConfig
		wantArmed     bool
		wantEnforcing bool
		wantAction    string
		wantCriteria  []string
		wantStatic    bool
	}{
		{
			name: "classifier none disarms even with dynamic context and run_command",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{Classifier: "none"}}
				return c
			}(),
			wantArmed: false,
		},
		{
			name:          "default policy arms enforcing with block-external",
			cfg:           dynamicContextAndCommand(),
			wantArmed:     true,
			wantEnforcing: true,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "on-detect abort arms enforcing abort",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{OnDetect: "abort"}}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: true,
			wantAction:    "abort",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "ask-upstream policy arms observe-only",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.PermissionPolicy = types.PermissionPolicyConfig{Type: "ask-upstream"}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "enforce false arms observe-only",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforceFalse}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "declared sensitive data arms observe-only with a pre-tripped latch",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.SensitiveData = &sensitive
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
			wantStatic:    true,
		},
		{
			name:      "run_command without dynamic context stays unarmed",
			cfg:       commandOnly(),
			wantArmed: false,
		},
		{
			name:      "dynamic context without run_command stays unarmed",
			cfg:       dynamicContextOnly(),
			wantArmed: false,
		},
		{
			name: "patterns classifier arms observe-only without dynamic context",
			cfg: func() *types.RunConfig {
				c := commandOnly()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{Classifier: "patterns"}}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "custom guard criteria override the default set",
			cfg: func() *types.RunConfig {
				c := dynamicContextAndCommand()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{GuardCriteria: []string{"custom_pii"}}}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: true,
			wantAction:    "block-external",
			wantCriteria:  []string{"custom_pii"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arming := resolveRuleOfTwoArming(tc.cfg)
			require.Equal(t, tc.wantArmed, arming.armed)

			monitor := buildRuleOfTwoMonitor(arming)
			if !tc.wantArmed {
				require.IsType(t, ruleoftwo.Noop{}, monitor)
				return
			}
			require.IsType(t, &ruleoftwo.PatternMonitor{}, monitor)

			require.Equal(t, tc.wantEnforcing, arming.enforcing)
			require.Equal(t, tc.wantEnforcing, monitor.Enforcing())
			require.Equal(t, tc.wantAction, arming.action)

			wantEffective := tc.wantAction
			if !tc.wantEnforcing {
				wantEffective = "warn"
			}
			require.Equal(t, wantEffective, monitor.Action())
			require.Equal(t, tc.wantCriteria, arming.criteria)
			require.Equal(t, tc.wantStatic, monitor.Tripped(), "latch must start tripped only when sensitive data is declared")
		})
	}
}

func TestEmitRuleOfTwoEvents_RuntimeArmedEvent(t *testing.T) {
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
		DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
	}
	sec, buf := captureSecLogger(t)
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
	out := buf.String()

	require.Contains(t, out, `"event":"rule_of_two_runtime_armed"`)
	assert.Contains(t, out, `"classifier":"patterns"`)
	assert.Contains(t, out, `"onDetect":"block-external"`)
	assert.Contains(t, out, `"enforcing":true`)
}

func TestEmitRuleOfTwoEvents_UnarmedEmitsNoRuntimeEvent(t *testing.T) {
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	sec, buf := captureSecLogger(t)
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))

	assert.NotContains(t, buf.String(), "rule_of_two_runtime_armed")
}

// --- loop wiring ---

// registerLeakyTool adds a tool whose result carries a live-shaped AWS
// key, simulating an agent reading credentials from a workspace file.
func registerLeakyTool(t *testing.T, loop *AgenticLoop) {
	t.Helper()
	registry, ok := loop.Tools.(*tool.Registry)
	require.True(t, ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
	registry.Register(&tool.Tool{
		Name:        "leaky_tool",
		Description: "returns sensitive content",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "AWS_ACCESS_KEY_ID=" + fakeLiveAWSKey, nil
		},
	})
}

func leakyToolProvider() *scriptedProvider {
	return &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "leaky_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
}

// TestLoop_RuleOfTwoDetectsSensitiveToolResult asserts the dark-ship
// contract: a tool result carrying a live-shaped key produces
// sensitive_data_detected + rule_of_two_triggered events while the run
// completes with the same outcome and turn count as an unmonitored run.
func TestLoop_RuleOfTwoDetectsSensitiveToolResult(t *testing.T) {
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = leakyToolProvider()
	registerLeakyTool(t, loop)
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.True(t, monitor.Tripped(), "monitor must latch on the key-bearing tool result")

	out := secBuf.String()
	assert.Contains(t, out, `"event":"sensitive_data_detected"`)
	assert.Contains(t, out, `"source":"tool_result"`)
	assert.Contains(t, out, `"secret/aws_access_key_id"`)
	assert.NotContains(t, out, fakeLiveAWSKey, "matched content must not reach the security event stream")
	assert.Contains(t, out, `"event":"rule_of_two_triggered"`)
	assert.Contains(t, out, `"sensitiveData":true`)
	assert.Contains(t, out, `"action":"block-external"`)
	assert.Contains(t, out, `"transition":true`)
	assert.Contains(t, out, `"scanning_suspended":true`, "soak data ends at the latch, so the trigger event suspends scanning")

	// Dark-ship pin: an identical run with the Noop monitor must end the
	// same way — outcome and turn count unchanged by detection.
	baseline := buildTestLoop(nil)
	baseline.Provider = leakyToolProvider()
	registerLeakyTool(t, baseline)
	baseline.RuleOfTwo = ruleoftwo.NewNoop()
	baselineTrace, err := baseline.Run(context.Background(), buildTestConfigWithMaxTurns(4))
	require.NoError(t, err)
	assert.Equal(t, baselineTrace.Outcome, runTrace.Outcome, "outcome must match an unmonitored run")
	assert.Equal(t, baselineTrace.Turns, runTrace.Turns, "turn count must match an unmonitored run")
}

func buildTestConfigWithMaxTurns(maxTurns int) *types.RunConfig {
	cfg := buildTestConfig()
	cfg.MaxTurns = maxTurns
	return cfg
}

// TestLoop_RuleOfTwoDetectsStructuredToolResult pins that the monitor
// scans the full ContentBlock.Structured payload kept in history, a
// superset of the wire view the Anthropic and Gemini adapters send, so a
// credential present only in the structured payload latches even when
// the text Content is benign.
func TestLoop_RuleOfTwoDetectsStructuredToolResult(t *testing.T) {
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "structured_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	registry, ok := loop.Tools.(*tool.Registry)
	require.True(t, ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
	registry.Register(&tool.Tool{
		Name:        "structured_tool",
		Description: "returns a structured payload carrying a credential",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		StructuredHandler: func(_ context.Context, _ json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{
				Text:       "command completed",
				Structured: json.RawMessage(`{"stdout":"` + fakeLiveAWSKey + `"}`),
				Kind:       "command_result",
			}, nil
		},
	})
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, "success", runTrace.Outcome, "observe-only run must not change the outcome")
	require.True(t, monitor.Tripped(), "monitor must latch on a credential present only in the structured payload")

	out := secBuf.String()
	assert.Contains(t, out, `"event":"sensitive_data_detected"`)
	assert.Contains(t, out, `"source":"tool_result"`)
	assert.Contains(t, out, `"secret/aws_access_key_id"`)
}

// trippedAtStreamProvider snapshots the monitor's latch state at the
// moment the provider is first contacted, so tests can assert the
// turn-0 scan latched before any model call.
type trippedAtStreamProvider struct {
	mu            sync.Mutex
	monitor       ruleoftwo.Monitor
	trippedAtCall bool
	called        bool
	events        []types.StreamEvent
}

func (p *trippedAtStreamProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	if !p.called {
		p.called = true
		p.trippedAtCall = p.monitor.Tripped()
	}
	p.mu.Unlock()
	ch := make(chan types.StreamEvent, len(p.events))
	for _, e := range p.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestLoop_RuleOfTwoLatchesOnDynamicContextBeforeFirstProviderCall(t *testing.T) {
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor
	prov := &trippedAtStreamProvider{
		monitor: monitor,
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop.Provider = prov

	config := buildTestConfig()
	// Luhn-valid PAN (the canonical Visa test number passes by design).
	config.DynamicContext = map[string]types.DynamicContextValue{
		"record": {Value: "customer card 4111111111111111 on file"},
	}

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, "success", runTrace.Outcome, "observe-only run must not change the outcome")
	require.True(t, prov.called, "provider was never contacted")
	require.True(t, prov.trippedAtCall, "latch must trip before the first provider call (turn-0 scan precedes Prompt.Build)")

	out := secBuf.String()
	assert.Contains(t, out, `"source":"dynamic_context"`)
	assert.Contains(t, out, `"pii/credit_card"`)
	assert.Contains(t, out, `"event":"rule_of_two_triggered"`)
}

// criterionGuard allows everything but tags each decision with a fixed
// criterion, exercising the ratchet without a deny.
type criterionGuard struct {
	criterion string
}

func (g *criterionGuard) Check(_ context.Context, _ guard.Input) (*guard.Decision, error) {
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "stub", Criterion: g.criterion}, nil
}

func TestLoop_RuleOfTwoGuardCriterionRatchetTrips(t *testing.T) {
	var secBuf bytes.Buffer
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoopWithSecurity(prov, &secBuf)
	loop.GuardRail = &criterionGuard{criterion: "sensitive_data"}
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := observability.NewMetricsForTesting(mp)
	require.NoError(t, err)
	loop.Metrics = metrics
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, "success", runTrace.Outcome)
	require.True(t, monitor.Tripped(), "matching guard criterion must trip the ratchet")

	out := secBuf.String()
	assert.Contains(t, out, `"source":"guard:stub"`)
	// Provenance namespacing: the guard's criterion must land in the
	// patterns field prefixed "guard:" so it can never impersonate a
	// deterministic detector name.
	assert.Contains(t, out, `"patterns":["guard:sensitive_data"]`)
	assert.Contains(t, out, `"event":"rule_of_two_triggered"`)
	assert.Equal(t, int64(1), ruleOfTwoDetectionPatternCount(t, reader, "guard:sensitive_data"), "detections labelled with the guard-prefixed pattern")
	assert.Equal(t, int64(0), ruleOfTwoDetectionPatternCount(t, reader, "sensitive_data"), "unprefixed criterion must not appear as a pattern label")
}

// ruleOfTwoDetectionPatternCount sums stirrup.ruleoftwo.detections data
// points whose pattern attribute equals pattern.
func ruleOfTwoDetectionPatternCount(t *testing.T, reader *sdkmetric.ManualReader, pattern string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	err := reader.Collect(context.Background(), &rm)
	require.NoError(t, err)
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.ruleoftwo.detections" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "pattern" && kv.Value.AsString() == pattern {
						total += dp.Value
					}
				}
			}
		}
	}
	return total
}

func TestLoop_RuleOfTwoGuardNonMatchingCriterionDoesNotTrip(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)
	loop.GuardRail = &criterionGuard{criterion: "jailbreak"}
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor

	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.False(t, monitor.Tripped(), "non-matching criterion must not trip the ratchet")
}

func TestLoop_RuleOfTwoNoopEmitsNoEvents(t *testing.T) {
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = leakyToolProvider()
	registerLeakyTool(t, loop)
	loop.RuleOfTwo = ruleoftwo.NewNoop()
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, "success", runTrace.Outcome)

	out := secBuf.String()
	assert.NotContains(t, out, "sensitive_data_detected")
	assert.NotContains(t, out, "rule_of_two_triggered")
}

// --- sub-agent sharing ---

func TestSpawnSubAgent_SharesRuleOfTwoMonitor(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "sub output"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	parentLoop := buildSubAgentTestLoop(prov)
	monitor := defaultRuleOfTwoTestMonitor()
	parentLoop.RuleOfTwo = monitor
	parentConfig := buildTestConfig()

	// The child's turn-0 prompt scan runs against the shared instance:
	// sensitive content observed inside the sub-agent must latch the
	// whole run, or spawn_agent becomes a latch escape hatch.
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "inspect this credential: " + fakeLiveAWSKey,
	})
	require.NoError(t, err)
	assert.Equal(t, "success", result.Outcome, "observe-only run must not change the outcome")
	require.True(t, monitor.Tripped(), "child observation must trip the shared parent monitor")
}
