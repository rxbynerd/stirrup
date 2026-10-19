package context

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.NoError(t, err)
	require.Len(t, result, 2)
}

func TestSlidingWindow_DropsOldestExchangesKeepingTask(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 400)),      // ~100 tokens
		makeMessage("assistant", strings.Repeat("b", 400)), // ~100 tokens
		makeMessage("user", strings.Repeat("c", 400)),      // ~100 tokens
		makeMessage("assistant", strings.Repeat("d", 400)), // ~100 tokens
		makeMessage("user", strings.Repeat("e", 400)),      // ~100 tokens
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          500,
		CurrentTokens:      estimateHistory(msgs),
		ReserveForResponse: 100,
	})
	require.NoError(t, err)
	// FitLimit is 360: dropping the first exchange after the task fits.
	require.Len(t, result, 3)
	require.Equal(t, strings.Repeat("a", 400), result[0].Content[0].Text, "keeps the task first")
	require.Equal(t, strings.Repeat("d", 400), result[1].Content[0].Text, "retained tail starts at the second assistant turn")
	require.Equal(t, estimateHistory(result), s.LastCompaction().TokensAfter)
}

func TestSlidingWindow_PreservesTaskAndLastExchange(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 4000)),
		makeMessage("assistant", strings.Repeat("b", 4000)),
		makeMessage("user", strings.Repeat("c", 4000)),
		makeMessage("assistant", strings.Repeat("d", 4000)),
		makeMessage("user", strings.Repeat("e", 4000)),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          100,
		CurrentTokens:      5000,
		ReserveForResponse: 50,
	})
	require.NoError(t, err)
	// Even though budget is tiny, the task and the last exchange survive.
	require.Equal(t, []types.Message{msgs[0], msgs[3], msgs[4]}, result)
}

func TestSlidingWindow_ZeroBudgetKeepsSingleExchange(t *testing.T) {
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
	require.NoError(t, err)
	require.Len(t, result, 3, "keeps the task and its only exchange")
	require.Nil(t, s.LastCompaction(), "no compaction when nothing can be dropped")
}

func TestSlidingWindow_NegativeBudgetKeepsTaskAndLastExchange(t *testing.T) {
	s := NewSlidingWindowStrategy()
	msgs := []types.Message{
		makeMessage("user", "a"),
		makeMessage("assistant", "b"),
		makeMessage("user", "c"),
		makeMessage("assistant", "d"),
		makeMessage("user", "e"),
	}

	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          50,
		CurrentTokens:      500,
		ReserveForResponse: 100,
	})
	require.NoError(t, err)
	require.Equal(t, []types.Message{msgs[0], msgs[3], msgs[4]}, result)

	c := s.LastCompaction()
	require.NotNil(t, c, "compaction event")
	require.Equal(t, 5, c.MessagesBefore)
	require.Equal(t, 3, c.MessagesAfter)
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
	require.NoError(t, err)
	require.Len(t, result, 1)
}

func TestSlidingWindow_ToolCallHistoryUnchangedWhenNoSafeCut(t *testing.T) {
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
	require.NoError(t, err)
	// The only exchange is the last one, so nothing can be dropped
	// without separating the tool_result from its tool_use.
	require.Equal(t, msgs, result)
}

// TestSlidingWindow_ParallelCallsNeverOrphanResults pins that a cut never
// lands between an assistant turn with parallel tool calls and the user
// message carrying their results: the window drops the whole exchange and
// keeps the task in front.
func TestSlidingWindow_ParallelCallsNeverOrphanResults(t *testing.T) {
	msgs := parallelCallHistory(0)
	s := NewSlidingWindowStrategy()
	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          450,
		CurrentTokens:      estimateHistory(msgs),
		ReserveForResponse: 100,
	})
	require.NoError(t, err)
	want := []types.Message{msgs[0], msgs[3], msgs[4]}
	require.Equal(t, want, result, "keeps the task and the last exchange")
	require.Equal(t, estimateHistory(want), s.LastCompaction().TokensAfter)
}

