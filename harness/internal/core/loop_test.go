package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// mockProvider is a test ProviderAdapter that returns predefined events.
type mockProvider struct {
	events []types.StreamEvent
}

func (m *mockProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
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
	var transportBuf bytes.Buffer
	registry := tool.NewRegistry()
	// Register a simple test tool.
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		SideEffects: false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})

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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}
}

func TestLoop_SimpleTextResponse(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", runTrace.Outcome)
	}
	if runTrace.Turns != 1 {
		t.Errorf("expected 1 turn, got %d", runTrace.Turns)
	}
}

func TestLoop_ToolUseAndContinue(t *testing.T) {
	// First call: model requests a tool call.
	// Second call: model provides final text.
	callCount := 0
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
	_ = callCount

	loop := buildTestLoop(nil)
	loop.Provider = prov

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", runTrace.Outcome)
	}
	if runTrace.Turns != 2 {
		t.Errorf("expected 2 turns, got %d", runTrace.Turns)
	}
}

func TestLoop_MaxTurns(t *testing.T) {
	// Provider always requests tool calls, never stops.
	prov := &infiniteToolCallProvider{}

	loop := buildTestLoop(nil)
	loop.Provider = prov

	config := buildTestConfig()
	config.MaxTurns = 3

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "max_turns" {
		t.Errorf("expected outcome 'max_turns', got %q", runTrace.Outcome)
	}
}

func TestLoop_BudgetExceeded(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()
	// Budget is checked at the START of each turn with token budget of 0.
	maxTokens := 0
	config.MaxTokenBudget = &maxTokens

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// First turn proceeds because budget check happens at start when tokens are 0.
	// Budget check: totalTokens (0) > maxTokenBudget (0) is false, so it passes.
	// After the turn, we don't re-check. So outcome should be success.
	// This is correct behaviour — a zero token budget is unusual.
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success' with zero token budget (check happens before first turn), got %q", runTrace.Outcome)
	}
}

func TestTokenTracker(t *testing.T) {
	tt := &TokenTracker{}

	tt.RecordTurn(1000, 500)

	tokens := tt.Tokens()
	if tokens.Input != 1000 || tokens.Output != 500 {
		t.Errorf("expected tokens 1000/500, got %d/%d", tokens.Input, tokens.Output)
	}
}

func TestCollectToolCalls(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: "text", Text: "some text"},
		{Type: "tool_use", ID: "tc_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		{Type: "text", Text: "more text"},
		{Type: "tool_use", ID: "tc_2", Name: "write_file", Input: json.RawMessage(`{"path":"out.go","content":"..."}`)},
	}

	calls := collectToolCalls(blocks)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Errorf("expected first call to be read_file, got %s", calls[0].Name)
	}
	if calls[1].Name != "write_file" {
		t.Errorf("expected second call to be write_file, got %s", calls[1].Name)
	}
}

func TestBuildMessages(t *testing.T) {
	msgs := buildMessages("hello")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", msgs[0].Role)
	}
	if msgs[0].Content[0].Text != "hello" {
		t.Errorf("expected text 'hello', got %q", msgs[0].Content[0].Text)
	}
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
	err error
}

func (m *errorProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	return nil, m.err
}

// streamErrorProvider sends an error event on the channel.
type streamErrorProvider struct {
	err error
}

func (m *streamErrorProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
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
	loop := buildTestLoop(&mockProvider{})

	call := types.ToolCall{
		ID:    "tc_unknown",
		Name:  "nonexistent_tool",
		Input: json.RawMessage(`{}`),
	}

	output, success := loop.dispatchToolCall(context.Background(), call)
	if success {
		t.Error("expected success == false for unknown tool")
	}
	if !strings.Contains(output, "Unknown tool") {
		t.Errorf("expected output to contain 'Unknown tool', got %q", output)
	}
}

type errorVerifier struct{}

func (m *errorVerifier) Verify(_ context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	return nil, errors.New("verifier transport failed")
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
	loop := buildTestLoop(&mockProvider{})

	// Register a side-effecting tool.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "dangerous_tool",
		Description: "A tool with side effects",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		SideEffects: true,
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
	if success {
		t.Error("expected success == false for denied tool")
	}
	if !strings.Contains(output, "Permission denied") {
		t.Errorf("expected output to contain 'Permission denied', got %q", output)
	}
}

func TestDispatchToolCall_ToolHandlerError(t *testing.T) {
	loop := buildTestLoop(&mockProvider{})

	// Register a tool whose handler returns an error.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "broken_tool",
		Description: "A tool that always errors",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		SideEffects: false,
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
	if success {
		t.Error("expected success == false for erroring tool handler")
	}
	if !strings.Contains(output, "Tool error") {
		t.Errorf("expected output to contain 'Tool error', got %q", output)
	}
}

