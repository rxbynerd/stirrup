package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// mockProvider is a test ProviderAdapter that returns predefined events.
// lastParams captures the StreamParams of the most recent Stream call so
// tests can assert request-shape invariants (e.g. the loop's
// Temperature=Float64Ptr(0.1) producer guarantee).
type mockProvider struct {
	events     []types.StreamEvent
	lastParams types.StreamParams
}

func (m *mockProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	m.lastParams = params
	ch := make(chan types.StreamEvent, len(m.events))
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func buildTestConfig() *types.RunConfig {
	timeout := 60
	return &types.RunConfig{
		RunID:            "test-run-1",
		Mode:             "execution",
		Prompt:           "Hello, write a test file.",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: "/tmp"},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         20,
		Timeout:          &timeout,
	}
}

func buildTestLoop(prov *mockProvider) *AgenticLoop {
	return buildTestLoopWithSecurity(prov, nil)
}

// buildTestLoopWithSecurity is identical to buildTestLoop but wires a
// SecurityLogger writing to the provided sink; pass nil to skip.
func buildTestLoopWithSecurity(prov *mockProvider, securitySink io.Writer) *AgenticLoop {
	var transportBuf bytes.Buffer
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

	var secLogger *security.SecurityLogger
	if securitySink != nil {
		secLogger = security.NewSecurityLogger(securitySink, "test-run")
	}

	return &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil, // Not needed for these tests
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&transportBuf, &bytes.Buffer{}),
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Security:    secLogger,
		Logger:      slog.Default(),
	}
}

func TestLoop_SimpleTextResponse_SucceedsInOneTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello! "},
			{Type: "text_delta", Text: "I'm here to help."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	check.Equal("success", runTrace.Outcome)
	check.Equal(1, runTrace.Turns)
}

// TestLoop_ForwardsConfiguredTemperature pins that the loop never silently
// forwards a nil temperature to the provider: unset RunConfig.Temperature
// falls back to the harness default (0.1), a non-zero override is forwarded
// verbatim, and an explicit 0.0 override (greedy decoding) is forwarded as
// 0.0 rather than coerced back to the default.
func TestLoop_ForwardsConfiguredTemperature(t *testing.T) {
	cases := []struct {
		name     string
		override *float64
		want     float64
	}{
		{name: "unset falls back to default", override: nil, want: 0.1},
		{name: "non-zero override forwarded", override: types.Float64Ptr(0.7), want: 0.7},
		{name: "explicit zero override forwarded", override: types.Float64Ptr(0.0), want: 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			prov := &mockProvider{
				events: []types.StreamEvent{
					{Type: "text_delta", Text: "Hi"},
					{Type: "message_complete", StopReason: "end_turn"},
				},
			}
			loop := buildTestLoop(prov)
			config := buildTestConfig()
			config.Temperature = tc.override

			_, err := loop.Run(context.Background(), config)
			must.NoError(err)
			must.NotNil(prov.lastParams.Temperature, "loop must never forward a nil temperature")
			must.InDelta(tc.want, *prov.lastParams.Temperature, 0)
		})
	}
}

// cacheKeyRecorder answers the first Stream call with a tool call and
// every later call with a final answer, recording each call's CacheKey.
type cacheKeyRecorder struct {
	keys []string
}

