package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// overflowStep scripts one provider call: an error from Stream, or the
// events streamed back.
type overflowStep struct {
	err    error
	events []types.StreamEvent
}

// overflowScriptProvider replays one step per call and records every
// request; calls past the script end the run.
type overflowScriptProvider struct {
	mu       sync.Mutex
	script   []overflowStep
	requests []types.StreamParams
}

func (p *overflowScriptProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	params.Messages = append([]types.Message(nil), params.Messages...)
	p.requests = append(p.requests, params)
	step := overflowStep{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}
	if i := len(p.requests) - 1; i < len(p.script) {
		step = p.script[i]
	}
	if step.err != nil {
		return nil, step.err
	}
	ch := make(chan types.StreamEvent, len(step.events))
	for _, e := range step.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// budgetRecordingStrategy wraps a strategy and records every budget it is
// asked to prepare for.
type budgetRecordingStrategy struct {
	inner   contextpkg.ContextStrategy
	budgets []contextpkg.TokenBudget
}

func (s *budgetRecordingStrategy) Prepare(ctx context.Context, messages []types.Message, budget contextpkg.TokenBudget) ([]types.Message, error) {
	s.budgets = append(s.budgets, budget)
	return s.inner.Prepare(ctx, messages, budget)
}

func (s *budgetRecordingStrategy) LastCompaction() *contextpkg.CompactionEvent {
	return s.inner.LastCompaction()
}

func overflowToolTurn(i int) overflowStep {
	return overflowStep{events: []types.StreamEvent{
		{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "test_tool", Input: map[string]any{"n": i}},
		{Type: "message_complete", StopReason: "tool_use"},
	}}
}

func promptTooLong() error {
	return types.MarkContextOverflow(errors.New(`anthropic API returned status 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`))
}

// overflowStepsByKind returns the step that overflows the context window
// through each path a provider reports it on.
func overflowStepsByKind() map[string]overflowStep {
	return map[string]overflowStep{
		"rejected request": {err: promptTooLong()},
		"stream error event": {events: []types.StreamEvent{
			{Type: "error", Error: types.MarkContextOverflow(errors.New("openai responses API: Your input exceeds the context window of this model. (code: context_length_exceeded)"))},
		}},
		"context window stop": {events: []types.StreamEvent{
			{Type: "text_delta", Text: "TRUNCATED-PARTIAL"},
			{Type: "message_complete", StopReason: types.StopReasonContextWindowExceeded, InputTokens: 2_000, OutputTokens: 40},
		}},
	}
}

// buildOverflowLoop returns a loop whose three tool turns build a history
// worth compacting before the scripted fourth call.
func buildOverflowLoop(t *testing.T, fourth ...overflowStep) (*AgenticLoop, *overflowScriptProvider, *budgetRecordingStrategy, *bytes.Buffer) {
	t.Helper()
	script := []overflowStep{overflowToolTurn(0), overflowToolTurn(1), overflowToolTurn(2)}
	prov := &overflowScriptProvider{script: append(script, fourth...)}
	loop := buildTestLoop(nil)
	registerLargeTestTool(t, loop)
	loop.Provider = prov
	strategy := &budgetRecordingStrategy{inner: contextpkg.NewSlidingWindowStrategy()}
	loop.Context = strategy
	var events bytes.Buffer
	loop.Transport = transport.NewStdioTransport(&events, &bytes.Buffer{})
	return loop, prov, strategy, &events
}

// forcedBudgets counts the budgets below the run's normal one, which only
// a forced compaction asks for.
func forcedBudgets(budgets []contextpkg.TokenBudget) int {
	n := 0
	for _, b := range budgets {
		if b.MaxTokens < budgets[0].MaxTokens {
			n++
		}
	}
	return n
}

// checkPairedResults fails when a request carries a tool_result whose
// tool_use is not earlier in the same request, or does not start with
// the task.
func checkPairedResults(t *testing.T, req types.StreamParams, task string) {
	t.Helper()
	require.NotEmpty(t, req.Messages, "request has no messages")
	first := req.Messages[0]
	require.Equal(t, "user", first.Role, "request does not start with a user message: %+v", req.Messages)
	require.Equal(t, task, first.Content[0].Text, "request does not start with the task: %+v", req.Messages)
	calls := map[string]bool{}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case "tool_use":
				calls[block.ID] = true
			case "tool_result":
				assert.Contains(t, calls, block.ToolUseID, "tool_result %s has no earlier tool_use in the request", block.ToolUseID)
			}
		}
	}
}

