package context

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func makeMessage(role, text string) types.Message {
	return types.Message{
		Role: role,
		Content: []types.ContentBlock{
			{Type: "text", Text: text},
		},
	}
}

func TestSlidingWindow_NoTruncation(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", "hello"),
		makeMessage("assistant", "hi"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          10000,
		CurrentTokens:      100,
		ReserveForResponse: 2000,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestSlidingWindow_DropsOldest(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 400)),     // ~100 tokens
		makeMessage("assistant", strings.Repeat("b", 400)), // ~100 tokens
		makeMessage("user", strings.Repeat("c", 400)),      // ~100 tokens
		makeMessage("assistant", strings.Repeat("d", 400)), // ~100 tokens
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          300,
		CurrentTokens:      400,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Available = 300 - 100 = 200. Need to drop 200 tokens worth.
	// Should drop first 2 messages (200 tokens), leaving the last 2.
	if len(result) != 2 {
		t.Errorf("expected 2 messages after truncation, got %d", len(result))
	}
	if result[0].Content[0].Text != strings.Repeat("c", 400) {
		t.Error("expected third message to be first after truncation")
	}
}

func TestSlidingWindow_PreservesMinimum(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 4000)),
		makeMessage("assistant", strings.Repeat("b", 4000)),
		makeMessage("user", strings.Repeat("c", 4000)),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          100,
		CurrentTokens:      3000,
		ReserveForResponse: 50,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Even though budget is tiny, we always keep at least 2 messages.
	if len(result) != 2 {
		t.Errorf("expected minimum 2 messages preserved, got %d", len(result))
	}
}

func TestSlidingWindow_ZeroBudget(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", "hello"),
		makeMessage("assistant", "hi"),
		makeMessage("user", "bye"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          100,
		CurrentTokens:      200,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) < minPreservedMessages {
		t.Errorf("expected at least %d messages, got %d", minPreservedMessages, len(result))
	}
}

func TestSlidingWindow_NegativeAvailable(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          50,
		CurrentTokens:      500,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != minPreservedMessages {
		t.Errorf("expected %d messages with negative budget, got %d", minPreservedMessages, len(result))
	}
}

func TestSlidingWindow_SingleMessage(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", "only one"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          10,
		CurrentTokens:      500,
		ReserveForResponse: 5,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Can't drop below total messages when total < minPreserved.
	if len(result) != 1 {
		t.Errorf("expected 1 message, got %d", len(result))
	}
}

func TestSlidingWindow_ToolUseMessages(t *testing.T) {
	s := NewSlidingWindowStrategy()

	toolInput, _ := json.Marshal(map[string]string{"path": "main.go"})
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("x", 800)),
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "read_file", Input: toolInput},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: strings.Repeat("y", 800)},
			},
		},
		makeMessage("user", "now fix it"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          500,
		CurrentTokens:      600,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Should have dropped some messages but preserved at least 2.
	if len(result) < minPreservedMessages {
		t.Errorf("expected at least %d messages, got %d", minPreservedMessages, len(result))
	}
}

func TestSlidingWindow_ImplementsInterface(t *testing.T) {
	var _ ContextStrategy = (*SlidingWindowStrategy)(nil)
}