func (p *cacheKeyRecorder) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.keys = append(p.keys, params.CacheKey)
	events := []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}
	if len(p.keys) == 1 {
		events = []types.StreamEvent{
			{Type: "tool_call", ID: "tc1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		}
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestLoop_CacheKeyIsStableHashOfRunID pins StreamParams.CacheKey: the
// first 32 hex characters of sha256(runID), identical on every turn of a
// run so the provider sees one conversation, distinct between run IDs, and
// empty for a run with no ID so unrelated runs never share a key.
func TestLoop_CacheKeyIsStableHashOfRunID(t *testing.T) {
	cases := []struct {
		name  string
		runID string
		want  string
	}{
		{name: "first run", runID: "test-run-1", want: "404b0dfface497f076048e07aa412671"},
		{name: "second run", runID: "test-run-2", want: "a7dbe4f7807e61746b98d54eda1f237e"},
		{name: "empty run ID", runID: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			prov := &cacheKeyRecorder{}
			loop := buildTestLoop(&mockProvider{})
			loop.Provider = prov
			config := buildTestConfig()
			config.RunID = tc.runID

			_, err := loop.Run(context.Background(), config)
			must.NoError(err)
			must.Len(prov.keys, 2, "Stream calls")
			for turn, got := range prov.keys {
				must.Equal(tc.want, got, "turn %d CacheKey", turn)
			}
		})
	}
}

// TestLoop_ResponsesBodyCarriesCacheKeyNotRunID drives the loop through
// the real Responses adapter and pins that the wire body carries the run's
// cache key as prompt_cache_key and does not carry the raw run ID.
func TestLoop_ResponsesBodyCarriesCacheKeyNotRunID(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	const runID = "run-7f3a-control-plane-id"
	sse := "event: response.output_text.delta\n" +
		`data: {"item_id":"msg_1","output_index":0,"delta":"done"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1"}],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	loop := buildTestLoop(&mockProvider{})
	loop.Provider = provider.NewOpenAIResponsesAdapter(func(context.Context) (string, error) { return "test-key", nil }, srv.URL, provider.OpenAIAuthConfig{})
	loop.Router = router.NewStaticRouter("openai-responses", "gpt-4o")
	config := buildTestConfig()
	config.RunID = runID

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	must.NotEmpty(bodies, "no request reached the Responses endpoint")
	want := `"prompt_cache_key":"` + providerCacheKey(runID) + `"`
	for i, body := range bodies {
		check.Contains(body, want, "request %d", i)
		check.NotContains(body, runID, "request %d carries the raw run ID", i)
	}
}

func TestLoop_DynamicContext_SanitizedBeforePromptBuild(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	capturingPrompt := &capturingPromptBuilder{}
	loop.Prompt = capturingPrompt
	var securityEvents bytes.Buffer
	loop.Security = security.NewSecurityLogger(&securityEvents, "test-run")
	config := buildTestConfig()
	config.DynamicContext = map[string]types.DynamicContextValue{
		"issue": {Value: "<tag>Fix main.go</tag><!-- hidden -->"},
	}

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Equal("Fix main.go", capturingPrompt.last.DynamicContext["issue"])

	event := securityEvents.String()
	must.Contains(event, "dynamic_context_sanitized")
	must.Contains(event, `"key":"issue"`)
	must.NotContains(event, "Fix main.go", "security event must not include context content")
}

// TestLoop_PromptModel_ThreadedIntoPromptContext pins that the loop threads
// the resolved prompt model into PromptContext: the promptModel override
// when set, otherwise the router's model.
func TestLoop_PromptModel_ThreadedIntoPromptContext(t *testing.T) {
	tests := []struct {
		name        string
		promptModel string
		want        string
	}{
		{name: "router model by default", promptModel: "", want: "claude-sonnet-4-6"},
		{name: "promptModel override wins", promptModel: "claude-fable-5", want: "claude-fable-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			prov := &mockProvider{
				events: []types.StreamEvent{
					{Type: "text_delta", Text: "Done"},
					{Type: "message_complete", StopReason: "end_turn"},
				},
			}
			loop := buildTestLoop(prov)
			capturingPrompt := &capturingPromptBuilder{}
			loop.Prompt = capturingPrompt
			config := buildTestConfig()
			config.PromptBuilder.PromptModel = tt.promptModel

			_, err := loop.Run(context.Background(), config)
			must.NoError(err)
			must.Equal(tt.want, capturingPrompt.last.Model)
		})
	}
}

// TestLoop_Executor_ThreadedIntoPromptContext pins that the workspace prompt
// fragments receive the run's executor, so they read the sandbox rather
// than the configured host path.
func TestLoop_Executor_ThreadedIntoPromptContext(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)
	exec := executor.NewNoneExecutor()
	loop.Executor = exec
	capturingPrompt := &capturingPromptBuilder{}
	loop.Prompt = capturingPrompt

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	got, ok := capturingPrompt.last.Executor.(*executor.NoneExecutor)
	must.True(ok, "PromptContext.Executor is %#v", capturingPrompt.last.Executor)
	must.Same(exec, got)
}

func TestLoop_ToolUse_ContinuesUntilEndTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	loop := buildTestLoop(nil)
	loop.Provider = prov

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	check.Equal("success", runTrace.Outcome)
	check.Equal(2, runTrace.Turns)
}

// TestLoop_TurnTrace_CountsToolCalls pins that each TurnTrace carries the
// number of tool calls its turn dispatched.
func TestLoop_TurnTrace_CountsToolCalls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "tool_call", ID: "tc_2", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	rec.mu.Lock()
	turns := append([]types.TurnTrace(nil), rec.turns...)
	rec.mu.Unlock()
	must.Len(turns, 2)
	check.Equal(2, turns[0].ToolCalls, "tool-calling turn")
	check.Equal(0, turns[1].ToolCalls, "final turn")
}

// TestLoop_TurnTrace_IgnoresToolBlocksOnNonToolUseStop pins that a turn which
// stops for a reason other than tool_use reports zero tool calls even when
// the response carries a tool block, because the loop never dispatches it.
func TestLoop_TurnTrace_IgnoresToolBlocksOnNonToolUseStop(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "max_tokens"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	rec.mu.Lock()
	turns := append([]types.TurnTrace(nil), rec.turns...)
	rec.mu.Unlock()
	must.NotEmpty(turns, "turn traces")
	check.Equal(0, turns[0].ToolCalls, "max_tokens turn")
}

// TestLoop_ToolCallEventsPrecedeMatchingToolResults pins that the transport
// sees a tool_call event for every dispatched tool, carrying the id that the
// matching tool_result echoes back as tool_use_id, and that the tool_call
// arrives first. The event is emitted while the provider stream is drained,
// before any dispatch starts.
func TestLoop_ToolCallEventsPrecedeMatchingToolResults(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{"n": 1}},
				{Type: "tool_call", ID: "tc_2", Name: "test_tool", Input: map[string]any{"n": 2}},
				{Type: "tool_call", ID: "tc_3", Name: "test_tool", Input: map[string]any{"n": 3}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	var transportBuf bytes.Buffer
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Transport = transport.NewStdioTransport(&transportBuf, &bytes.Buffer{})

	config := buildTestConfig()
	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	lines := strings.Split(strings.TrimSpace(transportBuf.String()), "\n")
	var events []types.HarnessEvent
	for _, line := range lines {
		var e types.HarnessEvent
		must.NoError(json.Unmarshal([]byte(line), &e), "unmarshal event %q", line)
		events = append(events, e)
	}

	toolCallIdx := map[string]int{}
	toolResultIdx := map[string]int{}
	for i, e := range events {
		switch e.Type {
		case "tool_call":
			toolCallIdx[e.ID] = i
			check.Contains([]string{`{"n":1}`, `{"n":2}`, `{"n":3}`}, string(e.Input), "tool_call %s Input", e.ID)
		case "tool_result":
			toolResultIdx[e.ToolUseID] = i
		}
	}

	wantIDs := []string{"tc_1", "tc_2", "tc_3"}
	must.Len(toolCallIdx, len(wantIDs), "tool_call events: %+v", toolCallIdx)
	for _, id := range wantIDs {
		t.Run(id, func(t *testing.T) {
			must := require.New(t)
			must.Contains(toolCallIdx, id, "missing tool_call event")
			must.Contains(toolResultIdx, id, "missing tool_result event")
			must.Less(toolCallIdx[id], toolResultIdx[id], "tool_call must precede its tool_result")
		})
	}
}

func TestLoop_MaxTurns_StopsWithMaxTurnsOutcome(t *testing.T) {
	must := require.New(t)
	// Provider always requests tool calls, never stops.
	prov := &infiniteToolCallProvider{}

	loop := buildTestLoop(nil)
	loop.Provider = prov

	config := buildTestConfig()
	config.MaxTurns = 3

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("max_turns", runTrace.Outcome)
}

// TestLoop_FinalAssistantText_Populated pins the happy path: a run whose
// final assistant turn carries text lands that text on
// RunTrace.FinalAssistantText.
func TestLoop_FinalAssistantText_Populated(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "The answer "},
			{Type: "text_delta", Text: "is 42."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Equal("The answer is 42.", runTrace.FinalAssistantText)
}

// TestLoop_FinalAssistantText_LastNonEmptyAcrossTurns pins that the loop
// captures the last non-empty assistant text across every turn.
func TestLoop_FinalAssistantText_LastNonEmptyAcrossTurns(t *testing.T) {
	must := require.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: "Let me check."},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "All done."},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Equal("All done.", runTrace.FinalAssistantText)
}

// TestLoop_FinalAssistantText_EmptyWhenNoText pins the omit path: a run
// that never produces an assistant text block leaves FinalAssistantText
// empty so the omitempty tag drops it from the emitted RunResult.
func TestLoop_FinalAssistantText_EmptyWhenNoText(t *testing.T) {
	must := require.New(t)
	prov := &infiniteToolCallProvider{}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()
	config.MaxTurns = 3

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("max_turns", runTrace.Outcome)
	must.Empty(runTrace.FinalAssistantText)
}

// TestLoop_FinalAssistantText_GuardrailBlockedDoesNotLeakDeniedText pins
// the guard-ordering invariant: when the PhasePostTurn guard denies a
// turn's final text, that text must not appear on
// RunTrace.FinalAssistantText.
func TestLoop_FinalAssistantText_GuardrailBlockedDoesNotLeakDeniedText(t *testing.T) {
	must := require.New(t)
	const deniedText = "The secret is ghp_shouldnotleak and here it is."
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: deniedText},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)
	loop.GuardRail = &phaseAwareFakeGuard{
		verdicts: map[guard.Phase]guard.Verdict{
			guard.PhasePreTurn:  guard.VerdictAllow,
			guard.PhasePostTurn: guard.VerdictDeny,
		},
	}
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("guardrail_blocked", runTrace.Outcome)
	must.Empty(runTrace.FinalAssistantText, "denied turn's text must not leak")
}

// TestLoop_FinalAssistantText_GuardrailBlockedReturnsPriorApprovedText is
// the two-turn variant of the guard-ordering invariant: when the PostTurn
// guard denies the final turn's text, the run must surface the prior turn's
// non-denied text, not the denied answer.
func TestLoop_FinalAssistantText_GuardrailBlockedReturnsPriorApprovedText(t *testing.T) {
	must := require.New(t)
	const priorText = "Prior non-denied answer."
	const deniedText = "Denied final answer."
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: priorText},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: deniedText},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.GuardRail = &phaseAwareFakeGuard{
		verdicts: map[guard.Phase]guard.Verdict{
			guard.PhasePreTurn:  guard.VerdictAllow,
			guard.PhasePreTool:  guard.VerdictAllow,
			guard.PhasePostTurn: guard.VerdictDeny,
		},
	}
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("guardrail_blocked", runTrace.Outcome)
	must.Equal(priorText, runTrace.FinalAssistantText, "prior non-denied turn's text")
	must.NotContains(runTrace.FinalAssistantText, deniedText, "denied turn's text must not leak")
}

// TestLoop_FinalAssistantText_NotClobberedByEmptyLaterTurn pins that a
// terminal turn carrying no text blocks must not blank out an earlier
// turn's text. Distinct from LastNonEmptyAcrossTurns, which covers a later
// non-empty turn overwriting an earlier one.
func TestLoop_FinalAssistantText_NotClobberedByEmptyLaterTurn(t *testing.T) {
	must := require.New(t)
	const text0 = "Here is the result."
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: text0},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Equal(text0, runTrace.FinalAssistantText, "earlier text retained across empty end_turn")
}

// TestLoop_FinalAssistantText_ReachesPersistedJSONLTrace proves the field
// reaches the persisted trace, not merely the returned struct.
func TestLoop_FinalAssistantText_ReachesPersistedJSONLTrace(t *testing.T) {
	must := require.New(t)
	const answer = "The persisted answer."
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: answer},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	var buf bytes.Buffer
	loop := buildTestLoop(prov)
	loop.Trace = trace.NewJSONLTraceEmitter(&buf, false)
	config := buildTestConfig()

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	var finished []trace.Event
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev trace.Event
		must.NoError(json.Unmarshal(line, &ev), "unmarshal trace line")
		if ev.Kind == trace.EventKindRunFinished {
			finished = append(finished, ev)
		}
	}
	must.NotEmpty(finished, "no run_finished event found in JSONL output")
	for _, ev := range finished {
		must.NotNil(ev.Trace, "run_finished event carries nil Trace")
		must.Equal(answer, ev.Trace.FinalAssistantText, "persisted FinalAssistantText")
	}
}

// TestLoop_FinalAssistantText_ScrubbedBeforePersist pins that the final
// assistant text is passed through security.Scrub before it reaches the
// trace/resultSink.
func TestLoop_FinalAssistantText_ScrubbedBeforePersist(t *testing.T) {
	must := require.New(t)
	// ghp_ GitHub PAT shape is a known deterministic scrub pattern.
	const secret = "ghp_0123456789abcdefghijABCDEFGHIJ0123"
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Your token is " + secret + " keep it safe."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.NotContains(runTrace.FinalAssistantText, secret, "raw secret must not persist")
	must.NotContains(runTrace.FinalAssistantText, "ghp_", "raw token prefix must not persist")
	must.Contains(runTrace.FinalAssistantText, "[REDACTED]", "want scrubbed marker")
}

func TestLoop_ZeroTokenBudget_FirstTurnProceeds(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	maxTokens := 0
	config.MaxTokenBudget = &maxTokens

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	// Budget is checked at the start of each turn; totalTokens(0) >
	// maxTokenBudget(0) is false, so the first turn proceeds and there is
	// no re-check afterward.
	must.Equal("success", runTrace.Outcome, "check happens before the first turn")
}

func TestTokenTracker_RecordTurn_ReportsUsage(t *testing.T) {
	must := require.New(t)
	tt := &TokenTracker{}

	tt.RecordTurn(types.TokenUsage{Input: 1000, Output: 500})

	tokens := tt.Tokens()
	must.Equal(1000, tokens.Input)
	must.Equal(500, tokens.Output)
}

func TestCollectToolCalls_ReturnsToolUseBlocksInOrder(t *testing.T) {
	must := require.New(t)
	blocks := []types.ContentBlock{
		{Type: "text", Text: "some text"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		{Type: "text", Text: "more text"},
		{Type: "tool_use", ID: "tc_2", Name: "write_file", Input: json.RawMessage(`{"path":"out.go","content":"..."}`)},
	}

	calls := collectToolCalls(blocks, nil)
	must.Len(calls, 2)
	must.Equal("read_file", calls[0].Name)
	must.Equal("write_file", calls[1].Name)
}

func TestBuildMessages_WrapsPromptAsUserMessage(t *testing.T) {
	must := require.New(t)
	msgs := buildMessages("hello")
	must.Len(msgs, 1)
	must.Equal("user", msgs[0].Role)
	must.Equal("hello", msgs[0].Content[0].Text)
}

// multiCallProvider returns different event sequences for successive calls.
type multiCallProvider struct {
	calls [][]types.StreamEvent
	idx   int
}

func (m *multiCallProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	if m.idx >= len(m.calls) {
		// Default: return end_turn.
		ch := make(chan types.StreamEvent, 1)
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
		close(ch)
		return ch, nil
	}
	events := m.calls[m.idx]
	m.idx++
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// infiniteToolCallProvider always returns a tool call, never end_turn.
// Each call uses a unique input to avoid triggering the stall detector.
type infiniteToolCallProvider struct {
	callCount int
}

func (m *infiniteToolCallProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	m.callCount++
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{
		Type:  "tool_call",
		ID:    fmt.Sprintf("tc_inf_%d", m.callCount),
		Name:  "test_tool",
		Input: map[string]any{"turn": m.callCount},
	}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)
	return ch, nil
}

// errorProvider is a provider whose Stream() always returns an error.
type errorProvider struct {
	err   error
	calls int
}

func (m *errorProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	m.calls++
	return nil, m.err
}

// streamErrorProvider sends an error event on the channel.
type streamErrorProvider struct {
	err   error
	calls int
}

func (m *streamErrorProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	m.calls++
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: "error", Error: m.err}
	close(ch)
	return ch, nil
}

// failingPromptBuilder always returns an error from Build.
type failingPromptBuilder struct {
	err error
}

func (m *failingPromptBuilder) Build(_ context.Context, _ prompt.PromptContext) (string, error) {
	return "", m.err
}

type capturingPromptBuilder struct {
	last prompt.PromptContext
}

func (b *capturingPromptBuilder) Build(_ context.Context, pc prompt.PromptContext) (string, error) {
	b.last = pc
	return "system prompt", nil
}

// failingVerifier always returns Passed: false.
type failingVerifier struct{}

func (m *failingVerifier) Verify(_ context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	return &types.VerificationResult{
		Passed:   false,
		Feedback: "verification check failed",
	}, nil
}

// --- P0: Security-critical tests ---

func TestDispatchToolCall_UnknownTool(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})

	call := types.ToolCall{
		ID:    "tc_unknown",
		Name:  "nonexistent_tool",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "Unknown tool")
}

// TestDispatchToolCall_LegacySearchFilesEmitsHint pins the migration error
// emitted when a model (or a stale operator config) still calls the
// legacy search_files tool: the dispatcher must direct the caller to the
// two replacement tools rather than emitting an opaque "Unknown tool".
func TestDispatchToolCall_LegacySearchFilesEmitsHint(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})

	call := types.ToolCall{
		ID:    "tc_legacy",
		Name:  "search_files",
		Input: json.RawMessage(`{"pattern":"foo","type":"grep"}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "search_files")
	must.Contains(output, "grep_files")
	must.Contains(output, "find_files")
	// The parenthetical suffixes are load-bearing model-guidance text; a
	// silent reword that drops either descriptor would degrade
	// self-correction without failing the name-only checks above.
	must.Contains(output, "regex content search", "grep_files must be described as regex content search")
	must.Contains(output, "glob filename search", "find_files must be described as glob filename search")
}

// TestDispatchToolCall_CaseMismatchNamesToolWithoutRunningIt pins that a
// call differing from a registered tool only in letter case is rejected
// with the correct name, and the tool never runs under the wrong name.
func TestDispatchToolCall_CaseMismatchNamesToolWithoutRunningIt(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})
	ran := false
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			ran = true
			return "contents", nil
		},
	})
	loop.Tools = registry

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_case",
		Name:  "Read_File",
		Input: json.RawMessage(`{}`),
	})

	must.False(success)
	must.False(ran, "the tool must not run under a case-mismatched name")
	must.Equal("Unknown tool: Read_File. Tool names are case-sensitive; call read_file instead.", output)

	call := types.ToolCall{ID: "tc_case", Name: "Read_File", Input: json.RawMessage(`{}`)}
	input, _, category := loop.preflightToolCall(call, loop.Tools.Resolve(call.Name))
	must.Nil(input, "a rejected call yields no input for the guard or permission checks")
	must.Equal(observability.ToolFailureUnknownTool, category)
}

