package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/ruleoftwo"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// --- factory helpers ---

func TestExternalCommToolSet(t *testing.T) {
	registry := tool.NewRegistry()
	for _, name := range []string{"run_command", "web_fetch", "read_file", "mcp_github_create_issue"} {
		registry.Register(&tool.Tool{
			Name:        name,
			Description: "t",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", nil
			},
		})
	}
	set := externalCommToolSet(registry)
	for _, want := range []string{"run_command", "web_fetch", "mcp_github_create_issue"} {
		if !set[want] {
			t.Errorf("externalCommToolSet missing %q", want)
		}
	}
	if set["read_file"] {
		t.Error("read_file is not external-comm")
	}
}

func TestWrapRuleOfTwoGate_Matrix(t *testing.T) {
	registry := tool.NewRegistry()
	inner := permission.NewAllowAll()
	cfg := buildTestConfig()
	metrics := observability.NewNoopMetrics()

	cases := []struct {
		name     string
		arming   ruleOfTwoArming
		wantGate bool
	}{
		{name: "unarmed unchanged", arming: ruleOfTwoArming{}, wantGate: false},
		{name: "armed observe-only unchanged", arming: ruleOfTwoArming{armed: true, enforcing: false, action: "block-external"}, wantGate: false},
		{name: "enforcing block-external gated", arming: ruleOfTwoArming{armed: true, enforcing: true, action: "block-external"}, wantGate: true},
		{name: "enforcing ask-upstream gated", arming: ruleOfTwoArming{armed: true, enforcing: true, action: "ask-upstream"}, wantGate: true},
		{name: "enforcing redact is loop-level", arming: ruleOfTwoArming{armed: true, enforcing: true, action: "redact"}, wantGate: false},
		{name: "enforcing abort is loop-level", arming: ruleOfTwoArming{armed: true, enforcing: true, action: "abort"}, wantGate: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			monitor := buildRuleOfTwoMonitor(tc.arming)
			tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
			pp := wrapRuleOfTwoGate(inner, monitor, tc.arming, registry, cfg, tp, metrics)
			gated := pp != permission.PermissionPolicy(inner)
			if gated != tc.wantGate {
				t.Fatalf("gated = %v, want %v (got %T)", gated, tc.wantGate, pp)
			}
			if tc.wantGate && permission.Unwrap(pp) != permission.PermissionPolicy(inner) {
				t.Errorf("Unwrap(gated) must reach the inner policy, got %T", permission.Unwrap(pp))
			}
		})
	}
}

// TestResolveRuleOfTwoArming_ExplicitPatternsWithSensitiveNotPreTripped
// is the D5 regression: an operator who explicitly selects
// classifier:"patterns" AND declares sensitiveData on a run that holds
// only the external-comm leg gets observe-only arming whose monitor is
// NOT pre-tripped, so detection telemetry actually flows. Pre-tripping
// (the pre-fix behaviour) suppressed every scan.
func TestResolveRuleOfTwoArming_ExplicitPatternsWithSensitiveNotPreTripped(t *testing.T) {
	sensitive := true
	cfg := &types.RunConfig{
		Mode:             "execution",
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"run_command"}}, // e only, not u
		SensitiveData:    &sensitive,
		RuleOfTwo:        &types.RuleOfTwoConfig{Runtime: &types.RuleOfTwoRuntimeConfig{Classifier: "patterns"}},
	}
	arming := resolveRuleOfTwoArming(cfg)
	if !arming.armed || arming.enforcing {
		t.Fatalf("want armed observe-only, got armed=%v enforcing=%v", arming.armed, arming.enforcing)
	}
	if arming.staticSensitive {
		t.Error("explicit-patterns observe-only monitor must not start pre-tripped (telemetry loss)")
	}
	monitor := buildRuleOfTwoMonitor(arming)
	if monitor.Tripped() {
		t.Error("monitor must not be pre-tripped; the first scan should be live")
	}
}

