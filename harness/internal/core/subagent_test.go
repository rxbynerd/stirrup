package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

func buildSubAgentTestLoop(prov *mockProvider) *AgenticLoop {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		SideEffects: false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})
	// Register a spawn_agent tool to verify it gets filtered for the child.
	registry.Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		SideEffects: true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "should not be called", nil
		},
	})

	return &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}
}

func TestSpawnSubAgent_SimpleTextResponse(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent output here."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do a subtask",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}

	if result.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", result.Outcome)
	}
	if result.Output != "Sub-agent output here." {
		t.Errorf("expected output 'Sub-agent output here.', got %q", result.Output)
	}
	if result.Turns != 1 {
		t.Errorf("expected 1 turn, got %d", result.Turns)
	}
}

func TestSpawnSubAgent_EmptyPromptReturnsError(t *testing.T) {
	prov := &mockProvider{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "",
	})
	if err == nil {
		t.Fatal("SpawnSubAgent() expected error for empty prompt, got nil")
	}
}

func TestSpawnSubAgent_MaxTurnsCapping(t *testing.T) {
	tests := []struct {
		name           string
		requested      int
		parentMax      int
		expectedCapped int
	}{
		{"default when zero", 0, 20, defaultSubAgentMaxTurns},
		{"cap at max sub-agent", 50, 100, maxSubAgentMaxTurns},
		{"cap at parent max", 15, 5, 5},
		{"use requested when within bounds", 8, 20, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxTurns := tt.requested
			if maxTurns <= 0 {
				maxTurns = defaultSubAgentMaxTurns
			}
			if maxTurns > maxSubAgentMaxTurns {
				maxTurns = maxSubAgentMaxTurns
			}
			if maxTurns > tt.parentMax {
				maxTurns = tt.parentMax
			}

			if maxTurns != tt.expectedCapped {
				t.Errorf("expected capped maxTurns %d, got %d", tt.expectedCapped, maxTurns)
			}
		})
	}
}

func TestFilterToolRegistry_ExcludesSpawnAgent(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})
	registry.Register(&tool.Tool{
		Name:        "run_command",
		Description: "Run a command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})

	filtered := filterToolRegistry(registry, "spawn_agent")
	defs := filtered.List()

	if len(defs) != 2 {
		t.Fatalf("expected 2 tools after filtering, got %d", len(defs))
	}

	for _, def := range defs {
		if def.Name == "spawn_agent" {
			t.Error("filtered registry should not contain spawn_agent")
		}
	}

	if filtered.Resolve("spawn_agent") != nil {
		t.Error("Resolve(\"spawn_agent\") should return nil in filtered registry")
	}
	if filtered.Resolve("read_file") == nil {
		t.Error("Resolve(\"read_file\") should return non-nil in filtered registry")
	}
	if filtered.Resolve("run_command") == nil {
		t.Error("Resolve(\"run_command\") should return non-nil in filtered registry")
	}
}

func TestSpawnSubAgent_InheritsParentMode(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	parentLoop := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()
	parentConfig.Mode = "execution"

	// When mode is empty, should inherit parent's mode.
	result, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{
		Prompt: "Do something",
		Mode:   "",
	})
	if err != nil {
		t.Fatalf("SpawnSubAgent() error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", result.Outcome)
	}
}

func TestCaptureTransport_RecordsTextDeltas(t *testing.T) {
	ct := newCaptureTransport()

	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "Hello "})
	_ = ct.Emit(types.HarnessEvent{Type: "text_delta", Text: "world!"})
	_ = ct.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc_1", Content: "result"})
	_ = ct.Emit(types.HarnessEvent{Type: "done", StopReason: "success"})

	text := ct.lastText()
	if text != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", text)
	}
}

func TestCaptureTransport_EmptyWhenNoTextDeltas(t *testing.T) {
	ct := newCaptureTransport()
	_ = ct.Emit(types.HarnessEvent{Type: "done"})

	if text := ct.lastText(); text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}