// TestDispatchToolCall_CaseMismatchDoesNotSuggestUnloadedTool pins that the
// hint never names a tool lazy loading has deferred and not yet loaded.
func TestDispatchToolCall_CaseMismatchDoesNotSuggestUnloadedTool(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})
	registry := tool.NewRegistry()
	for _, name := range []string{"read_file", "web_fetch"} {
		registry.Register(&tool.Tool{
			Name:        name,
			Description: "test tool",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
		})
	}
	loop.Tools = registry
	loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}}, tool.LazyLoaderOptions{})
	must.True(loop.LazyTools.Unloaded("web_fetch"), "web_fetch must start deferred for this test to mean anything")

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{ID: "tc_deferred", Name: "WEB_FETCH", Input: json.RawMessage(`{}`)})
	must.False(success)
	must.Equal("Unknown tool: WEB_FETCH", output)

	output, success = loop.dispatchToolCall(context.Background(), types.ToolCall{ID: "tc_eager", Name: "READ_FILE", Input: json.RawMessage(`{}`)})
	must.False(success)
	must.Contains(output, "call read_file instead")
}

func TestCaseFoldedToolName_MatchesOnlyAUniqueCaseVariant(t *testing.T) {
	defs := func(names ...string) []types.ToolDefinition {
		out := make([]types.ToolDefinition, 0, len(names))
		for _, n := range names {
			out = append(out, types.ToolDefinition{Name: n})
		}
		return out
	}
	cases := []struct {
		name   string
		call   string
		tools  []types.ToolDefinition
		want   string
		wantOK bool
	}{
		{"unique case variant", "READ_FILE", defs("read_file", "write_file"), "read_file", true},
		{"no variant", "list_files", defs("read_file"), "", false},
		{"ambiguous variants", "FOO", defs("foo", "Foo"), "", false},
		{"exact name is not a hint", "read_file", defs("read_file"), "", false},
		{"no tools", "read_file", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			got, ok := caseFoldedToolName(tc.call, tc.tools)
			check.Equal(tc.wantOK, ok)
			check.Equal(tc.want, got)
		})
	}
}