// TestLoop_ContextOverflowRecoversWithOneForcedCompaction pins that a turn
// whose request exceeds the context window is compacted to at most 70% of
// its estimate and retried once, and that the run then succeeds.
func TestLoop_ContextOverflowRecoversWithOneForcedCompaction(t *testing.T) {
	for kind, step := range overflowStepsByKind() {
		t.Run(kind, func(t *testing.T) {
			loop, prov, strategy, events := buildOverflowLoop(t, step)
			config := buildTestConfig()

			runTrace, err := loop.Run(context.Background(), config)
			require.NoError(t, err)
			require.Equal(t, "success", runTrace.Outcome)
			require.Len(t, prov.requests, 5, "three tool turns, the overflow and its retry")
			require.Equal(t, 1, forcedBudgets(strategy.budgets), "exactly one forced compaction")

			rejected, retried := prov.requests[3], prov.requests[4]
			limit := requestTokens(rejected) * overflowCompactionPercent / 100
			require.LessOrEqual(t, requestTokens(retried), limit,
				"retry must estimate at most %d%% of the rejected request's %d tokens", overflowCompactionPercent, requestTokens(rejected))
			require.Less(t, len(retried.Messages), len(rejected.Messages), "retried request should have fewer messages than the rejected request")
			checkPairedResults(t, retried, config.Prompt)
			for _, msg := range retried.Messages {
				for _, block := range msg.Content {
					require.NotContains(t, block.Text, "TRUNCATED-PARTIAL", "the retried request carries the discarded response")
				}
			}
			require.Contains(t, events.String(), "retrying the turn once", "no warning event announces the retry")
			if kind == "context window stop" {
				out := events.String()
				partial, warning := strings.Index(out, "TRUNCATED-PARTIAL"), strings.Index(out, "streamed output is discarded")
				require.GreaterOrEqual(t, partial, 0, "the discarded output is missing from the transport events:\n%s", out)
				require.GreaterOrEqual(t, warning, partial, "no warning after the discarded output says it was discarded:\n%s", out)
			}
		})
	}
}

// TestLoop_ContextOverflowChargesTheDiscardedResponse pins that a response
// stopped by the context window is charged to the run before the retry.
func TestLoop_ContextOverflowChargesTheDiscardedResponse(t *testing.T) {
	loop, _, _, _ := buildOverflowLoop(t, overflowStepsByKind()["context window stop"])

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, 40, runTrace.TokenUsage.Output, "the discarded response's output tokens")
}

// TestLoop_ContextOverflowTwiceEndsExhausted pins that a retry which
// overflows again ends the run as context_exhausted with a clear error,
// without a third request.
func TestLoop_ContextOverflowTwiceEndsExhausted(t *testing.T) {
	for kind, step := range overflowStepsByKind() {
		t.Run(kind, func(t *testing.T) {
			loop, prov, strategy, events := buildOverflowLoop(t, step, step)

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, err)
			require.Equal(t, "context_exhausted", runTrace.Outcome)
			require.Len(t, prov.requests, 5, "no request after the second overflow")
			require.Equal(t, 1, forcedBudgets(strategy.budgets), "exactly one forced compaction")
			for _, want := range []string{"context window exceeded", "again after it was compacted", "contextStrategy.maxTokens"} {
				require.Contains(t, events.String(), want)
			}
		})
	}
}