// TestBuildLoop_EnforceFalseObservesOnly is the D2 live-loop pin: with
// ruleOfTwo.enforce:false the factory arms observe-only — detections
// still emit, but the gate is absent, so external-comm tools keep
// working after sensitive data is seen. This is the auditable-override
// posture (the detection events stay; only enforcement is disarmed).
func TestBuildLoop_EnforceFalseObservesOnly(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com/\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://second.example/\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer server.Close()

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "ruleoftwo-enforce-false",
		Mode:             "research",
		Prompt:           "Investigate example.com",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         5,
		Timeout:          &timeout,
	}

	var secBuf bytes.Buffer
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()
	loop.Security = security.NewSecurityLogger(&secBuf, config.RunID)

	presenter := loop.Tools.(*tool.Presenter)
	registry := presenter.Unwrap().(*tool.Registry)
	original := registry.Resolve("web_fetch")
	var fetchCalls atomic.Int32
	registry.Register(&tool.Tool{
		Name:              original.Name,
		Description:       original.Description,
		InputSchema:       original.InputSchema,
		WorkspaceMutating: original.WorkspaceMutating,
		RequiresApproval:  original.RequiresApproval,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			fetchCalls.Add(1)
			return "fetched AWS_ACCESS_KEY_ID=" + fakeLiveAWSKey, nil
		},
	})

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success", runTrace.Outcome)
	}
	// Both web_fetch calls run: enforce:false means no gate, so egress
	// is never revoked despite the key entering the conversation.
	if got := fetchCalls.Load(); got != 2 {
		t.Errorf("web_fetch ran %d times; want 2 (egress intact under enforce:false)", got)
	}
	for _, call := range runTrace.ToolCalls {
		if call.Name == "web_fetch" && !call.Success && strings.Contains(call.ErrorReason, "rule_of_two:") {
			t.Errorf("enforce:false must not deny egress, but web_fetch was gated: %s", call.ErrorReason)
		}
	}
	// Detection telemetry still flows — the override is auditable.
	out := secBuf.String()
	if !strings.Contains(out, `"event":"sensitive_data_detected"`) {
		t.Errorf("observe-only run must still emit sensitive_data_detected, got: %s", out)
	}
	if !strings.Contains(out, `"action":"warn"`) {
		t.Errorf("observe-only detections must carry action=warn, got: %s", out)
	}
}

