package context

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"reflect"
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
		MaxTokens:          400,
		CurrentTokens:      500,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Available = 300: dropping the first exchange after the task fits.
	if len(result) != 3 {
		t.Fatalf("expected 3 messages after truncation, got %d", len(result))
	}
	if result[0].Content[0].Text != strings.Repeat("a", 400) {
		t.Error("expected the task to stay first")
	}
	if result[1].Content[0].Text != strings.Repeat("d", 400) {
		t.Error("expected the retained tail to start at the second assistant turn")
	}
	if got := s.LastCompaction().TokensAfter; got != 300 {
		t.Errorf("TokensAfter = %d, want 300", got)
	}
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
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Even though budget is tiny, the task and the last exchange survive.
	if want := []types.Message{msgs[0], msgs[3], msgs[4]}; !reflect.DeepEqual(result, want) {
		t.Errorf("expected the task and the last exchange, got %d messages", len(result))
	}
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
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("expected the task and its only exchange kept, got %d messages", len(result))
	}
	if s.LastCompaction() != nil {
		t.Error("expected no compaction when nothing can be dropped")
	}
}

func TestSlidingWindow_NegativeAvailable(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if want := []types.Message{msgs[0], msgs[3], msgs[4]}; !reflect.DeepEqual(result, want) {
		t.Errorf("expected the task and the last exchange with negative budget, got %d messages", len(result))
	}
	if c := s.LastCompaction(); c == nil || c.MessagesBefore != 5 || c.MessagesAfter != 3 {
		t.Errorf("LastCompaction = %+v, want 5 -> 3 messages", c)
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
	// The only exchange is the last one, so nothing can be dropped
	// without separating the tool_result from its tool_use.
	if !reflect.DeepEqual(result, msgs) {
		t.Errorf("expected the history unchanged, got %d messages", len(result))
	}
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
		CurrentTokens:      500,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if want := []types.Message{msgs[0], msgs[3], msgs[4]}; !reflect.DeepEqual(result, want) {
		t.Fatalf("kept %+v, want the task and the last exchange", result)
	}
	if got := s.LastCompaction().TokensAfter; got != 300 {
		t.Errorf("TokensAfter = %d, want 300", got)
	}
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
			available := budget.MaxTokens - budget.ReserveForResponse
			s := NewSlidingWindowStrategy()
			result, err := s.Prepare(context.Background(), msgs, budget)
			if err != nil {
				t.Fatalf("seed %d: Prepare() error: %v", seed, err)
			}
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
				if len(result) != len(msgs) {
					t.Fatalf("seed %d max=%d: %d messages dropped without a compaction event", seed, budget.MaxTokens, len(msgs)-len(result))
				}
				if available > 0 && budget.CurrentTokens > available && last > 1 {
					t.Fatalf("seed %d max=%d: over budget with droppable exchanges but nothing dropped", seed, budget.MaxTokens)
				}
				continue
			}
			cut := len(msgs) - len(result) + 1
			if available <= 0 {
				if cut != last {
					t.Fatalf("seed %d max=%d: no-room cut = %d, want the last exchange at %d", seed, budget.MaxTokens, cut, last)
				}
				continue
			}
			if got, want := c.TokensAfter, tokensWithCut(cut); got != want {
				t.Fatalf("seed %d max=%d: TokensAfter = %d, want %d", seed, budget.MaxTokens, got, want)
			}
			if c.TokensAfter > available && cut != last {
				t.Fatalf("seed %d max=%d: still over budget at cut %d before the last exchange %d", seed, budget.MaxTokens, cut, last)
			}
			for earlier := 1; earlier < cut; earlier++ {
				if msgs[earlier].Role == "assistant" && tokensWithCut(earlier) <= available {
					t.Fatalf("seed %d max=%d: cut %d drops more than needed; cut %d already fits", seed, budget.MaxTokens, cut, earlier)
				}
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
	if got := estimateTokens(msg); got != 100 {
		t.Errorf("estimateTokens = %d, want 100 (thinking text only)", got)
	}
}

func TestSlidingWindow_ImplementsInterface(t *testing.T) {
	var _ ContextStrategy = (*SlidingWindowStrategy)(nil)
}

// TestSlidingWindow_CountsReplayFields pins that replay state resent with a
// message (a 40 KB stored Responses output array here) counts toward its
// estimate, so the window drops it rather than overflowing the context.
func TestSlidingWindow_CountsReplayFields(t *testing.T) {
	stored := `["` + strings.Repeat("A", 40*1024-4) + `"]`
	withReplay := makeMessage("assistant", strings.Repeat("b", 400))
	withReplay.ReplayFields = map[string]json.RawMessage{"openai_responses.output": json.RawMessage(stored)}
	if got, want := estimateTokens(withReplay), 100+40*1024/4; got != want {
		t.Fatalf("estimateTokens = %d, want %d", got, want)
	}

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
		CurrentTokens:      5*100 + 100 + 40*1024/4,
		ReserveForResponse: 100,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	// Dropping the first exchange after the task removes the replay state
	// and fits the remaining 400 tokens in the 900 available.
	if len(result) != 4 || result[0].Content[0].Text != strings.Repeat("a", 400) ||
		result[1].Content[0].Text != strings.Repeat("d", 400) {
		t.Fatalf("kept %d messages, want the task and the last 3", len(result))
	}
	if got := s.LastCompaction().TokensAfter; got != 400 {
		t.Errorf("TokensAfter = %d, want 400", got)
	}
}