// TestLoop_UnrelatedProviderErrorIsNotOverflow pins that a provider error
// without the overflow mark, such as an authentication failure or an
// unrelated 400, ends the run as error without a forced compaction.
func TestLoop_UnrelatedProviderErrorIsNotOverflow(t *testing.T) {
	for name, err := range map[string]error{
		"authentication error": errors.New(`anthropic API returned status 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
		"unrelated 400":        errors.New(`anthropic API returned status 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			loop, prov, strategy, _ := buildOverflowLoop(t, overflowStep{err: err})

			runTrace, runErr := loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, runErr)
			require.Equal(t, "error", runTrace.Outcome)
			require.Len(t, prov.requests, 4, "no retry")
			require.Zero(t, forcedBudgets(strategy.budgets), "no forced compaction")
		})
	}
}

// TestLoop_ContextOverflowWithNothingToCompactSendsNoRetry pins that when
// the forced compaction cannot shrink the request below its target, the
// run ends as context_exhausted without resending it.
func TestLoop_ContextOverflowWithNothingToCompactSendsNoRetry(t *testing.T) {
	prov := &overflowScriptProvider{script: []overflowStep{{err: promptTooLong()}}}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	var events bytes.Buffer
	loop.Transport = transport.NewStdioTransport(&events, &bytes.Buffer{})

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "context_exhausted", runTrace.Outcome)
	require.Len(t, prov.requests, 1, "only the rejected request is sent")
	require.Contains(t, events.String(), "forced compaction target", "transport events should name the forced compaction target")
}

// TestBuildLoop_OpenAIContextOverflowRecoversEndToEnd drives a factory-built
// loop against an OpenAI-compatible server that rejects the fourth request
// with its documented context_length_exceeded error. The run recovers with
// one forced compaction that drops the oldest file read, and succeeds.
func TestBuildLoop_OpenAIContextOverflowRecoversEndToEnd(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	workspace := t.TempDir()
	for i := range 3 {
		content := strings.Repeat(fmt.Sprintf("FILE%d-CONTENT line of a large file\n", i), 500)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%d.txt", i)), []byte(content), 0o600))
	}
	readTurn := func(i int) string {
		return openAIChunk(fmt.Sprintf(`{"id":"r%d","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_%d","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"f%d.txt\"}"}}]},"finish_reason":null}]}`, i, i, i)) +
			openAIChunk(fmt.Sprintf(`{"id":"r%d","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, i)) +
			"data: [DONE]\n\n"
	}
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		switch {
		case n <= 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, readTurn(n-1))
		case n == 4:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, openAIChunk(`{"id":"done","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`)+
				openAIChunk(`{"id":"done","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
				"data: [DONE]\n\n")
		}
	}))
	defer server.Close()

	timeout := 60
	config := &types.RunConfig{
		RunID:            "overflow-e2e",
		Mode:             "execution",
		Prompt:           "Read the three files.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         10,
		Timeout:          &timeout,
	}
	var events bytes.Buffer
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&events, &bytes.Buffer{}))
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome, "events:\n%s", events.String())
	require.Len(t, bodies, 5)
	rejected, retried := bodies[3], bodies[4]
	require.Contains(t, rejected, "FILE0-CONTENT", "the rejected request never carried the oldest file read")
	require.NotContains(t, retried, "FILE0-CONTENT", "the retry still carries the oldest file read")
	require.Contains(t, retried, "FILE2-CONTENT", "the retry lost the latest file read")
	require.Contains(t, retried, "Read the three files.", "the retry lost the task")
	require.Contains(t, events.String(), "retrying the turn once", "no warning event announces the retry")
}

// TestBuildLoop_SummariseUsesTheConfiguredWindow pins that the factory
// hands the summarise strategy the run's context window: with
// contextStrategy.maxTokens below the summary model's registered window,
// the summary call's response cap is a quarter of that window rather than
// the cap for an unknown one.
func TestBuildLoop_SummariseUsesTheConfiguredWindow(t *testing.T) {
	const window, reads = 8000, 12
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	workspace := t.TempDir()
	for i := range reads {
		content := strings.Repeat(fmt.Sprintf("FILE%d line\n", i), 120)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%d.txt", i)), []byte(content), 0o600))
	}
	textReply := func(text string) string {
		return openAIChunk(fmt.Sprintf(`{"id":"t","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)) +
			openAIChunk(`{"id":"t","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n"
	}
	var mu sync.Mutex
	var turns int
	var summaryBodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(string(raw), "You are a precise summariser") {
			summaryBodies = append(summaryBodies, body)
			_, _ = fmt.Fprint(w, textReply("earlier files were read"))
			return
		}
		i := turns
		turns++
		if i >= reads {
			_, _ = fmt.Fprint(w, textReply("done"))
			return
		}
		_, _ = fmt.Fprint(w, openAIChunk(fmt.Sprintf(`{"id":"r%d","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_%d","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"f%d.txt\"}"}}]},"finish_reason":null}]}`, i, i, i))+
			openAIChunk(fmt.Sprintf(`{"id":"r%d","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, i))+
			"data: [DONE]\n\n")
	}))
	defer server.Close()

	timeout := 60
	config := &types.RunConfig{
		RunID:            "summarise-window",
		Mode:             "execution",
		Prompt:           "Read every file.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "summarise", MaxTokens: window},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         reads + 2,
		Timeout:          &timeout,
	}
	var events bytes.Buffer
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&events, &bytes.Buffer{}))
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome, "events:\n%s", events.String())
	require.NotEmpty(t, summaryBodies, "the run never made a summary call")
	for i, body := range summaryBodies {
		t.Run(fmt.Sprintf("summary call %d", i+1), func(t *testing.T) {
			maxTokens := body["max_tokens"]
			if maxTokens == nil {
				maxTokens = body["max_completion_tokens"]
			}
			got, ok := maxTokens.(float64)
			require.True(t, ok, "response cap is not a number: %v", maxTokens)
			require.Equal(t, window/4, int(got), "response cap for the summary call")
		})
	}
}