// TestSlidingWindow_DropsFewestExchanges pins over generated histories that
// the window drops only as many exchanges as the budget requires, and that
// it stops at the last exchange when even that does not fit.
func TestSlidingWindow_DropsFewestExchanges(t *testing.T) {
	for seed := range uint64(100) {
		r := rand.New(rand.NewPCG(seed, 2))
		msgs := generateHistory(r, r.IntN(14))
		overhead := r.IntN(3000)
		for _, budget := range compactionBudgets(msgs, overhead) {
			label := fmt.Sprintf("seed %d max=%d", seed, budget.MaxTokens)
			available := FitLimit(budget)
			s := NewSlidingWindowStrategy()
			result, err := s.Prepare(context.Background(), msgs, budget)
			require.NoError(t, err, label)
			tokensWithCut := func(cut int) int {
				total := budget.CurrentTokens
				for _, msg := range msgs[1:cut] {
					total -= estimateTokens(msg)
				}
				return total
			}
			last := lastExchangeStart(msgs)
			c := s.LastCompaction()
			if c == nil {
				require.Len(t, result, len(msgs), "%s: messages dropped without a compaction event", label)
				require.False(t, available > 0 && budget.CurrentTokens > available && last > 1,
					"%s: over budget with droppable exchanges but nothing dropped", label)
				continue
			}
			cut := len(msgs) - len(result) + 1
			if available <= 0 {
				require.Equal(t, last, cut, "%s: no-room cut should be the last exchange", label)
				continue
			}
			require.Equal(t, tokensWithCut(cut), c.TokensAfter, "%s: TokensAfter", label)
			require.False(t, c.TokensAfter > available && cut != last,
				"%s: still over budget at cut %d before the last exchange %d", label, cut, last)
			for earlier := 1; earlier < cut; earlier++ {
				require.False(t, msgs[earlier].Role == "assistant" && tokensWithCut(earlier) <= available,
					"%s: cut %d drops more than needed; cut %d already fits", label, cut, earlier)
			}
		}
	}
}

// TestSlidingWindow_EstimateCountsThinkingTextNotSignature pins that a
// thinking block contributes its reasoning text to the estimate and its
// signature does not: billing follows thinking tokens, and a signature is
// several times longer than the text it attests.
func TestSlidingWindow_EstimateCountsThinkingTextNotSignature(t *testing.T) {
	msg := types.Message{Role: "assistant", Content: []types.ContentBlock{
		{Type: "thinking", Text: strings.Repeat("t", 400), ThoughtSignature: strings.Repeat("s", 4000)},
		{Type: "redacted_thinking", ThoughtSignature: strings.Repeat("d", 4000)},
	}}
	// 4 (message) + 3 + 100 (thinking block and its text) + 3 (redacted block)
	require.Equal(t, 110, estimateTokens(msg), "thinking text and block overhead only")
}

func TestSlidingWindow_ImplementsContextStrategy(t *testing.T) {
	var _ ContextStrategy = (*SlidingWindowStrategy)(nil)
}

// TestSlidingWindow_CountsReplayFields pins that replay state resent with a
// message (a 40 KB stored Responses output array here) counts toward its
// estimate, so the window drops it rather than overflowing the context.
func TestSlidingWindow_CountsReplayFields(t *testing.T) {
	stored := `["` + strings.Repeat("A", 40*1024-4) + `"]`
	plain := makeMessage("assistant", strings.Repeat("b", 400))
	withReplay := makeMessage("assistant", strings.Repeat("b", 400))
	withReplay.ReplayFields = map[string]json.RawMessage{"openai_responses.output": json.RawMessage(stored)}
	require.Equal(t, estimateTokens(plain)+40*1024/4, estimateTokens(withReplay),
		"replay state adds one token per four bytes")

	msgs := []types.Message{
		makeMessage("user", strings.Repeat("a", 400)),
		withReplay,
		makeMessage("user", strings.Repeat("c", 400)),
		makeMessage("assistant", strings.Repeat("d", 400)),
		makeMessage("user", strings.Repeat("e", 400)),
		makeMessage("assistant", strings.Repeat("f", 400)),
	}
	s := NewSlidingWindowStrategy()
	result, err := s.Prepare(context.Background(), msgs, TokenBudget{
		MaxTokens:          1000,
		CurrentTokens:      estimateHistory(msgs),
		ReserveForResponse: 100,
	})
	require.NoError(t, err)
	// Dropping the first exchange after the task removes the replay state
	// and fits the remaining four messages in the fit limit of 810.
	require.Len(t, result, 4, "want the task and the last 3 messages")
	require.Equal(t, strings.Repeat("a", 400), result[0].Content[0].Text, "keeps the task first")
	require.Equal(t, strings.Repeat("d", 400), result[1].Content[0].Text, "retained tail starts at the second assistant turn")
	require.Equal(t, estimateHistory(result), s.LastCompaction().TokensAfter)
}

