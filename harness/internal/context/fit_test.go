package context

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func cloneHistory(messages []types.Message) []types.Message {
	clone := make([]types.Message, len(messages))
	for i, msg := range messages {
		clone[i] = msg
		clone[i].Content = slices.Clone(msg.Content)
	}
	return clone
}

func historyTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		total += estimateTokens(msg)
	}
	return total
}

func toolExchange(id, result string) []types.Message {
	return []types.Message{
		{Role: "assistant", Content: []types.ContentBlock{{Type: "tool_use", ID: id, Name: "read_file", Input: json.RawMessage(`{"path":"big.txt"}`)}}},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: result}}},
	}
}

func TestFitLimit_SubtractsReserveFromWindow(t *testing.T) {
	cases := []struct {
		name   string
		budget TokenBudget
		want   int
	}{
		{"default window", TokenBudget{MaxTokens: 200_000, ReserveForResponse: 64_000}, 122_400},
		{"small window", TokenBudget{MaxTokens: 2000, ReserveForResponse: 500}, 1350},
		{"reserve fills the window", TokenBudget{MaxTokens: 500, ReserveForResponse: 500}, 0},
		{"reserve exceeds the window", TokenBudget{MaxTokens: 100, ReserveForResponse: 500}, -400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, FitLimit(tc.budget), "budget %+v", tc.budget)
		})
	}
}

func TestTruncateToolResults_LargeResultFitsSmallBudget(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	head := "HEAD:" + strings.Repeat("h", 995)
	tail := strings.Repeat("t", 995) + ":TAIL"
	large := head + strings.Repeat("x", 38_000) + tail
	msgs := append([]types.Message{makeMessage("user", "summarise big.txt")}, toolExchange("call_1", large)...)
	original := cloneHistory(msgs)

	limit := FitLimit(TokenBudget{MaxTokens: 2000, ReserveForResponse: 500})
	excess := historyTokens(msgs) - limit
	must.Positive(excess, "history of %d tokens already fits %d", historyTokens(msgs), limit)

	result, cut := TruncateToolResults(msgs, excess)
	must.Equal(1, cut)
	must.LessOrEqual(historyTokens(result), limit, "truncated history must fit the limit")

	block := result[2].Content[0]
	must.Equal("call_1", block.ToolUseID, "truncated block lost its pairing: %+v", block)
	must.Equal("tool_result", block.Type, "truncated block lost its pairing: %+v", block)
	check.Contains(block.Content, "[...truncated ", "truncated content has no marker")
	check.True(strings.HasPrefix(block.Content, "HEAD:"), "truncated content must keep the head of the result")
	check.True(strings.HasSuffix(block.Content, ":TAIL"), "truncated content must keep the tail of the result")

	must.NoError(checkCompactedHistory(msgs, result))
	must.Equal(original, msgs, "TruncateToolResults modified the stored history")
}

func TestTruncateToolResults_OldestResultFirst(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", strings.Repeat("o", 8000))...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 500)
	must.Equal(1, cut)
	check.Contains(result[2].Content[0].Content, "[...truncated ", "the oldest result was not truncated")
	check.Equal(msgs[4], result[4], "the newest result was truncated although the oldest covered the excess")

	dropped := historyTokens(msgs) - historyTokens(result)
	check.GreaterOrEqual(dropped, 500, "the cut must cover the excess of 500 tokens")
	check.LessOrEqual(dropped, 600, "the cut should stay close to the excess of 500 tokens")
}

func TestTruncateToolResults_SpillsToNewerResults(t *testing.T) {
	must := require.New(t)
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", strings.Repeat("o", 2000))...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 1000)
	must.Equal(2, cut)
	dropped := historyTokens(msgs) - historyTokens(result)
	must.GreaterOrEqual(dropped, 1000, "the cut must cover the excess of 1000 tokens")
	must.NoError(checkCompactedHistory(msgs, result))
}