// TestLoop_RecoveredOverflowIsNotAProviderFailure pins that a rejected
// request the loop recovers from with a forced compaction is reported
// once, as the retry warning, and is neither counted as a provider error
// or tool failure nor recorded as an error turn.
func TestLoop_RecoveredOverflowIsNotAProviderFailure(t *testing.T) {
	for _, kind := range []string{"rejected request", "stream error event"} {
		t.Run(kind, func(t *testing.T) {
			loop, _, _, events := buildOverflowLoop(t, overflowStepsByKind()[kind])
			reader := sdkmetric.NewManualReader()
			metrics, err := observability.NewMetricsForTesting(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
			require.NoError(t, err)
			loop.Metrics = metrics
			recorder := &recordingTraceEmitter{}
			loop.Trace = recorder

			_, err = loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, err)

			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &rm))
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != "stirrup.harness.provider_errors" && m.Name != "stirrup.harness.tool_failures" {
						continue
					}
					for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
						require.Zero(t, dp.Value, "%s counts the recovered rejection", m.Name)
					}
				}
			}

			turns, _ := recorder.snapshot()
			seen := map[int]bool{}
			for _, turn := range turns {
				require.NotEqual(t, "error", turn.StopReason, "turn %d recorded as an error", turn.Turn)
				require.False(t, seen[turn.Turn], "turn %d recorded more than once: %+v", turn.Turn, turns)
				seen[turn.Turn] = true
			}

			out := events.String()
			require.NotContains(t, out, "provider anthropic (claude-sonnet-4-6): ", "the recovered rejection was emitted as a provider failure")
			require.Equal(t, 1, strings.Count(out, "retrying the turn once"), "retry warnings:\n%s", out)
		})
	}
}