// TestBuildLoop_EnvExfilDeniedEndToEnd is the canonical Rule-of-Two
// enforcement scenario, driven end-to-end through the factory over a
// real local executor: the model reads a seeded config file carrying a
// live-shaped key (sensitive data enters the conversation), then issues
// an otherwise-innocuous run_command. The factory auto-arms enforcing
// block-external (a benign dynamic-context entry supplies the
// untrusted-input leg; run_command supplies external comms), so the
// run_command call is denied with the rule_of_two reason and the run
// still finishes cleanly. The seeded file is NOT named .env and the
// command is a plain `ls`, so neither trips the GuardToolCall tripwire
// (credential_path / exfiltration_command) — proving the denial comes
// from the Rule-of-Two gate, which revokes egress regardless of the
// specific command once sensitive data is on hand.
func TestBuildLoop_EnvExfilDeniedEndToEnd(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	workspace := t.TempDir()
	secretFile := filepath.Join(workspace, "deploy-notes.txt")
	if err := os.WriteFile(secretFile, []byte("the staging key is AWS_ACCESS_KEY_ID="+fakeLiveAWSKey+"\n"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}

	server := newOpenAIServer(t, nil, []string{
		// Turn 1: read the key-bearing notes file.
		openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"deploy-notes.txt\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		// Turn 2: an innocuous shell command — denied purely because
		// the latch tripped, not because of the command content.
		openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"ls -la\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		// Turn 3: model gives up.
		openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{"content":"blocked"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "ruleoftwo-env-exfil",
		Mode:             "execution",
		Prompt:           "Inspect the workspace",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		// read_file (sensitive-data ingress) + run_command (external
		// comms). A benign dynamic-context entry supplies the
		// untrusted-input leg so the factory auto-arms enforcement.
		Tools:          types.ToolsConfig{BuiltIn: []string{"read_file", "run_command"}},
		DynamicContext: map[string]types.DynamicContextValue{"task": {Value: "review the repository layout"}},
		MaxTurns:       5,
		Timeout:        &timeout,
	}

	var secBuf bytes.Buffer
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()
	loop.Security = security.NewSecurityLogger(&secBuf, config.RunID)

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (block-external is non-fatal)", runTrace.Outcome)
	}
	var sawRead, runCommandDenied bool
	for _, call := range runTrace.ToolCalls {
		if call.Name == "read_file" && call.Success {
			sawRead = true
		}
		if call.Name == "run_command" {
			if call.Success {
				t.Errorf("run_command must be denied after the .env read trips the latch")
			}
			if strings.Contains(call.ErrorReason, "rule_of_two:") {
				runCommandDenied = true
			}
		}
	}
	if !sawRead {
		t.Fatal("expected the read_file call to succeed and surface the key")
	}
	if !runCommandDenied {
		t.Errorf("expected run_command denied with the rule_of_two reason; trace: %+v", runTrace.ToolCalls)
	}
	if !strings.Contains(secBuf.String(), `"event":"permission_denied"`) {
		t.Errorf("expected permission_denied security event, got: %s", secBuf.String())
	}
}

// --- sub-agent regression: ForChildRun under wrappers ---

// childParentRunIDForbid is a Cedar policy that denies run_command for
// any principal carrying parentRunId — i.e. exactly sub-agent runs.
// Mirrors TestPolicyEngine_ForChildRun_PopulatesParentRunID; here it
// pins that the clone survives the factory's wrapper chain.
const childParentRunIDForbid = `forbid (
	principal,
	action == Action::"tool:run_command",
	resource == Tool::"run_command"
) when {
	principal has parentRunId && principal.parentRunId != ""
};`

func buildWrappedPolicyEngine(t *testing.T, monitor ruleoftwo.Monitor) permission.PermissionPolicy {
	t.Helper()
	policyPath := filepath.Join(t.TempDir(), "policy.cedar")
	if err := os.WriteFile(policyPath, []byte(childParentRunIDForbid), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pe, err := permission.New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyPath, Fallback: "allow-all"},
		permission.PolicyEngineEnv{RunID: "run-parent-1", Mode: "execution"},
		func(string) (permission.PermissionPolicy, error) { return permission.NewAllowAll(), nil },
	)
	if err != nil {
		t.Fatalf("permission.New: %v", err)
	}
	gate := permission.NewRuleOfTwoGate(pe, monitor, map[string]bool{"run_command": true, "web_fetch": true}, nil, nil)
	return permission.NewMetricRecorder(gate, observability.NewNoopMetrics(), "policy-engine")
}

// registerStubRunCommand registers a run_command stand-in whose handler
// records execution, so tests can assert a denial happened before
// dispatch.
func registerStubRunCommand(t *testing.T, loop *AgenticLoop, ran *atomic.Int32) {
	t.Helper()
	registry, ok := loop.Tools.(*tool.Registry)
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
	registry.Register(&tool.Tool{
		Name:              "run_command",
		Description:       "stub shell",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			ran.Add(1)
			return "ran", nil
		},
	})
}

// childRunCommandScript scripts a child run: one run_command attempt,
// then a final answer.
func childRunCommandScript() *scriptedProvider {
	return &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "run_command", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
}

// TestSpawnSubAgent_ForChildRunSurvivesWrapperChain is the regression
// for the wave-4 Unwrap fix: with the policy engine wrapped in the
// rule-of-two gate and the metric recorder, the child must still get a
// parentRunId-populated Cedar clone — the pre-fix direct type-assert
// silently skipped the clone under any wrapper, negating the
// subagent-capability-cap starter policy.
func TestSpawnSubAgent_ForChildRunSurvivesWrapperChain(t *testing.T) {
	prov := &mockProvider{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Provider = childRunCommandScript()

	var secBuf bytes.Buffer
	parentLoop.Security = security.NewSecurityLogger(&secBuf, "run-parent-1")

	// Untripped monitor: the gate is present but inert, so the denial
	// observed below can only come from the Cedar parentRunId forbid.
	monitor := ruleoftwo.NewPatternMonitor(true, "block-external", []string{"sensitive_data"}, false)
	parentLoop.Permissions = buildWrappedPolicyEngine(t, monitor)
	parentLoop.RuleOfTwo = monitor

	var ran atomic.Int32
	registerStubRunCommand(t, parentLoop, &ran)

	parentConfig := buildTestConfig()
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "do a subtask with the shell",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("outcome = %q, want success (denial is per-call, not terminal)", result.Outcome)
	}
	if ran.Load() != 0 {
		t.Fatalf("run_command handler executed %d times in the child; the Cedar forbid must deny before dispatch", ran.Load())
	}
	out := secBuf.String()
	if !strings.Contains(out, `"event":"permission_denied"`) {
		t.Fatalf("expected permission_denied event from the child's run_command attempt, got: %s", out)
	}
	if strings.Contains(out, "rule_of_two:") {
		t.Errorf("denial must come from the Cedar clone, not the (untripped) gate: %s", out)
	}
}

