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

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
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
	if len(req.Messages) == 0 || req.Messages[0].Role != "user" || req.Messages[0].Content[0].Text != task {
		t.Fatalf("request does not start with the task: %+v", req.Messages)
	}
	calls := map[string]bool{}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case "tool_use":
				calls[block.ID] = true
			case "tool_result":
				if !calls[block.ToolUseID] {
					t.Errorf("tool_result %s has no earlier tool_use in the request", block.ToolUseID)
				}
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
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if runTrace.Outcome != "success" {
				t.Fatalf("outcome = %q, want success", runTrace.Outcome)
			}
			if len(prov.requests) != 5 {
				t.Fatalf("provider saw %d requests, want 5 (three tool turns, the overflow and its retry)", len(prov.requests))
			}
			if n := forcedBudgets(strategy.budgets); n != 1 {
				t.Errorf("forced compactions = %d, want exactly 1", n)
			}

			rejected, retried := prov.requests[3], prov.requests[4]
			if got, limit := requestTokens(retried), requestTokens(rejected)*overflowCompactionPercent/100; got > limit {
				t.Errorf("retried request estimates %d tokens, want at most %d (%d%% of the rejected %d)",
					got, limit, overflowCompactionPercent, requestTokens(rejected))
			}
			if len(retried.Messages) >= len(rejected.Messages) {
				t.Errorf("retried request has %d messages, want fewer than the rejected %d", len(retried.Messages), len(rejected.Messages))
			}
			checkPairedResults(t, retried, config.Prompt)
			for _, msg := range retried.Messages {
				for _, block := range msg.Content {
					if strings.Contains(block.Text, "TRUNCATED-PARTIAL") {
						t.Error("the retried request carries the discarded response")
					}
				}
			}
			if !strings.Contains(events.String(), "retrying the turn once") {
				t.Errorf("no warning event announces the retry:\n%s", events.String())
			}
		})
	}
}

// TestLoop_ContextOverflowChargesTheDiscardedResponse pins that a response
// stopped by the context window is charged to the run before the retry.
func TestLoop_ContextOverflowChargesTheDiscardedResponse(t *testing.T) {
	loop, _, _, _ := buildOverflowLoop(t, overflowStepsByKind()["context window stop"])

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.TokenUsage.Output != 40 {
		t.Errorf("TokenUsage.Output = %d, want the discarded response's 40", runTrace.TokenUsage.Output)
	}
}

// TestLoop_ContextOverflowTwiceEndsExhausted pins that a retry which
// overflows again ends the run as context_exhausted with a clear error,
// without a third request.
func TestLoop_ContextOverflowTwiceEndsExhausted(t *testing.T) {
	for kind, step := range overflowStepsByKind() {
		t.Run(kind, func(t *testing.T) {
			loop, prov, strategy, events := buildOverflowLoop(t, step, step)

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if runTrace.Outcome != "context_exhausted" {
				t.Fatalf("outcome = %q, want context_exhausted", runTrace.Outcome)
			}
			if len(prov.requests) != 5 {
				t.Fatalf("provider saw %d requests, want 5 (no request after the second overflow)", len(prov.requests))
			}
			if n := forcedBudgets(strategy.budgets); n != 1 {
				t.Errorf("forced compactions = %d, want exactly 1", n)
			}
			for _, want := range []string{"context window exceeded", "again after it was compacted", "contextStrategy.maxTokens"} {
				if !strings.Contains(events.String(), want) {
					t.Errorf("transport events do not mention %q:\n%s", want, events.String())
				}
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
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			if runTrace.Outcome != "error" {
				t.Fatalf("outcome = %q, want error", runTrace.Outcome)
			}
			if len(prov.requests) != 4 {
				t.Errorf("provider saw %d requests, want 4 (no retry)", len(prov.requests))
			}
			if n := forcedBudgets(strategy.budgets); n != 0 {
				t.Errorf("forced compactions = %d, want 0", n)
			}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "context_exhausted" {
		t.Fatalf("outcome = %q, want context_exhausted", runTrace.Outcome)
	}
	if len(prov.requests) != 1 {
		t.Errorf("provider saw %d requests, want only the rejected one", len(prov.requests))
	}
	if !strings.Contains(events.String(), "forced compaction target") {
		t.Errorf("transport events do not name the forced compaction target:\n%s", events.String())
	}
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
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%d.txt", i)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success; events:\n%s", runTrace.Outcome, events.String())
	}
	if len(bodies) != 5 {
		t.Fatalf("server saw %d requests, want 5", len(bodies))
	}
	rejected, retried := bodies[3], bodies[4]
	if !strings.Contains(rejected, "FILE0-CONTENT") || strings.Contains(retried, "FILE0-CONTENT") {
		t.Error("the retry still carries the oldest file read, or the rejected request never did")
	}
	if !strings.Contains(retried, "FILE2-CONTENT") || !strings.Contains(retried, "Read the three files.") {
		t.Error("the retry lost the task or the latest file read")
	}
	if !strings.Contains(events.String(), "retrying the turn once") {
		t.Errorf("no warning event announces the retry:\n%s", events.String())
	}
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
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%d.txt", i)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success; events:\n%s", runTrace.Outcome, events.String())
	}
	if len(summaryBodies) == 0 {
		t.Fatal("the run never made a summary call")
	}
	for i, body := range summaryBodies {
		maxTokens := body["max_tokens"]
		if maxTokens == nil {
			maxTokens = body["max_completion_tokens"]
		}
		if got, ok := maxTokens.(float64); !ok || int(got) != window/4 {
			t.Errorf("summary call %d caps the response at %v tokens, want %d", i+1, maxTokens, window/4)
		}
	}
}
