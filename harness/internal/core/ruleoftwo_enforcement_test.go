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