// TestRenamedToolHint exercises the rename table directly so the wording
// of each entry is contracted independently of the dispatcher. The
// dispatcher test above covers the wiring; this one pins the hint
// strings themselves.
func TestRenamedToolHint_WordingAndUnknownNames(t *testing.T) {
	must := require.New(t)
	want := "tool not found: search_files; use grep_files (regex content search) or find_files (glob filename search)"
	got, ok := renamedToolHint("search_files")
	must.True(ok, "renamedToolHint must recognise search_files")
	must.Equal(want, got)

	// Unrecognised names must return ok=false so the caller falls through
	// to the generic unknown-tool path.
	_, ok = renamedToolHint("not_a_tool")
	must.False(ok)
}

type errorVerifier struct{}

func (m *errorVerifier) Verify(_ context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	return nil, errors.New("verifier transport failed")
}

type trackingPermission struct {
	called bool
}

func (p *trackingPermission) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*permission.PermissionResult, error) {
	p.called = true
	return &permission.PermissionResult{Allowed: true}, nil
}

type closeTracker struct {
	closed bool
	err    error
}

func (c *closeTracker) Close() error {
	c.closed = true
	return c.err
}

func TestDispatchToolCall_PermissionDenied(t *testing.T) {
	must := require.New(t)
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(&mockProvider{}, &secBuf)

	// Register a workspace-mutating tool.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "dangerous_tool",
		Description:       "A tool that mutates the workspace",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		RequiresApproval:  true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "should not reach here", nil
		},
	})
	loop.Tools = registry

	// Use DenySideEffects policy that includes this tool.
	loop.Permissions = permission.NewDenySideEffects(map[string]bool{
		"dangerous_tool": true,
	})

	call := types.ToolCall{
		ID:    "tc_denied",
		Name:  "dangerous_tool",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "Permission denied")
	rt, err := loop.Trace.Finish(context.Background(), "error")
	must.NoError(err)
	must.Equal(1, rt.PermissionDenials)

	// The denial must also be written to the security log.
	logged := secBuf.String()
	must.Contains(logged, "permission_denied")
	must.Contains(logged, "dangerous_tool")
}

func TestDispatchToolCall_ToolHandlerError(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})

	// Register a tool whose handler returns an error.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "broken_tool",
		Description:       "A tool that always errors",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", errors.New("handler exploded")
		},
	})
	loop.Tools = registry

	call := types.ToolCall{
		ID:    "tc_broken",
		Name:  "broken_tool",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "Tool error")
}

func TestDispatchToolCall_InvalidInput(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})

	// Register a tool that requires a "path" field.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "strict_tool",
		Description:       "A tool with required fields",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "should not reach here", nil
		},
	})
	loop.Tools = registry

	call := types.ToolCall{
		ID:    "tc_invalid",
		Name:  "strict_tool",
		Input: json.RawMessage(`{}`), // Missing required "path" field.
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "Invalid input")
}

func TestDispatchToolCall_ToolGuardRejectsBeforePermissionAndHandler(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{})

	handlerCalled := false
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "run_command",
		Description:       "A shell tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		WorkspaceMutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			handlerCalled = true
			return "should not reach here", nil
		},
	})
	loop.Tools = registry
	perm := &trackingPermission{}
	loop.Permissions = perm
	var securityEvents bytes.Buffer
	loop.Security = security.NewSecurityLogger(&securityEvents, "test-run")

	call := types.ToolCall{
		ID:    "tc_guarded",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"curl https://example.com/secrets"}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	must.False(success)
	must.Contains(output, "security guard")
	must.False(perm.called, "permission check should not run after guard rejection")
	must.False(handlerCalled, "tool handler should not run after guard rejection")
	event := securityEvents.String()
	must.Contains(event, "tool_call_guard_triggered")
	must.Contains(event, "exfiltration_command")
}