// TestEstimateTokens_CountsEveryFieldSent pins that the estimate counts the
// structured envelope, IDs, tool names and the structural overhead of each
// message and block, as the agentic loop's request estimate does.
func TestEstimateTokens_CountsEveryFieldSent(t *testing.T) {
	call := types.Message{Role: "assistant", Content: []types.ContentBlock{{
		Type: "tool_use", ID: "call_0001", Name: "read_file", Input: json.RawMessage(strings.Repeat("i", 40)),
	}}}
	result := types.Message{Role: "user", Content: []types.ContentBlock{{
		Type: "tool_result", ToolUseID: "call_0001", Content: strings.Repeat("c", 400),
		Structured: json.RawMessage(strings.Repeat("s", 200)), Kind: "file_read",
	}}}

	t.Run("tool use block", func(t *testing.T) {
		// 4 (message) + 3 (block) + 2 (ID) + 2 (name) + 10 (input)
		require.Equal(t, 21, estimateTokens(call))
	})
	t.Run("tool result block", func(t *testing.T) {
		// 4 (message) + 3 (block) + 2 (tool use ID) + 100 (content) + 50 (structured)
		require.Equal(t, 159, estimateTokens(result))
	})
	t.Run("history of both messages", func(t *testing.T) {
		require.Equal(t, 180, EstimateHistoryTokens([]types.Message{call, result}))
	})
}

// structuredToolHistory is a task followed by exchanges of one read_file
// call whose result carries a structured envelope as large as its text.
func structuredToolHistory(exchanges int) []types.Message {
	msgs := []types.Message{makeMessage("user", strings.Repeat("t", 400))}
	for i := range exchanges {
		id := fmt.Sprintf("call_%03d", i)
		msgs = append(msgs,
			types.Message{Role: "assistant", Content: []types.ContentBlock{{
				Type: "tool_use", ID: id, Name: "read_file", Input: json.RawMessage(fmt.Sprintf(`{"path":"f%03d.go"}`, i)),
			}}},
			types.Message{Role: "user", Content: []types.ContentBlock{{
				Type: "tool_result", ToolUseID: id, Content: fmt.Sprintf("m%03d ", 2*i+2) + strings.Repeat("c", 2000),
				Structured: json.RawMessage(`{"content":"` + strings.Repeat("s", 2000) + `"}`), Kind: "file_read",
			}}},
		)
	}
	return msgs
}

// TestSlidingWindow_StructuredResultsDropFewestExchanges pins that the
// window credits each dropped message with its structured envelope, so it
// keeps as many exchanges as fit the budget the loop computes.
func TestSlidingWindow_StructuredResultsDropFewestExchanges(t *testing.T) {
	const overhead = 2000
	msgs := structuredToolHistory(20)
	budget := TokenBudget{
		MaxTokens:          20_000,
		CurrentTokens:      overhead + EstimateHistoryTokens(msgs),
		ReserveForResponse: 4000,
	}
	limit := FitLimit(budget)

	s := NewSlidingWindowStrategy()
	result, err := s.Prepare(context.Background(), msgs, budget)
	require.NoError(t, err)
	kept := overhead + EstimateHistoryTokens(result)
	require.Equal(t, kept, s.LastCompaction().TokensAfter)
	require.LessOrEqual(t, kept, limit, "kept history exceeds the fit limit")
	cut := len(msgs) - len(result) + 1
	require.GreaterOrEqual(t, cut, 3, "nothing was dropped from a history of %d tokens", budget.CurrentTokens)
	oneMore := overhead + EstimateHistoryTokens(pinTask(msgs, cut-2))
	require.Greater(t, oneMore, limit, "keeping one more exchange should exceed the fit limit")
}
