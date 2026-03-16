package context

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// mockFileWriter records writes and can be configured to fail.
type mockFileWriter struct {
	written map[string]string // path -> content
	err     error             // if non-nil, WriteFile returns this
}

func newMockFileWriter() *mockFileWriter {
	return &mockFileWriter{written: make(map[string]string)}
}

func (m *mockFileWriter) WriteFile(_ context.Context, path string, content string) error {
	if m.err != nil {
		return m.err
	}
	m.written[path] = content
	return nil
}

func TestOffloadToFile_ImplementsInterface(t *testing.T) {
	var _ ContextStrategy = (*OffloadToFileStrategy)(nil)
}

func TestOffloadToFile_WithinBudget_ReturnsUnchanged(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)
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
	if len(fw.written) != 0 {
		t.Errorf("expected no files written, got %d", len(fw.written))
	}
}

func TestOffloadToFile_OverBudget_OffloadsLargeToolResults(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	largeContent := strings.Repeat("x", 3000)
	msgs := []types.Message{
		makeMessage("user", "do something"),
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "read_file"},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: largeContent},
			},
		},
		makeMessage("user", "more context"),
		makeMessage("assistant", "working on it"),
		makeMessage("user", "question"),
		makeMessage("assistant", "answer"),
		makeMessage("user", "another question"),
		makeMessage("assistant", "another answer"),
		// Last 6 messages are indices 3-8, so index 2 (the tool_result) is eligible.
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(result))
	}

	// The tool_result at index 2 should have been offloaded.
	offloaded := result[2].Content[0]
	expectedPath := ".stirrup/context/turn-2-t1.txt"
	expectedRef := fmt.Sprintf("[Full output offloaded to %s — read this file if you need the details]", expectedPath)
	if offloaded.Content != expectedRef {
		t.Errorf("expected offload reference, got: %s", offloaded.Content)
	}

	// Verify the file was written with the original content.
	if fw.written[expectedPath] != largeContent {
		t.Errorf("expected file to contain original content")
	}
}

func TestOffloadToFile_RecentMessagesPreservedInFull(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	largeContent := strings.Repeat("y", 3000)
	// Build exactly 6 messages where the last 6 contain a large tool_result.
	// Since they're all "recent", none should be offloaded.
	msgs := []types.Message{
		makeMessage("user", "do something"),
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "read_file"},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: largeContent},
			},
		},
		makeMessage("assistant", "got it"),
		makeMessage("user", "now what"),
		makeMessage("assistant", "done"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// The tool_result at index 2 is within the last 6, so should be untouched.
	if result[2].Content[0].Content != largeContent {
		t.Error("recent large tool_result should not be offloaded")
	}
	if len(fw.written) != 0 {
		t.Errorf("expected no files written for recent messages, got %d", len(fw.written))
	}
}

func TestOffloadToFile_SmallToolResultsNotOffloaded(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	smallContent := strings.Repeat("z", 500) // well under threshold
	msgs := []types.Message{
		makeMessage("user", "do something"),
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "read_file"},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: smallContent},
			},
		},
		// Pad with 6 more messages to push the tool_result out of recency window.
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
		makeMessage("assistant", "f"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// Small tool result should remain unchanged.
	if result[2].Content[0].Content != smallContent {
		t.Error("small tool_result should not be offloaded")
	}
	if len(fw.written) != 0 {
		t.Errorf("expected no files written for small content, got %d", len(fw.written))
	}
}

func TestOffloadToFile_WriteFailure_FallsBackToTruncation(t *testing.T) {
	fw := newMockFileWriter()
	fw.err = fmt.Errorf("disk full")
	s := NewOffloadToFileStrategy(fw)

	largeContent := strings.Repeat("q", 3000)
	msgs := []types.Message{
		makeMessage("user", "do something"),
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "t1", Name: "read_file"},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: largeContent},
			},
		},
		// 6 recent messages to push tool_result outside recency window.
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
		makeMessage("assistant", "f"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	content := result[2].Content[0].Content
	if !strings.Contains(content, "[...truncated...]") {
		t.Error("expected truncation marker in fallback content")
	}
	// Should keep first 500 and last 500 chars.
	if !strings.HasPrefix(content, strings.Repeat("q", 500)) {
		t.Error("truncated content should start with first 500 chars")
	}
	if !strings.HasSuffix(content, strings.Repeat("q", 500)) {
		t.Error("truncated content should end with last 500 chars")
	}
}

func TestOffloadToFile_MultipleToolResultsSameTurn(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	large1 := strings.Repeat("a", 3000)
	large2 := strings.Repeat("b", 3000)
	msgs := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: large1},
				{Type: "tool_result", ToolUseID: "t2", Content: large2},
			},
		},
		// 6 recent messages.
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
		makeMessage("assistant", "f"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// Both tool results in message 0 should be offloaded to separate files.
	if len(fw.written) != 2 {
		t.Fatalf("expected 2 files written, got %d", len(fw.written))
	}

	path1 := ".stirrup/context/turn-0-t1.txt"
	path2 := ".stirrup/context/turn-0-t2.txt"

	if fw.written[path1] != large1 {
		t.Error("first tool result not written correctly")
	}
	if fw.written[path2] != large2 {
		t.Error("second tool result not written correctly")
	}

	// Verify both content blocks were replaced.
	block0 := result[0].Content[0]
	block1 := result[0].Content[1]
	if !strings.Contains(block0.Content, path1) {
		t.Errorf("first block should reference %s, got: %s", path1, block0.Content)
	}
	if !strings.Contains(block1.Content, path2) {
		t.Errorf("second block should reference %s, got: %s", path2, block1.Content)
	}
}

func TestOffloadToFile_NonToolResultMessagesUntouched(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	longText := strings.Repeat("x", 5000)
	msgs := []types.Message{
		makeMessage("user", longText),
		makeMessage("assistant", longText),
		// 6 recent messages.
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
		makeMessage("assistant", "f"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	// Text messages should be completely untouched.
	if result[0].Content[0].Text != longText {
		t.Error("user text message was modified")
	}
	if result[1].Content[0].Text != longText {
		t.Error("assistant text message was modified")
	}
	if len(fw.written) != 0 {
		t.Errorf("expected no files written for non-tool-result messages, got %d", len(fw.written))
	}
}

func TestOffloadToFile_ReplacementTextContainsCorrectPath(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	msgs := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_abc123", Content: strings.Repeat("x", 3000)},
			},
		},
		// 6 recent messages.
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
		makeMessage("assistant", "f"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}

	expectedPath := ".stirrup/context/turn-0-call_abc123.txt"
	content := result[0].Content[0].Content
	expectedFull := fmt.Sprintf("[Full output offloaded to %s — read this file if you need the details]", expectedPath)
	if content != expectedFull {
		t.Errorf("replacement text mismatch.\nwant: %s\ngot:  %s", expectedFull, content)
	}
}

func TestOffloadToFile_EmptyMessages_ReturnsEmpty(t *testing.T) {
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	result, err := s.Prepare(context.Background(), nil, TokenBudget{
		MaxTokens:          10000,
		CurrentTokens:      100,
		ReserveForResponse: 2000,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 messages, got %d", len(result))
	}
}
