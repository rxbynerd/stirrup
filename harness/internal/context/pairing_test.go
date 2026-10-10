package context

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// checkCompactedHistory reports the first way prepared breaks the shape
// providers accept: the original task must come first and unchanged
// (a strategy may append blocks after it), roles must alternate, and every
// tool_result must answer a tool_use in the immediately preceding assistant
// message, which in turn must have all its results in the next message.
func checkCompactedHistory(original, prepared []types.Message) error {
	if len(original) == 0 {
		if len(prepared) != 0 {
			return fmt.Errorf("empty history prepared into %d messages", len(prepared))
		}
		return nil
	}
	if len(prepared) == 0 {
		return fmt.Errorf("non-empty history prepared into no messages")
	}

	task := original[0]
	first := prepared[0]
	if first.Role != task.Role || len(first.Content) < len(task.Content) ||
		!reflect.DeepEqual(first.Content[:len(task.Content)], task.Content) {
		return fmt.Errorf("first message is not the original task: %+v", first)
	}

	for i, msg := range prepared {
		if i > 0 && msg.Role == prepared[i-1].Role {
			return fmt.Errorf("messages %d and %d both have role %q", i-1, i, msg.Role)
		}
		for _, block := range msg.Content {
			if block.Type != "tool_result" {
				continue
			}
			if i == 0 || !hasBlock(prepared[i-1], "tool_use", block.ToolUseID) {
				return fmt.Errorf("message %d: tool_result %q has no tool_use in the previous message", i, block.ToolUseID)
			}
		}
		if i+1 == len(prepared) {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_use" && !hasBlock(prepared[i+1], "tool_result", block.ID) {
				return fmt.Errorf("message %d: tool_use %q has no tool_result in the next message", i, block.ID)
			}
		}
	}
	return nil
}

func hasBlock(msg types.Message, blockType, id string) bool {
	if blockType == "tool_use" && msg.Role != "assistant" {
		return false
	}
	for _, block := range msg.Content {
		if block.Type != blockType {
			continue
		}
		if (blockType == "tool_use" && block.ID == id) || (blockType == "tool_result" && block.ToolUseID == id) {
			return true
		}
	}
	return false
}

// generateHistory builds a history in the shape the agentic loop produces:
// a task, then exchanges of single or parallel tool calls answered by one
// user message (optionally carrying queued operator input after the
// results), or a text answer followed by verifier feedback, an escalation
// nudge, or operator input. Assistant turns may carry thinking blocks and
// replay state, and tool results may carry a structured envelope.
func generateHistory(r *rand.Rand, exchanges int) []types.Message {
	text := func(prefix string, maxLen int) string {
		return prefix + strings.Repeat(".", r.IntN(maxLen))
	}
	task := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: text("task ", 2000)}}}
	if r.IntN(5) == 0 {
		task.Content[0].Text += "\n\nqueued operator input"
	}
	messages := []types.Message{task}

	callID := 0
	for e := range exchanges {
		assistant := types.Message{Role: "assistant"}
		if r.IntN(3) == 0 {
			assistant.Content = append(assistant.Content, types.ContentBlock{
				Type: "thinking", Text: text("reasoning ", 800), ThoughtSignature: "sig",
			})
		}
		if r.IntN(4) == 0 {
			assistant.ReplayFields = map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"r"`)}
		}

		switch kind := r.IntN(20); {
		case kind < 12:
			if r.IntN(2) == 0 {
				assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text("plan ", 400)})
			}
			user := types.Message{Role: "user"}
			for range 1 + r.IntN(3) {
				callID++
				id := fmt.Sprintf("call_%d", callID)
				assistant.Content = append(assistant.Content, types.ContentBlock{
					Type: "tool_use", ID: id, Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`),
				})
				size := 400
				if r.IntN(5) == 0 {
					size = 3000
				}
				result := types.ContentBlock{
					Type: "tool_result", ToolUseID: id, Content: text("result ", size), IsError: r.IntN(8) == 0,
				}
				if r.IntN(3) == 0 {
					result.Structured = json.RawMessage(`{"content":"` + strings.Repeat("s", size) + `"}`)
					result.Kind = "file_read"
				}
				user.Content = append(user.Content, result)
			}
			if r.IntN(7) == 0 {
				user.Content = append(user.Content, types.ContentBlock{Type: "text", Text: "operator input"})
			}
			messages = append(messages, assistant, user)
		case kind < 15:
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text("done ", 400)})
			messages = append(messages, assistant, types.Message{
				Role: "user", Synthetic: true,
				Content: []types.ContentBlock{{Type: "text", Text: fmt.Sprintf("verifier feedback %d", e)}},
			})
		case kind < 17:
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text("answer ", 400)})
			messages = append(messages, assistant, types.Message{
				Role: "user", Synthetic: true,
				Content: []types.ContentBlock{{Type: "text", Text: "Use the tools to make the change."}},
			})
		default:
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text("question ", 400)})
			messages = append(messages, assistant, types.Message{
				Role:    "user",
				Content: []types.ContentBlock{{Type: "text", Text: text("operator reply ", 400)}},
			})
		}
	}
	return messages
}