func TestTruncateToolResults_CutsLargeResultsBeforeSmallOnes(t *testing.T) {
	must := require.New(t)
	reference := "[Full output offloaded to .stirrup/context/turn-2-old.txt — read this file if you need the details]"
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", reference)...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 1000)
	must.Equal(1, cut)
	must.Equal(msgs[2], result[2], "a small older result was cut although a larger result covered the excess")

	result, cut = TruncateToolResults(msgs, 2100)
	must.Equal(2, cut, "the small result must be cut once the large result alone cannot cover the excess")
	must.Contains(result[2].Content[0].Content, "[...truncated ", "the small result was not cut")
}

func TestTruncateToolResults_DropsStructuredEnvelope(t *testing.T) {
	must := require.New(t)
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("call_1", strings.Repeat("r", 4000))...)
	msgs[2].Content[0].Structured = json.RawMessage(`{"lines":"` + strings.Repeat("r", 4000) + `"}`)
	msgs[2].Content[0].Kind = "read_file"

	result, cut := TruncateToolResults(msgs, 1200)
	must.Equal(1, cut)
	block := result[2].Content[0]
	must.Empty(block.Kind, "a truncated result kept its kind")
	must.Nil(block.Structured, "a truncated result kept its structured envelope")
	must.NotNil(msgs[2].Content[0].Structured, "the stored block lost its structured envelope")
}

func TestTruncateToolResults_NothingToDo(t *testing.T) {
	withToolResult := []types.Message{makeMessage("user", "task")}
	withToolResult = append(withToolResult, toolExchange("call_1", strings.Repeat("r", 4000))...)
	textOnly := []types.Message{makeMessage("user", strings.Repeat("t", 40_000))}

	cases := []struct {
		name   string
		msgs   []types.Message
		excess int
	}{
		{"zero excess", withToolResult, 0},
		{"negative excess", withToolResult, -10},
		{"history without tool results", textOnly, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			result, cut := TruncateToolResults(tc.msgs, tc.excess)
			must.Equal(0, cut)
			must.Same(&tc.msgs[0], &result[0], "the input slice must come back unchanged")
		})
	}
}

func TestTruncateToolResults_ExcessBeyondAllResults(t *testing.T) {
	must := require.New(t)
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("a", strings.Repeat("a", 3000))...)
	msgs = append(msgs, toolExchange("b", strings.Repeat("b", 3000))...)

	result, cut := TruncateToolResults(msgs, 100_000)
	must.Equal(2, cut)
	for _, tc := range []struct {
		name  string
		index int
	}{
		{"oldest result", 2},
		{"newest result", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := result[tc.index].Content[0].Content
			must.True(strings.HasPrefix(got, "\n[...truncated 3000 bytes...]"), "want only the marker left, got %q", got)
		})
	}
	must.NoError(checkCompactedHistory(msgs, result))
}

func TestTruncateHeadTail_KeepsHeadAndTail(t *testing.T) {
	t.Run("short content is unchanged", func(t *testing.T) {
		must := require.New(t)
		must.Equal("short", truncateHeadTail("short", 10))
	})

	t.Run("long content keeps both ends around a marker", func(t *testing.T) {
		must := require.New(t)
		content := strings.Repeat("a", 100) + strings.Repeat("b", 100)
		want := strings.Repeat("a", 10) + "\n[...truncated 180 bytes...]\n" + strings.Repeat("b", 10)
		must.Equal(want, truncateHeadTail(content, 10))
	})

	t.Run("multi-byte characters are not split", func(t *testing.T) {
		must := require.New(t)
		multibyte := strings.Repeat("é", 500) + strings.Repeat("語", 500)
		for keep := range 8 {
			got := truncateHeadTail(multibyte, 100+keep)
			must.True(utf8.ValidString(got), "keep=%d: truncation split a multi-byte character", keep)
			must.Less(len(got), len(multibyte), "keep=%d: truncation did not shorten the content", keep)
		}
	})
}

