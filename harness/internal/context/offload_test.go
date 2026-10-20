package context

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
	must := require.New(t)
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
	must.NoError(err)
	must.Len(result, 2)
	must.Empty(fw.written)
}

func TestOffloadToFile_OverBudget_OffloadsLargeToolResults(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Len(result, len(msgs))

	// The tool_result at index 2 is offloaded and replaced by a reference to its file.
	expectedPath := ".stirrup/context/turn-2-t1.txt"
	expectedRef := fmt.Sprintf("[Full output offloaded to %s — read this file if you need the details]", expectedPath)
	must.Equal(expectedRef, result[2].Content[0].Content)

	must.Equal(largeContent, fw.written[expectedPath], "offloaded file should hold the original content")
}

func TestOffloadToFile_PreservesReplayFieldsOnModifiedMessages(t *testing.T) {
	must := require.New(t)
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	replay := map[string]json.RawMessage{
		"reasoning_content": json.RawMessage(`"thinking..."`),
	}
	largeContent := strings.Repeat("x", 3000)
	msgs := []types.Message{
		makeMessage("user", "do something"),
		// The rebuild path constructs a fresh types.Message; any field not
		// copied explicitly — ReplayFields here — would be silently dropped.
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: largeContent},
			},
			ReplayFields: replay,
		},
		makeMessage("user", "more context"),
		makeMessage("assistant", "working on it"),
		makeMessage("user", "question"),
		makeMessage("assistant", "answer"),
		makeMessage("user", "another question"),
		makeMessage("assistant", "another answer"),
		// Last 6 messages are indices 2-7, so index 1 is offload-eligible.
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          2000,
		CurrentTokens:      5000,
		ReserveForResponse: 500,
	})
	must.NoError(err)

	// The rebuild must have fired (the tool_result was offloaded)…
	must.Contains(result[1].Content[0].Content, "offloaded to")
	// …and the replay state must have survived it intact.
	must.NotNil(result[1].ReplayFields, "ReplayFields dropped by the offload rebuild")
	must.Equal(`"thinking..."`, string(result[1].ReplayFields["reasoning_content"]))
}

func TestOffloadToFile_RecentMessages_KeptInFull(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	// The tool_result at index 2 is within the last 6, so should be untouched.
	must.Equal(largeContent, result[2].Content[0].Content, "recent large tool_result should not be offloaded")
	must.Empty(fw.written, "no files should be written for recent messages")
}

func TestOffloadToFile_SmallToolResults_NotOffloaded(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	must.Equal(smallContent, result[2].Content[0].Content, "small tool_result should not be offloaded")
	must.Empty(fw.written, "no files should be written for small content")
}

func TestOffloadToFile_WriteFailure_FallsBackToTruncation(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	content := result[2].Content[0].Content
	must.Contains(content, "[...truncated 2000 bytes...]")
	// Should keep first 500 and last 500 chars.
	must.True(strings.HasPrefix(content, strings.Repeat("q", 500)), "truncated content should start with first 500 chars")
	must.True(strings.HasSuffix(content, strings.Repeat("q", 500)), "truncated content should end with last 500 chars")
}

func TestOffloadToFile_MultipleToolResults_EachOffloadedToOwnFile(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	// Both tool results in message 0 should be offloaded to separate files.
	must.Len(fw.written, 2)

	path1 := ".stirrup/context/turn-0-t1.txt"
	path2 := ".stirrup/context/turn-0-t2.txt"

	must.Equal(large1, fw.written[path1], "content written for %s", path1)
	must.Equal(large2, fw.written[path2], "content written for %s", path2)

	// Verify both content blocks were replaced.
	block0 := result[0].Content[0]
	block1 := result[0].Content[1]
	must.Contains(block0.Content, path1)
	must.Contains(block1.Content, path2)
}

func TestOffloadToFile_NonToolResultMessagesUntouched(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	// Text messages should be completely untouched.
	must.Equal(longText, result[0].Content[0].Text, "user text message was modified")
	must.Equal(longText, result[1].Content[0].Text, "assistant text message was modified")
	must.Empty(fw.written, "no files should be written for non-tool-result messages")
}

func TestOffloadToFile_ReplacementText_NamesOffloadPath(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	expectedPath := ".stirrup/context/turn-0-call_abc123.txt"
	expectedFull := fmt.Sprintf("[Full output offloaded to %s — read this file if you need the details]", expectedPath)
	must.Equal(expectedFull, result[0].Content[0].Content)
}

func TestOffloadToFile_EmptyMessages_ReturnsEmpty(t *testing.T) {
	must := require.New(t)
	fw := newMockFileWriter()
	s := NewOffloadToFileStrategy(fw)

	result, err := s.Prepare(context.Background(), nil, TokenBudget{
		MaxTokens:          10000,
		CurrentTokens:      100,
		ReserveForResponse: 2000,
	})
	must.NoError(err)
	must.Empty(result)
}

// The no-room path keeps the task and the last complete exchange, never the
// results of parallel tool calls without the assistant turn that made them.
func TestOffloadToFile_NoRoom_KeepsTaskAndLastExchange(t *testing.T) {
	must := require.New(t)
	msgs := parallelCallHistory(1)
	s := NewOffloadToFileStrategy(newMockFileWriter())

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          100,
		CurrentTokens:      700,
		ReserveForResponse: 100,
	})
	must.NoError(err)

	want := []types.Message{msgs[0], msgs[5], msgs[6]}
	must.Equal(want, result, "keeps the task and the last exchange")

	c := s.LastCompaction()
	must.NotNil(c, "a no-room compaction should record its LastCompaction")
	must.Equal(7, c.MessagesBefore)
	must.Equal(3, c.MessagesAfter)

	must.NoError(checkCompactedHistory(msgs, result))
}