// compactionBudgets returns a sweep of budgets for messages, from no room
// at all to more than enough, with overhead standing in for the system
// prompt and tool definitions the loop adds to CurrentTokens.
func compactionBudgets(messages []types.Message, overhead int) []TokenBudget {
	current := overhead
	for _, msg := range messages {
		current += estimateTokens(msg)
	}
	const reserve = 1000
	budgets := []TokenBudget{
		{MaxTokens: reserve, CurrentTokens: current, ReserveForResponse: reserve},
		{MaxTokens: reserve / 2, CurrentTokens: current, ReserveForResponse: reserve},
	}
	for _, pct := range []int{1, 5, 10, 25, 40, 50, 60, 75, 90, 99, 100, 150} {
		budgets = append(budgets, TokenBudget{
			MaxTokens:          reserve + max(current*pct/100, 1),
			CurrentTokens:      current,
			ReserveForResponse: reserve,
		})
	}
	return budgets
}

// compactionStrategies returns a fresh instance of every strategy and of
// the summarise fallback path, keyed by a test-name label.
func compactionStrategies() map[string]func() ContextStrategy {
	return map[string]func() ContextStrategy{
		"sliding-window": func() ContextStrategy { return NewSlidingWindowStrategy() },
		"summarise": func() ContextStrategy {
			return NewSummariseStrategy(&mockSummaryProvider{summary: "earlier work"}, "summary-model", 0)
		},
		"summarise-small-window": func() ContextStrategy {
			return NewSummariseStrategy(&mockSummaryProvider{summary: "earlier work"}, "summary-model", 4000)
		},
		"summarise-start-error-fallback": func() ContextStrategy {
			return NewSummariseStrategy(&mockSummaryProvider{err: fmt.Errorf("connection refused")}, "summary-model", 0)
		},
		"summarise-stream-error-fallback": func() ContextStrategy {
			return NewSummariseStrategy(&errorStreamProvider{}, "summary-model", 0)
		},
		"offload-to-file": func() ContextStrategy { return NewOffloadToFileStrategy(newMockFileWriter()) },
		"offload-to-file-write-failure": func() ContextStrategy {
			fw := newMockFileWriter()
			fw.err = fmt.Errorf("disk full")
			return NewOffloadToFileStrategy(fw)
		},
	}
}

// parallelCallHistory is a task, an assistant turn with two parallel tool
// calls, their results in one user message, then extra+1 single-call
// exchanges. Every message estimates at about 110 tokens.
func parallelCallHistory(extra int) []types.Message {
	pad := func(s string, n int) string { return s + strings.Repeat(".", n-len(s)) }
	call := func(id string, n int) types.ContentBlock {
		return types.ContentBlock{Type: "tool_use", ID: id, Name: "read_file", Input: json.RawMessage(pad(`{"path":"`, n-2) + `"}`)}
	}
	result := func(id string, n int) types.ContentBlock {
		return types.ContentBlock{Type: "tool_result", ToolUseID: id, Content: pad("result "+id, n)}
	}
	messages := []types.Message{
		makeMessage("user", pad("task", 400)),
		{Role: "assistant", Content: []types.ContentBlock{call("a", 200), call("b", 200)}},
		{Role: "user", Content: []types.ContentBlock{result("a", 200), result("b", 200)}},
	}
	for i := range extra + 1 {
		id := fmt.Sprintf("c%d", i)
		messages = append(messages,
			types.Message{Role: "assistant", Content: []types.ContentBlock{call(id, 400)}},
			types.Message{Role: "user", Content: []types.ContentBlock{result(id, 400)}},
		)
	}
	return messages
}