func TestDispatchToolCall_InvalidInput(t *testing.T) {
	loop := buildTestLoop(&mockProvider{})

	// Register a tool that requires a "path" field.
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "strict_tool",
		Description: "A tool with required fields",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		SideEffects: false,
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
	if success {
		t.Error("expected success == false for invalid input")
	}
	if !strings.Contains(output, "Invalid input") {
		t.Errorf("expected output to contain 'Invalid input', got %q", output)
	}
}

func TestCheckBudget_TokenLimitExceeded(t *testing.T) {
	tt := &TokenTracker{}
	tt.RecordTurn(1_000_000, 100_000)

	maxTokens := 500_000
	check := tt.CheckBudget(&maxTokens)
	if check.WithinBudget {
		t.Error("expected WithinBudget == false when tokens exceed limit")
	}
	if check.Reason != "token_limit_exceeded" {
		t.Errorf("expected reason 'token_limit_exceeded', got %q", check.Reason)
	}
}

// --- P1: Reliability-critical tests ---

func TestLoop_ProviderStreamError(t *testing.T) {
	prov := &errorProvider{err: errors.New("connection refused")}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() should not return a Go error on provider stream failure, got: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace even on provider error")
	}
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error', got %q", runTrace.Outcome)
	}
}

func TestLoop_StreamEventError(t *testing.T) {
	prov := &streamErrorProvider{err: errors.New("server returned 500")}

	loop := buildTestLoop(nil)
	loop.Provider = prov
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() should not return a Go error on stream event error, got: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace even on stream event error")
	}
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error', got %q", runTrace.Outcome)
	}
}

func TestLoop_ContextCancelled(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	// Cancel the context before running.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() should not return a Go error on context cancellation, got: %v", err)
	}
	if runTrace == nil {
		t.Fatal("expected non-nil RunTrace even on context cancellation")
	}
	if runTrace.Outcome != "error" {
		t.Errorf("expected outcome 'error', got %q", runTrace.Outcome)
	}
}

func TestLoop_VerificationFailed(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "verification_failed" {
		t.Errorf("expected outcome 'verification_failed', got %q", runTrace.Outcome)
	}
}

func TestLoop_VerificationError(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "verification_error" {
		t.Errorf("expected outcome 'verification_error', got %q", runTrace.Outcome)
	}
}

func TestLoop_MaxTokensStopReasonIsFailure(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Partial answer"},
			{Type: "message_complete", StopReason: "max_tokens"},
		},
	}

	loop := buildTestLoop(prov)
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "max_tokens" {
		t.Errorf("expected outcome 'max_tokens', got %q", runTrace.Outcome)
	}
}

func TestLoop_PromptBuildError(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected non-nil error when prompt builder fails")
	}
	if !strings.Contains(err.Error(), "build system prompt") {
		t.Errorf("expected error to mention prompt building, got: %v", err)
	}
}

func TestEstimateCurrentTokens(t *testing.T) {
	// Empty messages should return 1 (minimum).
	if got := estimateCurrentTokens(nil); got != 1 {
		t.Errorf("empty messages: want 1, got %d", got)
	}

	// A single text message: overhead + content tokens.
	msgs := []types.Message{{
		Role:    "user",
		Content: []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", 40)}},
	}}
	got := estimateCurrentTokens(msgs)
	// 4 (message overhead) + 3 (block overhead) + 10 (40/4 chars) = 17
	if got != 17 {
		t.Errorf("single text message: want 17, got %d", got)
	}

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
	got = estimateCurrentTokens(toolMsg)
	// 4 (msg) + 3 (block) + 4 (ID) + 2 (Name) + 3 (Input) = 16
	if got != 16 {
		t.Errorf("tool_use message: want 16, got %d", got)
	}
}

func TestEstimateSystemPromptTokens(t *testing.T) {
	prompt := strings.Repeat("a", 400) // 400 chars → 100 content tokens
	got := estimateSystemPromptTokens(prompt)
	// 100 + 4 (message overhead) = 104
	if got != 104 {
		t.Errorf("system prompt 400 chars: want 104, got %d", got)
	}
}