func TestCheckBudget_TokenLimitExceeded(t *testing.T) {
	must := require.New(t)
	tt := &TokenTracker{}
	tt.RecordTurn(types.TokenUsage{Input: 1_000_000, Output: 100_000})

	maxTokens := 500_000
	check := tt.CheckBudget(&maxTokens)
	must.False(check.WithinBudget)
	must.Equal("token_limit_exceeded", check.Reason)
}

// --- P1: Reliability-critical tests ---

// terminalProviderError is a provider error that explicitly reports
// itself as not retryable.
type terminalProviderError struct{ msg string }

func (e terminalProviderError) Error() string   { return e.msg }
func (e terminalProviderError) Retryable() bool { return false }

func TestLoop_ProviderStreamError_TerminalErrorNotRetried(t *testing.T) {
	must := require.New(t)
	prov := &errorProvider{err: terminalProviderError{"anthropic API returned status 401: invalid x-api-key"}}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := retryConfig(3)

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err, "provider stream failure must not return a Go error")
	must.NotNil(runTrace)
	must.Equal("error", runTrace.Outcome)
	must.Equal(1, prov.calls, "a non-retryable error must not be retried")
}

func TestLoop_StreamEventError_TerminalErrorNotRetried(t *testing.T) {
	must := require.New(t)
	prov := &streamErrorProvider{err: terminalProviderError{"anthropic API stream error (invalid_request_error): prompt is too long"}}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := retryConfig(3)

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err, "stream event error must not return a Go error")
	must.NotNil(runTrace)
	must.Equal("error", runTrace.Outcome)
	must.Equal(1, prov.calls, "a non-retryable error must not be retried")
}

// TestLoop_ProviderError_SurfacesViaTransport guards against the loop
// swallowing provider/stream errors into OTel spans only; the error string
// must also be reachable on the transport stream and the slog log output.
func TestLoop_ProviderError_SurfacesViaTransport(t *testing.T) {
	cases := []struct {
		name   string
		prov   provider.ProviderAdapter
		needle string
	}{
		{
			name:   "stream-init",
			prov:   &errorProvider{err: errors.New("ValidationException: bad model id")},
			needle: "ValidationException: bad model id",
		},
		{
			name:   "stream-event",
			prov:   &streamErrorProvider{err: errors.New("upstream 503 retrying")},
			needle: "upstream 503 retrying",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var transportBuf, logBuf bytes.Buffer

			registry := tool.NewRegistry()
			loop := &AgenticLoop{
				Provider:    tc.prov,
				Router:      router.NewStaticRouter("bedrock", "claude-sonnet-4-6"),
				Prompt:      prompt.NewDefaultPromptBuilder(),
				Context:     contextpkg.NewSlidingWindowStrategy(),
				Tools:       registry,
				Edit:        edit.NewWholeFileStrategy(),
				Verifier:    verifier.NewNoneVerifier(),
				Permissions: permission.NewAllowAll(),
				Git:         git.NewNoneGitStrategy(),
				Transport:   transport.NewStdioTransport(&transportBuf, &bytes.Buffer{}),
				Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
				Tracer:      noop.NewTracerProvider().Tracer(""),
				Metrics:     observability.NewNoopMetrics(),
				Logger:      slog.New(slog.NewJSONHandler(&logBuf, nil)),
			}

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			must.Equal("error", runTrace.Outcome)

			must.Contains(transportBuf.String(), tc.needle, "transport stream should carry the error detail")
			must.Contains(transportBuf.String(), `"type":"warning"`, "transport stream should carry a warning event")
			must.Contains(logBuf.String(), tc.needle, "slog output should carry the error detail")
		})
	}
}

func TestLoop_ContextCancelled(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	// With no cause attached this is a plain/signal-style cancel and maps
	// to outcome="cancelled".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runTrace, err := loop.Run(ctx, config)
	must.NoError(err, "cancellation should not surface as a Go error")
	must.NotNil(runTrace, "cancellation should still return a RunTrace")
	must.Equal("cancelled", runTrace.Outcome)
}

func TestLoop_VerificationFailed(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done with the task."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	loop.Verifier = &failingVerifier{}
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("verification_failed", runTrace.Outcome)
}

func TestLoop_VerificationError(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done with the task."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	loop.Verifier = &errorVerifier{}
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("verification_error", runTrace.Outcome)
}

// TestLoop_MaxTokensStopIsFailureOnceRecoveriesAreSpent pins that a
// model which keeps stopping at the output limit ends the run with
// max_tokens after the bounded recoveries.
func TestLoop_MaxTokensStopIsFailureOnceRecoveriesAreSpent(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Partial answer"},
			{Type: "message_complete", StopReason: "max_tokens"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("max_tokens", runTrace.Outcome)
	must.Equal(maxOutputLimitRecoveries+1, runTrace.Turns, "one turn per recovery plus the terminal stop")
}

func TestLoop_PromptBuildError(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	loop.Prompt = &failingPromptBuilder{err: errors.New("template parse failure")}
	config := buildTestConfig()

	_, err := loop.Run(context.Background(), config)
	must.ErrorContains(err, "build system prompt")
}

func TestEstimateCurrentTokens_CountsOverheadAndContent(t *testing.T) {
	must := require.New(t)
	// Empty messages should return 1 (minimum).
	must.Equal(1, estimateCurrentTokens(nil, true), "empty messages")

	// A single text message: overhead + content tokens.
	msgs := []types.Message{{
		Role:    "user",
		Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 40)}},
	}}
	// 4 (message overhead) + 3 (block overhead) + 10 (40/4 chars) = 17
	must.Equal(17, estimateCurrentTokens(msgs, true), "single text message")

	// Tool use block includes ID, Name, and Input metadata.
	toolMsg := []types.Message{{
		Role: "assistant",
		Content: []types.ContentBlock{{
			Type:  "tool_use",
			ID:    "toolu_1234567890",                 // 16 chars → 4 tokens
			Name:  "read_file",                        // 9 chars → 2 tokens
			Input: json.RawMessage(`{"path":"/foo"}`), // 15 chars → 3 tokens
		}},
	}}
	// 4 (msg) + 3 (block) + 4 (ID) + 2 (Name) + 3 (Input) = 16
	must.Equal(16, estimateCurrentTokens(toolMsg, true), "tool_use message")
}

// TestEstimateCurrentTokens_CountsReplayFields pins that replay state
// resent with a message counts toward the estimate: a 40 KB stored
// Responses output array adds 10240 tokens.
func TestEstimateCurrentTokens_CountsReplayFields(t *testing.T) {
	must := require.New(t)
	msg := types.Message{
		Role:    "assistant",
		Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
		ReplayFields: map[string]json.RawMessage{
			"openai_responses.output": json.RawMessage(`["` + strings.Repeat("A", 40*1024-4) + `"]`),
			"openai_responses.origin": json.RawMessage(`"m@abc"`),
		},
	}
	// 4 (msg) + 3 (block) + (40960 + 7) / 4 = 10248
	must.Equal(10248, estimateCurrentTokens([]types.Message{msg}, true), "message with 40 KB replay state")
}