// --- redact action ---

// structuredLeakProvider scripts a tool call whose result carries a
// credential in both the text Content and the Structured payload, then
// a turn that echoes what the model received back as its final text so
// the test can inspect the post-redaction message history.
func redactToolProvider() *scriptedProvider {
	return &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "leaky_struct_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
}

func TestLoop_RuleOfTwoRedactRewritesContentAndStructured(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = redactToolProvider()
	registry, ok := loop.Tools.(*tool.Registry)
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
	registry.Register(&tool.Tool{
		Name:        "leaky_struct_tool",
		Description: "leaks a key in text and structured payload",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		StructuredHandler: func(_ context.Context, _ json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{
				Text:       "key is " + fakeLiveAWSKey + " keep this line",
				Structured: json.RawMessage(`{"stdout":"` + fakeLiveAWSKey + `","note":"surrounding"}`),
				Kind:       "command_result",
			}, nil
		},
	})
	loop.RuleOfTwo = ruleoftwo.NewPatternMonitor(true, "redact", []string{"sensitive_data"}, false)

	config := buildTestConfig()
	config.MaxTurns = 4

	var capturedMessages []types.Message
	loop.Provider = &messageCapturingProvider{
		inner:   redactToolProvider(),
		capture: &capturedMessages,
	}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (redact keeps the run alive)", runTrace.Outcome)
	}

	// The second provider call must have seen the redacted tool result:
	// no key in either Content or Structured, surrounding text intact.
	var toolResult *types.ContentBlock
	for i := range capturedMessages {
		for j := range capturedMessages[i].Content {
			if capturedMessages[i].Content[j].Type == "tool_result" {
				toolResult = &capturedMessages[i].Content[j]
			}
		}
	}
	if toolResult == nil {
		t.Fatal("no tool_result block reached the provider on the second turn")
	}
	if strings.Contains(toolResult.Content, fakeLiveAWSKey) {
		t.Errorf("key survived redaction in Content: %q", toolResult.Content)
	}
	if !strings.Contains(toolResult.Content, "keep this line") {
		t.Errorf("surrounding text lost in Content: %q", toolResult.Content)
	}
	if !strings.Contains(toolResult.Content, ruleoftwo.RedactionPlaceholder) {
		t.Errorf("placeholder missing from Content: %q", toolResult.Content)
	}
	if strings.Contains(string(toolResult.Structured), fakeLiveAWSKey) {
		t.Errorf("key survived redaction in Structured: %q", toolResult.Structured)
	}
	if !json.Valid(toolResult.Structured) {
		t.Errorf("redacted Structured is not valid JSON: %q", toolResult.Structured)
	}
	if !strings.Contains(string(toolResult.Structured), "surrounding") {
		t.Errorf("surrounding structured field lost: %q", toolResult.Structured)
	}
}

// TestLoop_RuleOfTwoRedactStructuredInvalidJSONFallback drives the
// fallback branch of redactSensitiveSpans: when scrubbing a Structured
// payload breaks its JSON (the generic_hex_secret span consumes the
// value's closing quote), the field is replaced with a marshalled
// placeholder rather than emitting malformed JSON. json.Marshal keeps
// the output valid regardless of the placeholder's contents.
func TestLoop_RuleOfTwoRedactStructuredInvalidJSONFallback(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.RuleOfTwo = ruleoftwo.NewPatternMonitor(true, "redact", []string{"sensitive_data"}, false)

	const hex = "0123456789abcdef0123456789abcdef"
	messages := []types.Message{
		{Role: "assistant", Content: []types.ContentBlock{{Type: "text", Text: "x"}}},
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "tc_1", Structured: json.RawMessage(`{"note":"password=` + hex + `"}`)},
		}},
	}
	n := loop.redactSensitiveSpans(messages, 1)
	if n == 0 {
		t.Fatal("expected the latch-tier secret to be redacted")
	}
	got := messages[1].Content[0].Structured
	if !json.Valid(got) {
		t.Errorf("fallback must keep Structured valid JSON, got: %q", got)
	}
	if strings.Contains(string(got), hex) {
		t.Errorf("secret survived the fallback: %q", got)
	}
	want, _ := json.Marshal(ruleoftwo.RedactionPlaceholder)
	if string(got) != string(want) {
		t.Errorf("fallback Structured = %q, want the marshalled placeholder %q", got, want)
	}
}