// TestTruncateToolResults_GeneratedHistories pins that truncation keeps
// every generated history paired and unmodified, and removes at least the
// requested excess unless every tool result is already down to its marker
// or too small to cut.
func TestTruncateToolResults_GeneratedHistories(t *testing.T) {
	must := require.New(t)
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 667))
		msgs := generateHistory(r, r.IntN(14))
		original := cloneHistory(msgs)
		excess := r.IntN(historyTokens(msgs) + 1)

		result, _ := TruncateToolResults(msgs, excess)
		must.NoError(checkCompactedHistory(msgs, result), "seed %d", seed)
		must.Equal(original, msgs, "seed %d: the stored history was modified", seed)
		must.Len(result, len(msgs), "seed %d: truncation changed the message count", seed)
		if drop := historyTokens(msgs) - historyTokens(result); drop >= excess {
			continue
		}
		for i, msg := range result {
			for j, block := range msg.Content {
				if block.Type != "tool_result" {
					continue
				}
				minimal := strings.HasPrefix(block.Content, "\n[...truncated ")
				small := len(msgs[i].Content[j].Content) <= len(truncationMarker(len(msgs[i].Content[j].Content)))+truncationSlackChars
				must.True(minimal || small,
					"seed %d: the excess of %d tokens was not met, yet message %d keeps %d bytes of a tool result",
					seed, excess, i, len(block.Content))
			}
		}
	}
}

// TestCompaction_PreparedHistoryFitsFitLimit pins that the strategies
// which drop exchanges compact to the fit check's limit, so the fit
// postcondition never truncates tool results after a compaction that could
// have fitted by dropping more. Summarise drops exchanges only through its
// fallback, and leaves a history too short to summarise unchanged.
func TestCompaction_PreparedHistoryFitsFitLimit(t *testing.T) {
	must := require.New(t)
	strategies := compactionStrategies()
	for name := range strategies {
		if name != "sliding-window" && !strings.HasSuffix(name, "-fallback") {
			delete(strategies, name)
		}
	}
	for seed := range uint64(150) {
		r := rand.New(rand.NewPCG(seed, 668))
		msgs := generateHistory(r, r.IntN(14))
		overhead := r.IntN(3000)
		minimal, _ := minimalHistory(msgs)
		summarisable := pairSafeCut(msgs, len(msgs)-minRecentMessages) > 1
		for name, newStrategy := range strategies {
			if name != "sliding-window" && !summarisable {
				continue
			}
			for _, budget := range compactionBudgets(msgs, overhead) {
				limit := FitLimit(budget)
				if limit <= 0 || overhead+historyTokens(minimal) > limit {
					continue
				}
				prepared, err := newStrategy().Prepare(context.Background(), msgs, budget)
				must.NoError(err, "seed %d %s: Prepare() returned an error", seed, name)
				tokens := overhead + historyTokens(prepared)
				_, cut := TruncateToolResults(prepared, tokens-limit)
				must.Zero(cut,
					"seed %d %s max=%d: prepared history estimates at %d against a fit limit of %d; tool results would be truncated",
					seed, name, budget.MaxTokens, tokens, limit)
			}
		}
	}
}

// TestCompaction_TriggersAtFitLimit pins that every strategy compacts a
// request that fits the room after the response reserve but not FitLimit.
func TestCompaction_TriggersAtFitLimit(t *testing.T) {
	must := require.New(t)
	msgs := parallelCallHistory(3)
	current := historyTokens(msgs)
	budget := TokenBudget{MaxTokens: 100 + current, CurrentTokens: current, ReserveForResponse: 100}
	must.Greater(current, FitLimit(budget), "history of %d tokens fits FitLimit", current)

	for name, newStrategy := range compactionStrategies() {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			s := newStrategy()
			_, err := s.Prepare(context.Background(), msgs, budget)
			must.NoError(err, "Prepare()")
			must.NotNil(s.LastCompaction(), "did not compact a request over FitLimit")
		})
	}
}