func TestEstimateSystemPromptTokens_AddsMessageOverhead(t *testing.T) {
	must := require.New(t)
	prompt := strings.Repeat("a", 400) // 400 chars → 100 content tokens
	// 100 + 4 (message overhead) = 104
	must.Equal(104, estimateSystemPromptTokens(prompt), "system prompt of 400 chars")
}

func TestEstimateToolDefinitionTokens_ScalesWithToolCount(t *testing.T) {
	must := require.New(t)
	must.Zero(estimateToolDefinitionTokens(nil), "no tools")

	tools := []types.ToolDefinition{{
		Name:        "read_file",                                          // 9 → 2
		Description: "Reads a file from the filesystem",                   // 34 → 8
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), // 35 → 8
	}}
	got := estimateToolDefinitionTokens(tools)
	// 10 (overhead) + 2 + 8 + 8 = 28
	must.Equal(28, got, "single tool")

	// Multiple tools scale linearly.
	tools = append(tools, tools[0])
	must.Equal(got*2, estimateToolDefinitionTokens(tools), "two identical tools")
}

func TestEstimateTokens_PerMessageOverheadDominatesShortMessages(t *testing.T) {
	must := require.New(t)
	// With many short messages, overhead should dominate over content.
	// 20 messages with tiny content.
	msgs := make([]types.Message, 20)
	for i := range msgs {
		msgs[i] = types.Message{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
		}
	}
	// Each message: 4 (msg overhead) + 3 (block overhead) + 0 ("ok" is 2 chars → 0 at /4) = 7
	// 20 * 7 = 140
	must.Equal(140, estimateCurrentTokens(msgs, true), "20 short messages")
}

// TestEffectiveReserveForResponse_ScalesDownAtOrBelowDefault pins the reserve-scaling boundary: any
// maxTokens above defaultReserveForResponse (including the unset/negative
// sentinels) is untouched byte-for-byte. At and below the boundary the
// reserve scales down to smallContextReserveDivisor's fraction of
// maxTokens, floored at 1 token so the provider is never asked for a
// zero-token completion.
func TestEffectiveReserveForResponse_ScalesDownAtOrBelowDefault(t *testing.T) {
	tests := []struct {
		name      string
		maxTokens int
		want      int
	}{
		{"unset (zero) uses the flat default", 0, defaultReserveForResponse},
		{"negative uses the flat default", -1, defaultReserveForResponse},
		{"the assumed default window is untouched", defaultMaxContextTokens, defaultReserveForResponse},
		{"just above the boundary is untouched", defaultReserveForResponse + 1, defaultReserveForResponse},
		{"at the boundary scales down", defaultReserveForResponse, defaultReserveForResponse / smallContextReserveDivisor},
		{"32 KB window scales down", 32768, 32768 / smallContextReserveDivisor},
		{"small local-model window", 8192, 8192 / smallContextReserveDivisor},
		{"sub-divisor budget floors at 1 token, never 0", 2, 1},
		{"single-token budget floors at 1 token", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, effectiveReserveForResponse(tt.maxTokens))
		})
	}
}

// TestLoop_SmallContextBudgetScalesProviderMaxTokens pins that the provider
// Stream() request's MaxTokens (max_completion_tokens on the wire) scales
// down with ContextStrategy.MaxTokens instead of staying hardwired to the
// flat defaultReserveForResponse, which a small-context server cannot
// honour.
func TestLoop_SmallContextBudgetScalesProviderMaxTokens(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	loop := buildTestLoop(prov)

	config := buildTestConfig()
	config.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 32768}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	want := 32768 / smallContextReserveDivisor
	must.Equal(want, prov.lastParams.MaxTokens, "scaled reserve, not the flat %d default", defaultReserveForResponse)
}

// TestLoop_SmallContextBudgetWarnsOnce pins that an operator running a
// small-context config gets an actionable warning (slog + a transport
// "warning" event) instead of silent truncation, exactly once per run
// since maxTokens is constant for the run's life.
func TestLoop_SmallContextBudgetWarnsOnce(t *testing.T) {
	must := require.New(t)
	var transportBuf, logBuf bytes.Buffer

	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Transport = transport.NewStdioTransport(&transportBuf, &bytes.Buffer{})
	loop.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	config := buildTestConfig()
	config.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 32768}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	const needle = "context response reserve scaled down"
	must.Contains(logBuf.String(), needle, "slog should warn about the scaled-down reserve")
	must.Equal(1, strings.Count(logBuf.String(), needle), "warn once across the run's 2 turns")
	must.Contains(transportBuf.String(), `"type":"warning"`, "transport stream should carry a warning event for the scaled-down reserve")
	must.Contains(transportBuf.String(), "32768", "transport warning should name the configured maxTokens")
}

func TestBuildLoop_RejectsMaxTurnsAboveLimit(t *testing.T) {
	must := require.New(t)
	config := buildTestConfig()
	config.MaxTurns = 200 // exceeds the maximum of 100

	_, err := BuildLoop(context.Background(), config)
	must.ErrorContains(err, "maxTurns")
}

// TestStreamEventsToResult_ThoughtSignaturePropagatedToBlock pins the
// in-loop hop that carries a provider's thoughtSignature from the
// StreamEvent channel onto the persisted assistant ContentBlock.
func TestStreamEventsToResult_ThoughtSignaturePropagatedToBlock(t *testing.T) {
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})

	t.Run("keeps a non-empty signature", func(t *testing.T) {
		must := require.New(t)
		ch := make(chan types.StreamEvent, 2)
		ch <- types.StreamEvent{
			Type:             "tool_call",
			ID:               "c1",
			Name:             "fn",
			Input:            map[string]any{},
			ThoughtSignature: "AY89==",
		}
		ch <- types.StreamEvent{Type: "message_complete"}
		close(ch)

		result, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
		must.NoError(err)
		must.Len(result.Blocks, 1)
		must.Equal("tool_use", result.Blocks[0].Type)
		must.Equal("AY89==", result.Blocks[0].ThoughtSignature)
	})

	t.Run("keeps an empty signature empty", func(t *testing.T) {
		must := require.New(t)
		ch := make(chan types.StreamEvent, 2)
		ch <- types.StreamEvent{
			Type:  "tool_call",
			ID:    "c2",
			Name:  "fn",
			Input: map[string]any{},
		}
		ch <- types.StreamEvent{Type: "message_complete"}
		close(ch)

		result, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
		must.NoError(err)
		must.Len(result.Blocks, 1)
		must.Empty(result.Blocks[0].ThoughtSignature, "zero value must not be mutated")
	})
}

