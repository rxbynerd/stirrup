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
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
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

// mockExecutor satisfies the executor.Executor interface for tests.
type mockExecutor struct{}

func (m *mockExecutor) ReadFile(_ context.Context, path string) (string, error) {
	return "file content of " + path, nil
}
func (m *mockExecutor) WriteFile(_ context.Context, _ string, _ string) error { return nil }
func (m *mockExecutor) ListDirectory(_ context.Context, _ string) ([]string, error) {
	return []string{"a.go", "b.go"}, nil
}
func (m *mockExecutor) Exec(_ context.Context, _ string, _ interface{}) (interface{}, error) {
	return nil, nil
}
func (m *mockExecutor) ResolvePath(p string) (string, error) { return "/workspace/" + p, nil }
func (m *mockExecutor) Capabilities() interface{}            { return nil }

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
			ID:    "toolu_1234567890", // 16 chars → 4 tokens
			Name:  "read_file",        // 9 chars → 2 tokens
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
		Name:        "read_file",                                        // 9 → 2
		Description: "Reads a file from the filesystem",                 // 34 → 8
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
