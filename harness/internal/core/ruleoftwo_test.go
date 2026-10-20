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
			must := require.New(t)
			arming := resolveRuleOfTwoArming(tc.cfg)
			must.Equal(tc.wantArmed, arming.armed)

			monitor := buildRuleOfTwoMonitor(arming)
			if !tc.wantArmed {
				must.IsType(ruleoftwo.Noop{}, monitor)
				return
			}
			must.IsType(&ruleoftwo.PatternMonitor{}, monitor)

			must.Equal(tc.wantEnforcing, arming.enforcing)
			must.Equal(tc.wantEnforcing, monitor.Enforcing())
			must.Equal(tc.wantAction, arming.action)

			wantEffective := tc.wantAction
			if !tc.wantEnforcing {
				wantEffective = "warn"
			}
			must.Equal(wantEffective, monitor.Action())
			must.Equal(tc.wantCriteria, arming.criteria)
			must.Equal(tc.wantStatic, monitor.Tripped(), "latch must start tripped only when sensitive data is declared")
		})
	}
}

func TestEmitRuleOfTwoEvents_RuntimeArmedEvent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
		DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
	}
	sec, buf := captureSecLogger(t)
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
	out := buf.String()

	must.Contains(out, `"event":"rule_of_two_runtime_armed"`)
	check.Contains(out, `"classifier":"patterns"`)
	check.Contains(out, `"onDetect":"block-external"`)
	check.Contains(out, `"enforcing":true`)
}

func TestEmitRuleOfTwoEvents_UnarmedEmitsNoRuntimeEvent(t *testing.T) {
	check := assert.New(t)
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	sec, buf := captureSecLogger(t)
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))

	check.NotContains(buf.String(), "rule_of_two_runtime_armed")
}

// --- loop wiring ---

// registerLeakyTool adds a tool whose result carries a live-shaped AWS
// key, simulating an agent reading credentials from a workspace file.
func registerLeakyTool(t *testing.T, loop *AgenticLoop) {
	t.Helper()
	must := require.New(t)
	registry, ok := loop.Tools.(*tool.Registry)
	must.True(ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
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
	check := assert.New(t)
	must := require.New(t)
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = leakyToolProvider()
	registerLeakyTool(t, loop)
	monitor := defaultRuleOfTwoTestMonitor()
	loop.RuleOfTwo = monitor
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.True(monitor.Tripped(), "monitor must latch on the key-bearing tool result")

	out := secBuf.String()
	check.Contains(out, `"event":"sensitive_data_detected"`)
	check.Contains(out, `"source":"tool_result"`)
	check.Contains(out, `"secret/aws_access_key_id"`)
	check.NotContains(out, fakeLiveAWSKey, "matched content must not reach the security event stream")
	check.Contains(out, `"event":"rule_of_two_triggered"`)
	check.Contains(out, `"sensitiveData":true`)
	check.Contains(out, `"action":"block-external"`)
	check.Contains(out, `"transition":true`)
	check.Contains(out, `"scanning_suspended":true`, "soak data ends at the latch, so the trigger event suspends scanning")

	// Dark-ship pin: an identical run with the Noop monitor must end the
	// same way — outcome and turn count unchanged by detection.
	baseline := buildTestLoop(nil)
	baseline.Provider = leakyToolProvider()
	registerLeakyTool(t, baseline)
	baseline.RuleOfTwo = ruleoftwo.NewNoop()
	baselineTrace, err := baseline.Run(context.Background(), buildTestConfigWithMaxTurns(4))
	must.NoError(err)
	check.Equal(baselineTrace.Outcome, runTrace.Outcome, "outcome must match an unmonitored run")
	check.Equal(baselineTrace.Turns, runTrace.Turns, "turn count must match an unmonitored run")
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
	must := require.New(t)
	check := assert.New(t)
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
	must.True(ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
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
	must.NoError(err)
	check.Equal("success", runTrace.Outcome, "observe-only run must not change the outcome")
	must.True(monitor.Tripped(), "monitor must latch on a credential present only in the structured payload")

	out := secBuf.String()
	check.Contains(out, `"event":"sensitive_data_detected"`)
	check.Contains(out, `"source":"tool_result"`)
	check.Contains(out, `"secret/aws_access_key_id"`)
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
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	check.Equal("success", runTrace.Outcome, "observe-only run must not change the outcome")
	must.True(prov.called, "provider was never contacted")
	must.True(prov.trippedAtCall, "latch must trip before the first provider call (turn-0 scan precedes Prompt.Build)")

	out := secBuf.String()
	check.Contains(out, `"source":"dynamic_context"`)
	check.Contains(out, `"pii/credit_card"`)
	check.Contains(out, `"event":"rule_of_two_triggered"`)
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
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	loop.Metrics = metrics
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	must.True(monitor.Tripped(), "matching guard criterion must trip the ratchet")

	out := secBuf.String()
	check.Contains(out, `"source":"guard:stub"`)
	// Provenance namespacing: the guard's criterion must land in the
	// patterns field prefixed "guard:" so it can never impersonate a
	// deterministic detector name.
	check.Contains(out, `"patterns":["guard:sensitive_data"]`)
	check.Contains(out, `"event":"rule_of_two_triggered"`)
	check.Equal(int64(1), ruleOfTwoDetectionPatternCount(t, reader, "guard:sensitive_data"), "detections labelled with the guard-prefixed pattern")
	check.Equal(int64(0), ruleOfTwoDetectionPatternCount(t, reader, "sensitive_data"), "unprefixed criterion must not appear as a pattern label")
}

// ruleOfTwoDetectionPatternCount sums stirrup.ruleoftwo.detections data
// points whose pattern attribute equals pattern.
func ruleOfTwoDetectionPatternCount(t *testing.T, reader *sdkmetric.ManualReader, pattern string) int64 {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	err := reader.Collect(context.Background(), &rm)
	must.NoError(err)
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
	must := require.New(t)
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
	must.NoError(err)
	must.False(monitor.Tripped(), "non-matching criterion must not trip the ratchet")
}

func TestLoop_RuleOfTwoNoopEmitsNoEvents(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = leakyToolProvider()
	registerLeakyTool(t, loop)
	loop.RuleOfTwo = ruleoftwo.NewNoop()
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)

	out := secBuf.String()
	check.NotContains(out, "sensitive_data_detected")
	check.NotContains(out, "rule_of_two_triggered")
}

// --- sub-agent sharing ---

func TestSpawnSubAgent_SharesRuleOfTwoMonitor(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	check.Equal("success", result.Outcome, "observe-only run must not change the outcome")
	must.True(monitor.Tripped(), "child observation must trip the shared parent monitor")
}
