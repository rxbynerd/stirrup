package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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

	// Base config shapes. u = DynamicContext present; e = run_command
	// enabled; s = SensitiveData declared.
	uAndE := func() *types.RunConfig {
		return &types.RunConfig{
			Mode:             "execution",
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
			DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		}
	}
	eOnly := func() *types.RunConfig {
		return &types.RunConfig{
			Mode:             "execution",
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}},
		}
	}
	uOnly := func() *types.RunConfig {
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
			name: "classifier none disarms even with u&&e",
			cfg: func() *types.RunConfig {
				c := uAndE()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{Classifier: "none"}}
				return c
			}(),
			wantArmed: false,
		},
		{
			name:          "u&&e&&!s default policy arms enforcing with block-external",
			cfg:           uAndE(),
			wantArmed:     true,
			wantEnforcing: true,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "u&&e&&!s with onDetect abort arms enforcing abort",
			cfg: func() *types.RunConfig {
				c := uAndE()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{OnDetect: "abort"}}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: true,
			wantAction:    "abort",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "u&&e with ask-upstream arms observe-only",
			cfg: func() *types.RunConfig {
				c := uAndE()
				c.PermissionPolicy = types.PermissionPolicyConfig{Type: "ask-upstream"}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "u&&e with enforce:false arms observe-only",
			cfg: func() *types.RunConfig {
				c := uAndE()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: &enforceFalse}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "u&&e&&s arms observe-only with pre-tripped latch",
			cfg: func() *types.RunConfig {
				c := uAndE()
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
			name:      "e without u stays unarmed",
			cfg:       eOnly(),
			wantArmed: false,
		},
		{
			name:      "u without e stays unarmed",
			cfg:       uOnly(),
			wantArmed: false,
		},
		{
			name: "explicit patterns classifier arms observe-only without u",
			cfg: func() *types.RunConfig {
				c := eOnly()
				c.RuleOfTwo = &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{Classifier: "patterns"}}
				return c
			}(),
			wantArmed:     true,
			wantEnforcing: false,
			wantAction:    "block-external",
			wantCriteria:  []string{"sensitive_data", "pii"},
		},
		{
			name: "custom guardCriteria override the default set",
			cfg: func() *types.RunConfig {
				c := uAndE()
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
			if arming.armed != tc.wantArmed {
				t.Fatalf("armed = %v, want %v", arming.armed, tc.wantArmed)
			}
			monitor := buildRuleOfTwoMonitor(arming)
			if !tc.wantArmed {
				if _, ok := monitor.(ruleoftwo.Noop); !ok {
					t.Fatalf("expected Noop monitor, got %T", monitor)
				}
				return
			}
			if _, ok := monitor.(*ruleoftwo.PatternMonitor); !ok {
				t.Fatalf("expected *PatternMonitor, got %T", monitor)
			}
			if arming.enforcing != tc.wantEnforcing {
				t.Errorf("enforcing = %v, want %v", arming.enforcing, tc.wantEnforcing)
			}
			if monitor.Enforcing() != tc.wantEnforcing {
				t.Errorf("monitor.Enforcing() = %v, want %v", monitor.Enforcing(), tc.wantEnforcing)
			}
			if arming.action != tc.wantAction {
				t.Errorf("action = %q, want %q", arming.action, tc.wantAction)
			}
			wantEffective := tc.wantAction
			if !tc.wantEnforcing {
				wantEffective = "warn"
			}
			if got := monitor.Action(); got != wantEffective {
				t.Errorf("monitor.Action() = %q, want %q", got, wantEffective)
			}
			if len(arming.criteria) != len(tc.wantCriteria) {
				t.Fatalf("criteria = %v, want %v", arming.criteria, tc.wantCriteria)
			}
			for i, c := range tc.wantCriteria {
				if arming.criteria[i] != c {
					t.Errorf("criteria[%d] = %q, want %q", i, arming.criteria[i], c)
				}
			}
			if monitor.Tripped() != tc.wantStatic {
				t.Errorf("monitor.Tripped() = %v, want %v (staticSensitive pre-trip)", monitor.Tripped(), tc.wantStatic)
			}
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
	if !strings.Contains(out, `"event":"rule_of_two_runtime_armed"`) {
		t.Fatalf("expected rule_of_two_runtime_armed event, got: %s", out)
	}
	if !strings.Contains(out, `"classifier":"patterns"`) {
		t.Errorf("expected classifier patterns, got: %s", out)
	}
	if !strings.Contains(out, `"onDetect":"block-external"`) {
		t.Errorf("expected onDetect block-external, got: %s", out)
	}
	if !strings.Contains(out, `"enforcing":true`) {
		t.Errorf("expected enforcing true, got: %s", out)
	}
}

func TestEmitRuleOfTwoEvents_UnarmedEmitsNoRuntimeEvent(t *testing.T) {
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	sec, buf := captureSecLogger(t)
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
	if strings.Contains(buf.String(), "rule_of_two_runtime_armed") {
		t.Errorf("unarmed run must not emit rule_of_two_runtime_armed, got: %s", buf.String())
	}
}

// --- loop wiring ---

// registerLeakyTool adds a tool whose result carries a live-shaped AWS
// key, simulating an agent reading credentials from a workspace file.
func registerLeakyTool(t *testing.T, loop *AgenticLoop) {
	t.Helper()
	registry, ok := loop.Tools.(*tool.Registry)
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !monitor.Tripped() {
		t.Fatal("monitor must latch on the key-bearing tool result")
	}

	out := secBuf.String()
	if !strings.Contains(out, `"event":"sensitive_data_detected"`) {
		t.Errorf("expected sensitive_data_detected event, got: %s", out)
	}
	if !strings.Contains(out, `"source":"tool_result"`) {
		t.Errorf("expected source tool_result, got: %s", out)
	}
	if !strings.Contains(out, `"secret/aws_access_key_id"`) {
		t.Errorf("expected the pattern name in the event, got: %s", out)
	}
	if strings.Contains(out, fakeLiveAWSKey) {
		t.Errorf("matched content leaked into the security event stream: %s", out)
	}
	if !strings.Contains(out, `"event":"rule_of_two_triggered"`) {
		t.Errorf("expected rule_of_two_triggered event, got: %s", out)
	}
	if !strings.Contains(out, `"sensitiveData":true`) {
		t.Errorf("expected sensitiveData:true in trigger payload, got: %s", out)
	}
	if !strings.Contains(out, `"action":"block-external"`) {
		t.Errorf("expected action block-external, got: %s", out)
	}
	if !strings.Contains(out, `"transition":true`) {
		t.Errorf("expected transition:true on the latching detection, got: %s", out)
	}
	if !strings.Contains(out, `"scanning_suspended":true`) {
		t.Errorf("expected scanning_suspended:true on the trigger event (soak data ends at the latch), got: %s", out)
	}

	// Dark-ship pin: an identical run with the Noop monitor must end the
	// same way — outcome and turn count unchanged by detection.
	baseline := buildTestLoop(nil)
	baseline.Provider = leakyToolProvider()
	registerLeakyTool(t, baseline)
	baseline.RuleOfTwo = ruleoftwo.NewNoop()
	baselineTrace, err := baseline.Run(context.Background(), buildTestConfigWithMaxTurns(4))
	if err != nil {
		t.Fatalf("baseline Run() error: %v", err)
	}
	if runTrace.Outcome != baselineTrace.Outcome {
		t.Errorf("outcome diverged: monitored %q vs baseline %q", runTrace.Outcome, baselineTrace.Outcome)
	}
	if runTrace.Turns != baselineTrace.Turns {
		t.Errorf("turns diverged: monitored %d vs baseline %d", runTrace.Turns, baselineTrace.Turns)
	}
}

func buildTestConfigWithMaxTurns(maxTurns int) *types.RunConfig {
	cfg := buildTestConfig()
	cfg.MaxTurns = maxTurns
	return cfg
}

// TestLoop_RuleOfTwoDetectsStructuredToolResult closes the structured-
// result bypass: the Anthropic and Gemini adapters forward
// ContentBlock.Structured to the model (as a second text block /
// functionResponse.response.structured respectively), so a credential
// present only in the structured payload is model-visible and must
// latch even when the text Content is benign.
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
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (observe-only)", runTrace.Outcome)
	}
	if !monitor.Tripped() {
		t.Fatal("monitor must latch on a credential present only in the structured payload")
	}
	out := secBuf.String()
	if !strings.Contains(out, `"event":"sensitive_data_detected"`) {
		t.Errorf("expected sensitive_data_detected event, got: %s", out)
	}
	if !strings.Contains(out, `"source":"tool_result"`) {
		t.Errorf("expected source tool_result, got: %s", out)
	}
	if !strings.Contains(out, `"secret/aws_access_key_id"`) {
		t.Errorf("expected the pattern name in the event, got: %s", out)
	}
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (observe-only)", runTrace.Outcome)
	}
	if !prov.called {
		t.Fatal("provider was never contacted")
	}
	if !prov.trippedAtCall {
		t.Fatal("latch must trip before the first provider call (turn-0 scan precedes Prompt.Build)")
	}

	out := secBuf.String()
	if !strings.Contains(out, `"source":"dynamic_context"`) {
		t.Errorf("expected source dynamic_context, got: %s", out)
	}
	if !strings.Contains(out, `"pii/credit_card"`) {
		t.Errorf("expected pii/credit_card pattern, got: %s", out)
	}
	if !strings.Contains(out, `"event":"rule_of_two_triggered"`) {
		t.Errorf("expected rule_of_two_triggered event, got: %s", out)
	}
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
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	loop.Metrics = metrics
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success", runTrace.Outcome)
	}
	if !monitor.Tripped() {
		t.Fatal("matching guard criterion must trip the ratchet")
	}
	out := secBuf.String()
	if !strings.Contains(out, `"source":"guard:stub"`) {
		t.Errorf("expected source guard:stub, got: %s", out)
	}
	// Provenance namespacing: the guard's criterion must land in the
	// patterns field prefixed "guard:" so it can never impersonate a
	// deterministic detector name.
	if !strings.Contains(out, `"patterns":["guard:sensitive_data"]`) {
		t.Errorf("expected patterns [guard:sensitive_data], got: %s", out)
	}
	if !strings.Contains(out, `"event":"rule_of_two_triggered"`) {
		t.Errorf("expected rule_of_two_triggered event, got: %s", out)
	}
	if got := ruleOfTwoDetectionPatternCount(t, reader, "guard:sensitive_data"); got != 1 {
		t.Errorf("rule-of-two detections with pattern=guard:sensitive_data = %d, want 1", got)
	}
	if got := ruleOfTwoDetectionPatternCount(t, reader, "sensitive_data"); got != 0 {
		t.Errorf("unprefixed criterion leaked into the pattern label: count = %d, want 0", got)
	}
}

// ruleOfTwoDetectionPatternCount sums stirrup.ruleoftwo.detections data
// points whose pattern attribute equals pattern.
func ruleOfTwoDetectionPatternCount(t *testing.T, reader *sdkmetric.ManualReader, pattern string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
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

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if monitor.Tripped() {
		t.Fatal("non-matching criterion must not trip the ratchet")
	}
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success", runTrace.Outcome)
	}
	out := secBuf.String()
	if strings.Contains(out, "sensitive_data_detected") {
		t.Errorf("Noop monitor must not emit sensitive_data_detected, got: %s", out)
	}
	if strings.Contains(out, "rule_of_two_triggered") {
		t.Errorf("Noop monitor must not emit rule_of_two_triggered, got: %s", out)
	}
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
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("outcome = %q, want success (observe-only)", result.Outcome)
	}
	if !monitor.Tripped() {
		t.Fatal("child observation must trip the shared parent monitor")
	}
}
