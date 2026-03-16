package context

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// mockSummaryProvider is a test double for the SummaryProvider interface.
type mockSummaryProvider struct {
	callCount int
	summary   string
	err       error
}

func (m *mockSummaryProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	m.callCount++

	if m.err != nil {
		return nil, m.err
	}

	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: m.summary}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// errorStreamProvider returns a stream that emits an error event.
type errorStreamProvider struct {
	callCount int
}

func (e *errorStreamProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	e.callCount++
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "error", Error: fmt.Errorf("model overloaded")}
	close(ch)
	return ch, nil
}

func TestSummarise_ImplementsInterface(t *testing.T) {
	var _ ContextStrategy = (*SummariseStrategy)(nil)
}

func TestSummarise_WithinBudget_ReturnsUnchanged(t *testing.T) {
	mock := &mockSummaryProvider{summary: "should not be called"}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := []types.Message{
		makeMessage("user", "hello"),
		makeMessage("assistant", "hi there"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          10000,
		CurrentTokens:      50,
		ReserveForResponse: 2000,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 messages unchanged, got %d", len(result))
	}
	if mock.callCount != 0 {
		t.Errorf("provider should not have been called, but was called %d times", mock.callCount)
	}
}

func TestSummarise_OverBudget_SummarisesOldMessages(t *testing.T) {
	mock := &mockSummaryProvider{summary: "User asked to fix a bug in main.go. Assistant found the issue."}
	s := NewSummariseStrategy(mock, "test-model")

	// Create 10 messages so there's enough to split (6 recent + 4 old).
	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, strings.Repeat("x", 400))
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          500,
		CurrentTokens:      1000,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	if mock.callCount != 1 {
		t.Errorf("expected provider to be called once, got %d", mock.callCount)
	}

	// Result should be: 1 summary message + 6 recent messages = 7.
	if len(result) != 7 {
		t.Errorf("expected 7 messages (1 summary + 6 recent), got %d", len(result))
	}

	// First message should be the summary.
	if result[0].Role != "user" {
		t.Errorf("expected summary message role 'user', got %q", result[0].Role)
	}
	if !strings.Contains(result[0].Content[0].Text, "<conversation_summary>") {
		t.Error("expected summary message to contain <conversation_summary> tag")
	}
	if !strings.Contains(result[0].Content[0].Text, "fix a bug") {
		t.Error("expected summary content to be present")
	}
}

func TestSummarise_RecentMessagesPreservedVerbatim(t *testing.T) {
	mock := &mockSummaryProvider{summary: "Earlier conversation summary."}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, fmt.Sprintf("message-%d", i))
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// The last 6 messages should be preserved exactly.
	for i := 1; i < len(result); i++ {
		originalIdx := len(msgs) - minRecentMessages + (i - 1)
		expected := fmt.Sprintf("message-%d", originalIdx)
		got := result[i].Content[0].Text
		if got != expected {
			t.Errorf("recent message %d: expected %q, got %q", i, expected, got)
		}
	}
}

func TestSummarise_ProviderStartError_FallsBackToSlidingWindow(t *testing.T) {
	mock := &mockSummaryProvider{err: fmt.Errorf("connection refused")}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, strings.Repeat("x", 400))
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          500,
		CurrentTokens:      1000,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// Should fall back to sliding window behavior (no summary message).
	for _, msg := range result {
		if strings.Contains(msg.Content[0].Text, "<conversation_summary>") {
			t.Error("fallback should not produce summary messages")
		}
	}
	// Should have fewer messages than original due to truncation.
	if len(result) >= len(msgs) {
		t.Errorf("expected fewer messages after fallback truncation, got %d", len(result))
	}
}

func TestSummarise_StreamError_FallsBackToSlidingWindow(t *testing.T) {
	mock := &errorStreamProvider{}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, strings.Repeat("x", 400))
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          500,
		CurrentTokens:      1000,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// Should fall back, not return summary.
	for _, msg := range result {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, "<conversation_summary>") {
				t.Error("stream error fallback should not produce summary messages")
			}
		}
	}
}

func TestSummarise_CachesPreviousSummary(t *testing.T) {
	mock := &mockSummaryProvider{summary: "Cached summary content."}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, fmt.Sprintf("turn-%d", i))
	}

	budget := TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	}

	// First call: should invoke provider.
	_, err := s.Prepare(context.Background(), msgs, budget)
	if err != nil {
		t.Fatalf("first Prepare() error: %v", err)
	}
	if mock.callCount != 1 {
		t.Fatalf("expected 1 provider call after first Prepare, got %d", mock.callCount)
	}

	// Second call with same messages: should use cache.
	result, err := s.Prepare(context.Background(), msgs, budget)
	if err != nil {
		t.Fatalf("second Prepare() error: %v", err)
	}
	if mock.callCount != 1 {
		t.Errorf("expected provider not called again (cache hit), but call count is %d", mock.callCount)
	}
	if !strings.Contains(result[0].Content[0].Text, "Cached summary content") {
		t.Error("expected cached summary to be used")
	}
}

func TestSummarise_CacheInvalidatedOnNewMessages(t *testing.T) {
	mock := &mockSummaryProvider{summary: "Summary v1."}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, fmt.Sprintf("turn-%d", i))
	}

	budget := TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      500,
		ReserveForResponse: 50,
	}

	// First call.
	_, err := s.Prepare(context.Background(), msgs, budget)
	if err != nil {
		t.Fatalf("first Prepare() error: %v", err)
	}

	// Add more messages (simulating conversation progressing).
	msgs = append(msgs,
		makeMessage("user", "new question"),
		makeMessage("assistant", "new answer"),
	)

	mock.summary = "Summary v2."

	// Second call with different old messages: cache should miss.
	_, err = s.Prepare(context.Background(), msgs, budget)
	if err != nil {
		t.Fatalf("second Prepare() error: %v", err)
	}
	if mock.callCount != 2 {
		t.Errorf("expected 2 provider calls (cache miss), got %d", mock.callCount)
	}
}

func TestSummarise_TooFewMessages_ReturnsAsIs(t *testing.T) {
	mock := &mockSummaryProvider{summary: "should not be called"}
	s := NewSummariseStrategy(mock, "test-model")

	// Only 4 messages, fewer than minRecentMessages (6).
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 400)),
		makeMessage("assistant", strings.Repeat("b", 400)),
		makeMessage("user", strings.Repeat("c", 400)),
		makeMessage("assistant", strings.Repeat("d", 400)),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          200,
		CurrentTokens:      400,
		ReserveForResponse: 50,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// With only 4 messages and minRecent=6, splitIdx=0 so nothing to summarise.
	// Messages returned as-is.
	if len(result) != 4 {
		t.Errorf("expected 4 messages returned as-is, got %d", len(result))
	}
	if mock.callCount != 0 {
		t.Errorf("provider should not be called when too few messages, got %d calls", mock.callCount)
	}
}

func TestSummarise_ZeroBudget_ReturnsRecentMessages(t *testing.T) {
	mock := &mockSummaryProvider{summary: "should not matter"}
	s := NewSummariseStrategy(mock, "test-model")

	msgs := make([]types.Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = makeMessage(role, "msg")
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          50,
		CurrentTokens:      500,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// With available <= 0, should return last minRecentMessages.
	if len(result) != minRecentMessages {
		t.Errorf("expected %d messages with zero budget, got %d", minRecentMessages, len(result))
	}
}