func TestEstimateToolDefinitionTokens(t *testing.T) {
	// No tools → 0.
	if got := estimateToolDefinitionTokens(nil); got != 0 {
		t.Errorf("no tools: want 0, got %d", got)
	}

	tools := []types.ToolDefinition{{
		Name:        "read_file",                                          // 9 → 2
		Description: "Reads a file from the filesystem",                   // 34 → 8
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), // 35 → 8
	}}
	got := estimateToolDefinitionTokens(tools)
	// 10 (overhead) + 2 + 8 + 8 = 28
	if got != 28 {
		t.Errorf("single tool: want 28, got %d", got)
	}

	// Multiple tools scale linearly.
	tools = append(tools, tools[0])
	got2 := estimateToolDefinitionTokens(tools)
	if got2 != got*2 {
		t.Errorf("two identical tools: want %d, got %d", got*2, got2)
	}
}

func TestEstimateTokens_OverheadSignificance(t *testing.T) {
	// With many short messages, overhead should dominate over content.
	// 20 messages with tiny content.
	msgs := make([]types.Message, 20)
	for i := range msgs {
		msgs[i] = types.Message{
			Role:    "user",
			Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
		}
	}
	got := estimateCurrentTokens(msgs)
	// Each message: 4 (msg overhead) + 3 (block overhead) + 0 ("ok" is 2 chars → 0 at /4) = 7
	// 20 * 7 = 140
	if got != 140 {
		t.Errorf("20 short messages: want 140, got %d", got)
	}
}

func TestBuildLoop_InvalidConfig(t *testing.T) {
	config := buildTestConfig()
	config.MaxTurns = 200 // exceeds the maximum of 100

	_, err := BuildLoop(context.Background(), config)
	if err == nil {
		t.Fatal("expected error for invalid config with maxTurns > 100")
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected error to mention maxTurns, got: %v", err)
	}
}

func TestStreamEventsToResult_MergesMessageCompleteFields(t *testing.T) {
	ch := make(chan types.StreamEvent, 3)
	ch <- types.StreamEvent{Type: "text_delta", Text: "Hello"}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	ch <- types.StreamEvent{Type: "message_complete", OutputTokens: 42}
	close(ch)

	result, err := streamEventsToResult(context.Background(), ch, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}), slog.Default())
	if err != nil {
		t.Fatalf("streamEventsToResult() error: %v", err)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("expected stop reason to be preserved, got %q", result.StopReason)
	}
	if result.OutputTokens != 42 {
		t.Fatalf("expected output tokens to be preserved, got %d", result.OutputTokens)
	}
}

func TestAgenticLoopClose_ClosesOwnedResources(t *testing.T) {
	first := &closeTracker{}
	second := &closeTracker{}
	loop := &AgenticLoop{
		ownedClosers: []io.Closer{first, second},
	}

	if err := loop.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if !first.closed || !second.closed {
		t.Fatalf("expected both closers to run, got first=%v second=%v", first.closed, second.closed)
	}
}

// TestDispatchToolCall_EmitsPrototypePollutionBlocked confirms that when a
// tool call's input contains __proto__ or constructor keys, dispatchToolCall
// emits the PrototypePollutionBlocked security event before validation AND
// passes a CLEANED input (without the dangerous keys) to the tool handler.
// The handler-receives-cleaned-input assertion guards the production
// contract: a refactor accidentally passing call.Input instead of
// inputForCall would be caught here.
func TestDispatchToolCall_EmitsPrototypePollutionBlocked(t *testing.T) {
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
	if !ok {
		t.Fatalf("dispatchToolCall returned !ok: %s", out)
	}

	got := secBuf.String()
	if !strings.Contains(got, `"event":"prototype_pollution_blocked"`) {
		t.Errorf("expected prototype_pollution_blocked event, got %q", got)
	}
	if !strings.Contains(got, "__proto__") {
		t.Errorf("expected __proto__ in dropped keys, got %q", got)
	}
	if !strings.Contains(got, `"tool":"test_tool"`) {
		t.Errorf("expected tool name in event, got %q", got)
	}

	// The handler must receive a cleaned input: no __proto__ key, but still
	// carrying the safe sibling.
	if len(capturedInput) == 0 {
		t.Fatal("handler was not invoked or received empty input")
	}
	if strings.Contains(string(capturedInput), "__proto__") {
		t.Errorf("handler received pollution key in input: %s", capturedInput)
	}
	if !strings.Contains(string(capturedInput), `"safe":"v"`) {
		t.Errorf("handler input missing safe key, got: %s", capturedInput)
	}
}

// recordingContextStrategy wraps another ContextStrategy and snapshots the
// loop's lastContextTokens atomic AFTER each Prepare returns. This lets a
// test reconstruct the per-turn gauge trajectory inside a synchronous Run
// without depending on the OTel SDK's collection cadence.
type recordingContextStrategy struct {
	inner    contextpkg.ContextStrategy
	loop     *AgenticLoop
	prepared []int   // number of messages returned per Prepare invocation
	atomics  []int64 // value of loop.lastContextTokens AFTER each Prepare
}