// TestStreamEventsToResult_ReplayFieldsStashedAndAttached pins the
// in-loop hop for message-level replay state: the message_complete event's
// ReplayFields must land on the streamResult, and appendAssistantContent
// must attach it to the persisted assistant Message so the next request
// can round-trip it.
func TestStreamEventsToResult_ReplayFieldsStashedAndAttached(t *testing.T) {
	must := require.New(t)
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})

	replay := map[string]json.RawMessage{
		"reasoning_content": json.RawMessage(`"step by step"`),
	}
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: "Hello"}
	ch <- types.StreamEvent{
		Type:         "message_complete",
		StopReason:   "end_turn",
		ReplayFields: replay,
	}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	must.NoError(err)
	must.Equal(`"step by step"`, string(result.ReplayFields["reasoning_content"]), "stream result should stash reasoning_content")

	messages := appendAssistantContent(nil, result.Blocks, result.ReplayFields)
	must.Len(messages, 1)
	must.Equal(`"step by step"`, string(messages[0].ReplayFields["reasoning_content"]), "assistant message should carry reasoning_content")

	// Without replay state the field stays nil, so the message serialises unchanged.
	plain := appendAssistantContent(nil, result.Blocks, nil)
	must.Nil(plain[0].ReplayFields, "nil replay state must stay nil on the message")
}

// TestStreamEventsToResult_EmitsToolCallEvent pins that the "tool_call" case
// emits the same normalised input bytes that are appended to history, so a
// control plane's view of a call's input matches what the provider replays
// on the next turn. A nil Input map exercises NormalizeToolInput's
// null-to-{} rewrite on both paths.
func TestStreamEventsToResult_EmitsToolCallEvent(t *testing.T) {
	must := require.New(t)
	var transportBuf bytes.Buffer
	tp := transport.NewStdioTransport(&transportBuf, &bytes.Buffer{})

	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "tool_call", ID: "tc_1", Name: "read_file", Input: nil}
	ch <- types.StreamEvent{Type: "message_complete"}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	must.NoError(err)
	must.Len(result.Blocks, 1)
	must.Equal("tool_use", result.Blocks[0].Type)

	lines := strings.Split(strings.TrimSpace(transportBuf.String()), "\n")
	must.Len(lines, 1, "expected exactly 1 emitted event")
	var e types.HarnessEvent
	must.NoError(json.Unmarshal([]byte(lines[0]), &e))
	must.Equal("tool_call", e.Type)
	must.Equal("tc_1", e.ID)
	must.Equal("read_file", e.Name)
	must.Equal(string(result.Blocks[0].Input), string(e.Input), "emitted input should match the history block input byte-for-byte")
	must.Equal("{}", string(e.Input), "nil input should normalise to {}")
}

// TestStreamEventsToResult_ToolCallEventInputDoesNotAliasHistory guards
// against the emitted event and the history ContentBlock sharing one
// backing array. A Transport implementation is free to redact or mutate
// its event.Input in place (transport.Transport is exported publicly as
// harnessapi.Transport); if the two shared a buffer, that mutation would
// silently rewrite the assistant turn the harness replays to the provider.
// Uses asyncTestTransport, which stores the raw types.HarnessEvent rather
// than a JSON round-trip, so a shared backing array would survive into the
// assertion below undetected by any JSON-level test.
func TestStreamEventsToResult_ToolCallEventInputDoesNotAliasHistory(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()

	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "tool_call", ID: "tc_1", Name: "read_file", Input: map[string]any{"path": "x"}}
	ch <- types.StreamEvent{Type: "message_complete"}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, tr, true, ignoreEmitFailure, nil)
	must.NoError(err)

	events := tr.Events()
	must.Len(events, 1)
	eventInput := events[0].Input
	historyInput := result.Blocks[0].Input

	must.NotSame(&eventInput[0], &historyInput[0], "emitted event Input and history block Input should not share a backing array")

	eventInput[0] = '!'
	must.NotEqual(byte('!'), historyInput[0], "mutating the emitted event's Input should not mutate the history block's Input")
}

func TestStreamEventsToResult_MergesMessageCompleteFields(t *testing.T) {
	must := require.New(t)
	ch := make(chan types.StreamEvent, 3)
	ch <- types.StreamEvent{Type: "text_delta", Text: "Hello"}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	ch <- types.StreamEvent{Type: "message_complete", OutputTokens: 42}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}), true, ignoreEmitFailure, nil)
	must.NoError(err)
	must.Equal("end_turn", result.StopReason, "stop reason should be preserved")
	must.Equal(42, result.Usage.Output, "output tokens should be preserved")
}

func TestAgenticLoopClose_ClosesOwnedResources(t *testing.T) {
	must := require.New(t)
	first := &closeTracker{}
	second := &closeTracker{}
	loop := &AgenticLoop{
		ownedClosers: []io.Closer{first, second},
	}

	must.NoError(loop.Close())
	must.True(first.closed, "first owned closer should run")
	must.True(second.closed, "second owned closer should run")
}

// TestDispatchToolCall_EmitsPrototypePollutionBlocked confirms that when a
// tool call's input contains __proto__ or constructor keys, dispatchToolCall
// emits the PrototypePollutionBlocked security event before validation AND
// passes a cleaned input (without the dangerous keys) to the tool handler.
func TestDispatchToolCall_EmitsPrototypePollutionBlocked(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()

	var capturedInput json.RawMessage
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "test",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			// Copy so the slice the loop owns is not aliased.
			capturedInput = append(json.RawMessage(nil), input...)
			return "ok", nil
		},
	})

	var secBuf bytes.Buffer
	secLogger := security.NewSecurityLogger(&secBuf, "run-1")

	loop := &AgenticLoop{
		Tools:       registry,
		Permissions: permission.NewAllowAll(),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
		Security:    secLogger,
	}

	out, ok := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc1",
		Name:  "test_tool",
		Input: json.RawMessage(`{"__proto__":{"admin":true},"safe":"v"}`),
	})
	must.True(ok, "dispatchToolCall should succeed, got output: %s", out)

	got := secBuf.String()
	must.Contains(got, `"event":"prototype_pollution_blocked"`)
	must.Contains(got, "__proto__", "dropped keys should name __proto__")
	must.Contains(got, `"tool":"test_tool"`, "security event should name the tool")

	// The handler must receive a cleaned input: no __proto__ key, but still
	// carrying the safe sibling.
	must.NotEmpty(capturedInput, "handler should be invoked with input")
	must.NotContains(string(capturedInput), "__proto__", "handler should not receive the pollution key")
	must.Contains(string(capturedInput), `"safe":"v"`, "handler input should keep the safe key")
}

// recordingContextStrategy wraps another ContextStrategy and snapshots the
// loop's lastContextTokens atomic AFTER each Prepare returns. This lets a
// test reconstruct the per-turn gauge trajectory inside a synchronous Run
// without depending on the OTel SDK's collection cadence.
type recordingContextStrategy struct {
	inner    contextpkg.ContextStrategy
	loop     *AgenticLoop
	inputs   []int   // number of messages passed in per Prepare invocation
	prepared []int   // number of messages returned per Prepare invocation
	atomics  []int64 // value of loop.lastContextTokens AFTER each Prepare
}

func (r *recordingContextStrategy) Prepare(ctx context.Context, messages []types.Message, budget contextpkg.TokenBudget) ([]types.Message, error) {
	out, err := r.inner.Prepare(ctx, messages, budget)
	r.inputs = append(r.inputs, len(messages))
	r.prepared = append(r.prepared, len(out))
	// The loop stores lastContextTokens *after* Prepare returns, so the
	// value snapshotted here reflects the previous turn's Store; the test
	// reads the final value from the atomic post-Run.
	if r.loop != nil {
		r.atomics = append(r.atomics, r.loop.lastContextTokens.Load())
	}
	return out, err
}