func TestCompaction_ParallelCallHistoryStaysPaired(t *testing.T) {
	for _, extra := range []int{0, 3} {
		msgs := parallelCallHistory(extra)
		for name, newStrategy := range compactionStrategies() {
			for _, budget := range compactionBudgets(msgs, 0) {
				t.Run(fmt.Sprintf("%s/extra=%d/max=%d", name, extra, budget.MaxTokens), func(t *testing.T) {
					result, err := newStrategy().Prepare(context.Background(), msgs, budget)
					if err != nil {
						t.Fatalf("Prepare() error: %v", err)
					}
					if err := checkCompactedHistory(msgs, result); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestCompaction_GeneratedHistoriesStayPaired(t *testing.T) {
	compactions := map[string]int{}
	for seed := range uint64(150) {
		r := rand.New(rand.NewPCG(seed, 624))
		msgs := generateHistory(r, r.IntN(14))
		overhead := r.IntN(3000)
		for name, newStrategy := range compactionStrategies() {
			for _, budget := range compactionBudgets(msgs, overhead) {
				strategy := newStrategy()
				result, err := strategy.Prepare(context.Background(), msgs, budget)
				if err != nil {
					t.Fatalf("seed %d %s max=%d: Prepare() error: %v", seed, name, budget.MaxTokens, err)
				}
				if err := checkCompactedHistory(msgs, result); err != nil {
					t.Fatalf("seed %d %s max=%d: %v", seed, name, budget.MaxTokens, err)
				}
				if strategy.LastCompaction() != nil {
					compactions[name]++
				}
			}
		}
	}
	for name := range compactionStrategies() {
		if compactions[name] == 0 {
			t.Errorf("%s never compacted a generated history; the sweep does not exercise it", name)
		}
	}
}

// TestCompaction_GeneratedHistoriesKeepSuffix pins that the strategies
// which drop messages keep an unchanged tail of the original history after
// the task, so provider replay state stays with its own message.
func TestCompaction_GeneratedHistoriesKeepSuffix(t *testing.T) {
	strategies := compactionStrategies()
	delete(strategies, "offload-to-file")
	delete(strategies, "offload-to-file-write-failure")
	for seed := range uint64(60) {
		r := rand.New(rand.NewPCG(seed, 1))
		msgs := generateHistory(r, r.IntN(14))
		for name, newStrategy := range strategies {
			for _, budget := range compactionBudgets(msgs, 0) {
				result, err := newStrategy().Prepare(context.Background(), msgs, budget)
				if err != nil {
					t.Fatalf("seed %d %s: Prepare() error: %v", seed, name, err)
				}
				tail := result[1:]
				if want := msgs[len(msgs)-len(tail):]; !reflect.DeepEqual(tail, want) {
					t.Fatalf("seed %d %s max=%d: retained messages are not an unchanged tail of the history", seed, name, budget.MaxTokens)
				}
			}
		}
	}
}

func TestPairSafeCut(t *testing.T) {
	msgs := parallelCallHistory(1) // task, a, u, a, u, a, u
	cases := []struct {
		candidate, want int
	}{
		{-3, 1}, {0, 1}, {1, 1}, {2, 3}, {3, 3}, {4, 5}, {5, 5}, {6, 5}, {40, 5},
	}
	for _, tc := range cases {
		if got := pairSafeCut(msgs, tc.candidate); got != tc.want {
			t.Errorf("pairSafeCut(%d) = %d, want %d", tc.candidate, got, tc.want)
		}
	}
	if got := pairSafeCut(msgs[:1], 1); got != 0 {
		t.Errorf("pairSafeCut on a task-only history = %d, want 0", got)
	}
	if got := pairSafeCut(nil, 1); got != 0 {
		t.Errorf("pairSafeCut on an empty history = %d, want 0", got)
	}
}

func TestMinimalHistory(t *testing.T) {
	msgs := parallelCallHistory(1)
	result, compacted := minimalHistory(msgs)
	if !compacted {
		t.Fatal("expected a compacted history")
	}
	if want := []types.Message{msgs[0], msgs[5], msgs[6]}; !reflect.DeepEqual(result, want) {
		t.Errorf("minimalHistory kept %d messages, want the task and the last exchange", len(result))
	}

	short := msgs[:3]
	if result, compacted := minimalHistory(short); compacted || len(result) != 3 {
		t.Errorf("a single-exchange history must be kept whole, got %d messages (compacted=%v)", len(result), compacted)
	}
}