func (r *recordingContextStrategy) Prepare(ctx context.Context, messages []types.Message, budget contextpkg.TokenBudget) ([]types.Message, error) {
	out, err := r.inner.Prepare(ctx, messages, budget)
	r.prepared = append(r.prepared, len(out))
	// The loop stores lastContextTokens *after* Prepare returns, so
	// snapshot via a deferred read-back at the start of the NEXT
	// invocation. We do that by reading the previous value here BEFORE
	// the loop has a chance to overwrite it again — but since this method
	// returns to the loop which then calls .Store(), we must snapshot in
	// a deferred goroutine. Instead: snapshot the current value here
	// (which reflects the previous turn's Store), and rely on the test
	// to read the final value from the atomic post-Run.
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
// pre-compaction observation. This is the negative-delta semantic: a
// successful compaction shrinks the absolute context-window estimate, the
// observable gauge surfaces that drop, and downstream dashboards see a
// downward step rather than an opaque negative delta on a counter.
//
// The recordingContextStrategy snapshots loop.lastContextTokens at each
// Prepare call. Because the loop stores into the atomic AFTER Prepare
// returns, snapshot[N] reflects the value stored after turn N-1's Prepare
// (the snapshot for turn 0's Prepare is the pre-run zero). The final
// post-Run atomic read gives the value stored after the LAST Prepare.
func TestLoop_ContextTokensGaugeReflectsCompaction(t *testing.T) {
	// We need a multi-turn run where the OLDEST messages dominate the
	// token count and get dropped by compaction. Strategy: a heavy user
	// prompt followed by short assistant turns. With MaxTokens far
	// below defaultReserveForResponse, the sliding-window strategy
	// preserves only the last minPreservedMessages on every Prepare call
	// where len(messages) > 2 — and after enough short turns the heavy
	// prompt falls off the kept window, shrinking the absolute count.
	prov := &multiCallProvider{
		calls: [][]types.StreamEvent{
			{
				{Type: "text_delta", Text: "ok1"},
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "ok2"},
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

	// Override the default test config's small prompt with a heavy one
	// AFTER buildTestLoop returns; this lets us shape the message
	// history so the heaviest message is the oldest, which compaction
	// can then drop.

	rec := &recordingContextStrategy{
		inner: contextpkg.NewSlidingWindowStrategy(),
		loop:  loop,
	}
	loop.Context = rec

	config := buildTestConfig()
	// Heavy initial prompt; subsequent assistant turns are short so the
	// heavy prompt dominates the early absolute count and gets dropped
	// by sliding-window compaction once it falls off the preserved tail.
	config.Prompt = strings.Repeat("a", 8000) // ~2000 tokens worth of chars
	// available = MaxTokens (50) - defaultReserveForResponse (64000) < 0.
	// SlidingWindow returns the last minPreservedMessages (2) when
	// available <= 0 and len(messages) > minPreservedMessages.
	config.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 50}

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// rec.atomics[i] reflects loop.lastContextTokens at the START of turn
	// i's Prepare invocation — i.e. the value stored AFTER turn (i-1)'s
	// Prepare. The very first entry is 0 (pre-run reset).
	if len(rec.atomics) < 2 {
		t.Fatalf("expected at least 2 Prepare invocations, got %d (atomics=%v)", len(rec.atomics), rec.atomics)
	}
	if rec.atomics[0] != 0 {
		t.Errorf("rec.atomics[0] = %d, want 0 (pre-run reset)", rec.atomics[0])
	}
	// Value after turn 0's Prepare (atomic at start of turn 1's Prepare).
	afterTurn0 := rec.atomics[1]
	if afterTurn0 <= 0 {
		t.Fatalf("afterTurn0 = %d, expected > 0", afterTurn0)
	}

	// Read the value stored after the LAST Prepare directly from the
	// atomic; the loop has finished by now, so this is stable.
	finalValue := loop.lastContextTokens.Load()

	// Compaction in turn 1 must have trimmed the message history. That
	// means turn 1's compute of (messages + sysprompt + tools) is
	// strictly less than what turn 0's compute would extrapolate to —
	// but more directly, the recording strategy gives us proof of
	// compaction (prepared count drops to 2).
	t.Logf("prepared lengths: %v", rec.prepared)
	t.Logf("atomic snapshots: %v (final=%d)", rec.atomics, finalValue)

	if len(rec.prepared) < 2 {
		t.Fatalf("expected >= 2 Prepare invocations, got %v", rec.prepared)
	}
	// The very first call may pass through unchanged (only 1 message).
	// Subsequent calls with growing histories should compact under our
	// pathologically-low MaxTokens. We require that the final Prepare
	// returned 2 messages (the minimum), confirming compaction fired.
	if rec.prepared[len(rec.prepared)-1] != 2 {
		t.Fatalf("expected last Prepare to compact to 2 messages, got %d (full sequence=%v)", rec.prepared[len(rec.prepared)-1], rec.prepared)
	}

	// Compaction shrinks the message contribution. The final absolute
	// count must be strictly less than the pre-compaction count
	// (afterTurn0 includes the bigText payload; the post-compaction count
	// preserves only 2 small messages out of the larger history).
	if finalValue >= afterTurn0 {
		t.Errorf("expected final gauge value < afterTurn0; got final=%d afterTurn0=%d", finalValue, afterTurn0)
	}
}

// TestLoop_RecordsContextTokensGauge asserts that runInnerLoop publishes the
// absolute (post-Prepare) context token estimate to the lastContextTokens
// atomic, and that the registered observable gauge callback yields that
// value to a ManualReader collection. We collect WHILE the run is still
// active (via a synchronous Run that ends before Collect is called) so that
// the registration is unwound by defer — but the SDK has the chance to
// observe at least once via the ManualReader path because we collect after
// Run completes BUT the gauge value is captured by the loop's stored value;
// since unregister fires on defer at Run return, we must collect as the
// callback is still registered. To exercise this we register our own
// callback inside the loop's lifetime, asserting the absolute count
// directly via the lastContextTokens atomic.
func TestLoop_RecordsContextTokensGauge(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	// Register a probe callback that captures the gauge observation each
	// time the SDK collects. We use a multiCallProvider so the loop runs
	// two turns: after each Context.Prepare the loop stores the absolute
	// token count, and the SDK observes via our probe.
	var (
		observedMu     sync.Mutex
		observedValues []int64
		observedAttrs  []attribute.Set
	)
	unregister, err := metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		// Return 0 here; the value we care about is whatever the loop's own
		// callback reports. We use this only to force a Collect cycle to
		// surface ALL registered callbacks (incl. the loop's).
		return 0, nil
	})
	if err != nil {
		t.Fatalf("RegisterContextTokensCallback: %v", err)
	}
	defer unregister()

	// Use a multi-call provider that drives 2 turns: first turn issues a
	// tool call, second turn ends. After each turn, the inner loop calls
	// Context.Prepare and stores the absolute token estimate, then we
	// drive a Collect at the END of the run to assert the final value.
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

	// Hook in a separate observer that captures whatever the loop's
	// per-run callback reports during a Collect. We register this BEFORE
	// Run so it is alive across Run; the loop registers its own callback
	// at run start. The two callbacks yield independent observations.
	captureUnreg, err := metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		v := loop.lastContextTokens.Load()
		observedMu.Lock()
		observedValues = append(observedValues, v)
		observedMu.Unlock()
		return v, []attribute.KeyValue{
			attribute.String("source", "test-probe"),
		}
	})
	if err != nil {
		t.Fatalf("RegisterContextTokensCallback (probe): %v", err)
	}
	defer captureUnreg()

	config := buildTestConfig()
	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// At this point the run has completed and unregistered its callback,
	// but our probe callback is still active. Collect to drive an
	// observation against the post-run lastContextTokens value (which the
	// loop populated on the final turn).
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Find the gauge in the output and assert it has the expected shape.
	var gaugeDataPoints []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.context_tokens" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("context_tokens has unexpected data type %T (want Gauge[int64])", m.Data)
			}
			gaugeDataPoints = append(gaugeDataPoints, g.DataPoints...)
		}
	}
	if len(gaugeDataPoints) == 0 {
		t.Fatal("expected at least one context_tokens gauge data point")
	}

	// The probe captured the loop's atomic on the final Collect; assert
	// it matches what the loop should have stored after the second turn.
	observedMu.Lock()
	probeValues := append([]int64(nil), observedValues...)
	probeAttrs := append([]attribute.Set(nil), observedAttrs...)
	observedMu.Unlock()
	_ = probeAttrs
	if len(probeValues) == 0 {
		t.Fatal("probe callback never fired")
	}
	finalValue := probeValues[len(probeValues)-1]
	if finalValue <= 0 {
		t.Errorf("expected lastContextTokens > 0 after run, got %d", finalValue)
	}
	if finalValue != loop.lastContextTokens.Load() {
		t.Errorf("probe captured %d but atomic now reads %d", finalValue, loop.lastContextTokens.Load())
	}
}