// messageCapturingProvider records the message slice of the LAST Stream
// call so a test can inspect what the model saw after redaction.
type messageCapturingProvider struct {
	inner   *scriptedProvider
	capture *[]types.Message
}

func (p *messageCapturingProvider) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	*p.capture = params.Messages
	return p.inner.Stream(ctx, params)
}

// --- abort action ---

func TestLoop_RuleOfTwoAbortTerminatesOnToolResult(t *testing.T) {
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	loop.Provider = childRunCommandScript() // tool call then end_turn
	registry, ok := loop.Tools.(*tool.Registry)
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
	registry.Register(&tool.Tool{
		Name:        "run_command",
		Description: "leaks a key",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "AWS_ACCESS_KEY_ID=" + fakeLiveAWSKey, nil
		},
	})
	loop.RuleOfTwo = ruleoftwo.NewPatternMonitor(true, "abort", []string{"sensitive_data"}, false)

	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "rule_of_two_violation" {
		t.Errorf("outcome = %q, want rule_of_two_violation", runTrace.Outcome)
	}
	// The abort fires on turn 1's tool_result scan, so exactly one turn
	// (turn 0) completed before termination.
	if runTrace.Turns != 1 {
		t.Errorf("turns = %d, want 1 (abort on the first sensitive tool result)", runTrace.Turns)
	}
	if !strings.Contains(secBuf.String(), `"event":"rule_of_two_triggered"`) {
		t.Errorf("expected rule_of_two_triggered before abort, got: %s", secBuf.String())
	}
}

func TestLoop_RuleOfTwoAbortTurnZeroBeforeProviderCall(t *testing.T) {
	prov := &countingProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.RuleOfTwo = ruleoftwo.NewPatternMonitor(true, "abort", []string{"sensitive_data"}, false)

	config := buildTestConfig()
	config.DynamicContext = map[string]types.DynamicContextValue{
		"record": {Value: "card 4111111111111111 on file"},
	}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "rule_of_two_violation" {
		t.Errorf("outcome = %q, want rule_of_two_violation", runTrace.Outcome)
	}
	if prov.calls != 0 {
		t.Errorf("provider was called %d times; a turn-0 abort must precede the first provider call", prov.calls)
	}
	if runTrace.Turns != 0 {
		t.Errorf("turns = %d, want 0", runTrace.Turns)
	}
}

// TestLoop_RuleOfTwoAbortPreTrippedLatchDoesNotAbort documents WHY the
// validator rejects onDetect:"abort" + sensitiveData:true (Wave-4
// review item 1): a pre-tripped abort monitor never fires the abort,
// because the loop keys abort on the false→true Transition and a
// pre-tripped latch can never transition. The provider IS reached and
// the run completes normally. This is the regression pin for anyone who
// later removes the validator check — the loop alone cannot catch this.
func TestLoop_RuleOfTwoAbortPreTrippedLatchDoesNotAbort(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)
	// staticallySensitive=true → latch starts tripped → no transition.
	loop.RuleOfTwo = ruleoftwo.NewPatternMonitor(true, "abort", []string{"sensitive_data"}, true)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome == "rule_of_two_violation" {
		t.Error("a pre-tripped abort latch must NOT abort (no transition); the validator is what prevents this config")
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (the loop cannot abort a pre-tripped run)", runTrace.Outcome)
	}
	if !loop.RuleOfTwo.Tripped() {
		t.Error("monitor should be pre-tripped")
	}
}

// --- default-flip pin (inverse of the wave-3 dark-ship pin) ---