func (r *recordingContextStrategy) LastCompaction() *contextpkg.CompactionEvent {
	return r.inner.LastCompaction()
}

// TestLoop_ContextTokensGaugeReflectsCompaction asserts that when a
// SlidingWindowStrategy compacts the message history, the lastContextTokens
// atomic (which the ContextTokens gauge reads) DECREASES vs the
// pre-compaction observation, rather than surfacing an opaque negative
// delta on a counter.
//
// The recordingContextStrategy snapshots loop.lastContextTokens at each
// Prepare call. Because the loop stores into the atomic AFTER Prepare
// returns, snapshot[N] reflects the value stored after turn N-1's Prepare
// (the snapshot for turn 0's Prepare is the pre-run zero). The final
// post-Run atomic read gives the value stored after the LAST Prepare.
func TestLoop_ContextTokensGaugeReflectsCompaction(t *testing.T) {
	must := require.New(t)
	// A heavy first assistant turn dominates the token count. It fits the
	// budget on its own, but once a second sizeable exchange exists the
	// history does not, and the sliding window drops the heavy turn,
	// keeping only the task and the last exchange.
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: strings.Repeat("a", 4000)},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: strings.Repeat("b", 2400)},
				{Type: "tool_call", ID: "tc_2", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov

	rec := &recordingContextStrategy{
		inner: contextpkg.NewSlidingWindowStrategy(),
		loop:  loop,
	}
	loop.Context = rec

	config := buildTestConfig()
	// effectiveReserveForResponse(2000) = 500 (quartered); the 1500 left
	// hold the ~1000-token heavy turn but not the ~600-token turn after it.
	config.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 2000}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	// rec.atomics[i] reflects loop.lastContextTokens at the start of turn
	// i's Prepare invocation — the value stored after turn (i-1)'s
	// Prepare. The first entry is 0 (pre-run reset).
	must.Len(rec.atomics, 3, "Prepare invocations (atomics=%v)", rec.atomics)
	must.Zero(rec.atomics[0], "gauge before the first Prepare")
	// Value after turn 1's Prepare, which still holds the heavy turn: it
	// is the only exchange, so nothing can be dropped yet.
	afterTurn1 := rec.atomics[2]
	must.Greater(afterTurn1, rec.atomics[1], "heavy turn should raise the gauge above the turn-0 value")

	// Read the value stored after the LAST Prepare directly from the
	// atomic; the loop has finished by now, so this is stable.
	finalValue := loop.lastContextTokens.Load()

	t.Logf("input lengths: %v, prepared lengths: %v", rec.inputs, rec.prepared)
	t.Logf("atomic snapshots: %v (final=%d)", rec.atomics, finalValue)

	// The last Prepare received [task, heavy turn, result, turn, result]
	// and must keep only the task and the last exchange.
	must.Equal(5, rec.inputs[2], "messages passed to the last Prepare")
	must.Equal(3, rec.prepared[2], "messages kept by the last Prepare")

	// Compaction shrinks the message contribution: dropping the heavy
	// turn leaves the final count strictly below the value that held it.
	must.Less(finalValue, afterTurn1, "gauge after compaction should drop below the value that held the heavy turn")
}

// TestLoop_SmallContextBudgetRetainsHistory pins that a small-but-workable
// context budget (32768, typical of local-model windows) must not silently
// drop history: effectiveReserveForResponse scales the reserve down to a
// quarter of maxTokens, leaving a budget that comfortably holds this test's
// modest message history, so nothing gets dropped. Contrast with
// TestLoop_ContextTokensGaugeReflectsCompaction's genuinely-impossible
// budget, which does collapse to the task and the last exchange.
func TestLoop_SmallContextBudgetRetainsHistory(t *testing.T) {
	must := require.New(t)
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: "ok1"},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: strings.Repeat("b", 2400)},
				{Type: "tool_call", ID: "tc_2", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov

	rec := &recordingContextStrategy{
		inner: contextpkg.NewSlidingWindowStrategy(),
		loop:  loop,
	}
	loop.Context = rec

	config := buildTestConfig()
	// A heavy prompt in a realistic small-context-model window.
	config.Prompt = strings.Repeat("a", 8000)
	config.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 32768}

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)

	must.GreaterOrEqual(len(rec.prepared), 3, "Prepare invocations (prepared=%v)", rec.prepared)
	t.Logf("input lengths: %v, prepared lengths: %v", rec.inputs, rec.prepared)

	// A small-but-workable budget must not silently drop real history.
	must.Equal(rec.inputs, rec.prepared, "each Prepare call should keep all of its input messages")

	last := rec.prepared[len(rec.prepared)-1]
	must.Greater(last, 3, "final Prepare should retain more than the task and the last exchange (prepared=%v)", rec.prepared)
}

// TestLoop_RecordsContextTokensGauge asserts that runInnerLoop publishes the
// absolute (post-Prepare) context token estimate to the lastContextTokens
// atomic, and that the registered observable gauge callback yields that
// value to a ManualReader collection. A probe callback registered before
// Run captures the loop's own callback output while it is still alive
// (the loop unregisters its callback via defer at Run return).
func TestLoop_RecordsContextTokensGauge(t *testing.T) {
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	must.NoError(err, "NewMetricsForTesting")

	// Register a probe callback that captures the gauge observation each
	// time the SDK collects. We use a multiCallProvider so the loop runs
	// two turns: after each Context.Prepare the loop stores the absolute
	// token count, and the SDK observes via our probe.
	var (
		observedMu     sync.Mutex
		observedValues []int64
	)
	unregister, err := metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		// Only present to force a Collect cycle that surfaces every
		// registered callback, including the loop's own.
		return 0, nil
	})
	must.NoError(err, "RegisterContextTokensCallback")
	defer unregister()

	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done!"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Metrics = metrics

	// Registered before Run so it stays alive across Run; the loop
	// registers its own independent callback at run start.
	captureUnreg, err := metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		v := loop.lastContextTokens.Load()
		observedMu.Lock()
		observedValues = append(observedValues, v)
		observedMu.Unlock()
		return v, []attribute.KeyValue{
			attribute.String("source", "test-probe"),
		}
	})
	must.NoError(err, "RegisterContextTokensCallback (probe)")
	defer captureUnreg()

	config := buildTestConfig()
	_, err = loop.Run(context.Background(), config)
	must.NoError(err)

	// The run's own callback is now unregistered, but the probe is still
	// active; Collect observes the post-run lastContextTokens value.
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))

	// Find the gauge in the output and assert it has the expected shape.
	var gaugeDataPoints []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.context_tokens" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			must.True(ok, "context_tokens data type %T (want Gauge[int64])", m.Data)
			gaugeDataPoints = append(gaugeDataPoints, g.DataPoints...)
		}
	}
	must.NotEmpty(gaugeDataPoints, "context_tokens gauge data points")

	// The probe captured the loop's atomic on the final Collect; assert
	// it matches what the loop should have stored after the second turn.
	observedMu.Lock()
	probeValues := append([]int64(nil), observedValues...)
	observedMu.Unlock()
	must.NotEmpty(probeValues, "probe callback should fire during Collect")
	finalValue := probeValues[len(probeValues)-1]
	must.Positive(finalValue, "lastContextTokens after run")
	must.Equal(loop.lastContextTokens.Load(), finalValue, "probe capture should match the atomic after run")
}