// TestBuildLoop_DefaultArmedEnforcesBlockExternal pins the wave-4
// behaviour flip: a factory-built run that holds untrusted input AND
// external comms (web_fetch) under the default policy now denies egress
// once sensitive data is observed — the inverse of the wave-3 dark-ship
// pin, where the identical scenario completed with egress intact.
func TestBuildLoop_DefaultArmedEnforcesBlockExternal(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	// Turn 1: model calls web_fetch (untrusted + external). Turn 2:
	// model calls web_fetch again (now denied). Turn 3: final answer.
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com/\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://evil.example/\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{"content":"stopping"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"r3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "ruleoftwo-default-flip",
		Mode:             "research",
		Prompt:           "Investigate example.com",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		// No RuleOfTwo override: the factory auto-arms enforcing
		// block-external because web_fetch makes the run hold both
		// untrusted input and external comms with no static sensitivity.
		MaxTurns: 5,
		Timeout:  &timeout,
	}

	var secBuf bytes.Buffer
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// Capture security events from the loop's own logger (the factory
	// wires stderr; reattach to our buffer for assertion).
	loop.Security = security.NewSecurityLogger(&secBuf, config.RunID)

	// Stub web_fetch so its FIRST call returns a sensitive body, tripping
	// the latch; the gate then denies the SECOND call.
	presenter, ok := loop.Tools.(*tool.Presenter)
	if !ok {
		t.Fatalf("expected *tool.Presenter, got %T", loop.Tools)
	}
	registry, ok := presenter.Unwrap().(*tool.Registry)
	if !ok {
		t.Fatalf("expected *tool.Registry under presenter, got %T", presenter.Unwrap())
	}
	original := registry.Resolve("web_fetch")
	if original == nil {
		t.Fatal("web_fetch must be registered in research mode")
	}
	var fetchCalls atomic.Int32
	registry.Register(&tool.Tool{
		Name:              original.Name,
		Description:       original.Description,
		InputSchema:       original.InputSchema,
		WorkspaceMutating: original.WorkspaceMutating,
		RequiresApproval:  original.RequiresApproval,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			fetchCalls.Add(1)
			return "fetched secret AWS_ACCESS_KEY_ID=" + fakeLiveAWSKey, nil
		},
	})

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("outcome = %q, want success (block-external keeps the run alive locally)", runTrace.Outcome)
	}
	// Exactly one web_fetch handler execution: the first call runs and
	// trips the latch; the second is denied by the gate before dispatch.
	if got := fetchCalls.Load(); got != 1 {
		t.Errorf("web_fetch handler ran %d times; want 1 (second call gated)", got)
	}
	var denied bool
	for _, call := range runTrace.ToolCalls {
		if call.Name == "web_fetch" && !call.Success && strings.Contains(call.ErrorReason, "rule_of_two:") {
			denied = true
		}
	}
	if !denied {
		t.Errorf("expected a web_fetch call denied with the rule_of_two reason; trace: %+v", runTrace.ToolCalls)
	}
	if !strings.Contains(secBuf.String(), `"event":"permission_denied"`) {
		t.Errorf("expected permission_denied security event, got: %s", secBuf.String())
	}
}

// TestSpawnSubAgent_ParentLatchBlocksChildEgress pins the shared-latch
// contract end to end: the parent's tripped monitor, read through the
// shared gate, denies the child's external-communication tools.
func TestSpawnSubAgent_ParentLatchBlocksChildEgress(t *testing.T) {
	prov := &mockProvider{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Provider = childRunCommandScript()

	var secBuf bytes.Buffer
	parentLoop.Security = security.NewSecurityLogger(&secBuf, "run-parent-1")

	monitor := ruleoftwo.NewPatternMonitor(true, "block-external", []string{"sensitive_data"}, false)
	parentLoop.Permissions = permission.NewRuleOfTwoGate(
		permission.NewAllowAll(), monitor,
		map[string]bool{"run_command": true, "web_fetch": true}, nil, nil,
	)
	parentLoop.RuleOfTwo = monitor

	var ran atomic.Int32
	registerStubRunCommand(t, parentLoop, &ran)

	if !monitor.TripFromGuard("g1", "sensitive_data") {
		t.Fatal("setup: monitor must trip")
	}

	parentConfig := buildTestConfig()
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "exfiltrate via the shell",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("outcome = %q, want success (block-external keeps the run alive)", result.Outcome)
	}
	if ran.Load() != 0 {
		t.Fatalf("run_command handler executed %d times under a tripped parent latch", ran.Load())
	}
	out := secBuf.String()
	if !strings.Contains(out, `"event":"permission_denied"`) {
		t.Fatalf("expected permission_denied from the child's egress attempt, got: %s", out)
	}
	if !strings.Contains(out, "rule_of_two:") {
		t.Errorf("denial reason must carry the rule_of_two prefix, got: %s", out)
	}
}
