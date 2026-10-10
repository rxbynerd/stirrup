package context

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestFitLimit(t *testing.T) {
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
			if got := FitLimit(tc.budget); got != tc.want {
				t.Errorf("FitLimit(%+v) = %d, want %d", tc.budget, got, tc.want)
			}
		})
	}
}

func TestTruncateToolResults_LargeResultFitsSmallBudget(t *testing.T) {
	head := "HEAD:" + strings.Repeat("h", 995)
	tail := strings.Repeat("t", 995) + ":TAIL"
	large := head + strings.Repeat("x", 38_000) + tail
	msgs := append([]types.Message{makeMessage("user", "summarise big.txt")}, toolExchange("call_1", large)...)
	original := cloneHistory(msgs)

	limit := FitLimit(TokenBudget{MaxTokens: 2000, ReserveForResponse: 500})
	excess := historyTokens(msgs) - limit
	if excess <= 0 {
		t.Fatalf("history of %d tokens already fits %d", historyTokens(msgs), limit)
	}

	result, cut := TruncateToolResults(msgs, excess)
	if cut != 1 {
		t.Fatalf("cut = %d, want 1", cut)
	}
	if got := historyTokens(result); got > limit {
		t.Fatalf("truncated history estimates %d tokens, over the limit of %d", got, limit)
	}
	block := result[2].Content[0]
	if block.ToolUseID != "call_1" || block.Type != "tool_result" {
		t.Fatalf("truncated block lost its pairing: %+v", block)
	}
	if !strings.Contains(block.Content, "[...truncated ") {
		t.Errorf("truncated content has no marker:\n%s", block.Content)
	}
	if !strings.HasPrefix(block.Content, "HEAD:") || !strings.HasSuffix(block.Content, ":TAIL") {
		t.Error("truncated content must keep the head and the tail of the result")
	}
	if err := checkCompactedHistory(msgs, result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(msgs, original) {
		t.Fatal("TruncateToolResults modified the stored history")
	}
}

func TestTruncateToolResults_OldestResultFirst(t *testing.T) {
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", strings.Repeat("o", 8000))...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 500)
	if cut != 1 {
		t.Fatalf("cut = %d, want 1", cut)
	}
	if got := result[2].Content[0].Content; !strings.Contains(got, "[...truncated ") {
		t.Error("the oldest result was not truncated")
	}
	if !reflect.DeepEqual(result[4], msgs[4]) {
		t.Error("the newest result was truncated although the oldest covered the excess")
	}
	if drop := historyTokens(msgs) - historyTokens(result); drop < 500 {
		t.Errorf("estimate dropped by %d tokens, want at least 500", drop)
	}
	if drop := historyTokens(msgs) - historyTokens(result); drop > 600 {
		t.Errorf("estimate dropped by %d tokens; the cut should stay close to the excess of 500", drop)
	}
}

func TestTruncateToolResults_SpillsToNewerResults(t *testing.T) {
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", strings.Repeat("o", 2000))...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 1000)
	if cut != 2 {
		t.Fatalf("cut = %d, want 2", cut)
	}
	if drop := historyTokens(msgs) - historyTokens(result); drop < 1000 {
		t.Errorf("estimate dropped by %d tokens, want at least 1000", drop)
	}
	if err := checkCompactedHistory(msgs, result); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateToolResults_CutsLargeResultsBeforeSmallOnes(t *testing.T) {
	reference := "[Full output offloaded to .stirrup/context/turn-2-old.txt — read this file if you need the details]"
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("old", reference)...)
	msgs = append(msgs, toolExchange("new", strings.Repeat("n", 8000))...)

	result, cut := TruncateToolResults(msgs, 1000)
	if cut != 1 {
		t.Fatalf("cut = %d, want 1", cut)
	}
	if !reflect.DeepEqual(result[2], msgs[2]) {
		t.Error("a small older result was cut although a larger result covered the excess")
	}

	result, cut = TruncateToolResults(msgs, 2100)
	if cut != 2 {
		t.Fatalf("cut = %d, want 2 once the large result alone cannot cover the excess", cut)
	}
	if got := result[2].Content[0].Content; !strings.Contains(got, "[...truncated ") {
		t.Errorf("the small result was not cut: %q", got)
	}
}

func TestTruncateToolResults_DropsStructuredEnvelope(t *testing.T) {
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("call_1", strings.Repeat("r", 4000))...)
	msgs[2].Content[0].Structured = json.RawMessage(`{"lines":"` + strings.Repeat("r", 4000) + `"}`)
	msgs[2].Content[0].Kind = "read_file"

	result, cut := TruncateToolResults(msgs, 1200)
	if cut != 1 {
		t.Fatalf("cut = %d, want 1", cut)
	}
	block := result[2].Content[0]
	if block.Structured != nil || block.Kind != "" {
		t.Errorf("a truncated result kept its structured envelope: kind=%q structured=%d bytes", block.Kind, len(block.Structured))
	}
	if msgs[2].Content[0].Structured == nil {
		t.Error("the stored block lost its structured envelope")
	}
}

func TestTruncateToolResults_NothingToDo(t *testing.T) {
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("call_1", strings.Repeat("r", 4000))...)

	for _, excess := range []int{0, -10} {
		result, cut := TruncateToolResults(msgs, excess)
		if cut != 0 || &result[0] != &msgs[0] {
			t.Errorf("excess %d: want the input slice back unchanged, got cut=%d", excess, cut)
		}
	}

	textOnly := []types.Message{makeMessage("user", strings.Repeat("t", 40_000))}
	result, cut := TruncateToolResults(textOnly, 100)
	if cut != 0 || &result[0] != &textOnly[0] {
		t.Errorf("a history without tool results must come back unchanged, got cut=%d", cut)
	}
}

func TestTruncateToolResults_ExcessBeyondAllResults(t *testing.T) {
	msgs := []types.Message{makeMessage("user", "task")}
	msgs = append(msgs, toolExchange("a", strings.Repeat("a", 3000))...)
	msgs = append(msgs, toolExchange("b", strings.Repeat("b", 3000))...)

	result, cut := TruncateToolResults(msgs, 100_000)
	if cut != 2 {
		t.Fatalf("cut = %d, want 2", cut)
	}
	for _, i := range []int{2, 4} {
		got := result[i].Content[0].Content
		if !strings.HasPrefix(got, "\n[...truncated 3000 bytes...]") {
			t.Errorf("message %d: want only the marker left, got %q", i, got)
		}
	}
	if err := checkCompactedHistory(msgs, result); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateHeadTail(t *testing.T) {
	if got := truncateHeadTail("short", 10); got != "short" {
		t.Errorf("content shorter than the kept ends changed: %q", got)
	}
	content := strings.Repeat("a", 100) + strings.Repeat("b", 100)
	got := truncateHeadTail(content, 10)
	if want := strings.Repeat("a", 10) + "\n[...truncated 180 bytes...]\n" + strings.Repeat("b", 10); got != want {
		t.Errorf("truncateHeadTail = %q, want %q", got, want)
	}
	multibyte := strings.Repeat("é", 500) + strings.Repeat("語", 500)
	for keep := range 8 {
		got := truncateHeadTail(multibyte, 100+keep)
		if !utf8.ValidString(got) {
			t.Fatalf("keep=%d: truncation split a multi-byte character", keep)
		}
		if len(got) >= len(multibyte) {
			t.Fatalf("keep=%d: truncation did not shorten the content", keep)
		}
	}
}

// TestTruncateToolResults_GeneratedHistories pins that truncation keeps
// every generated history paired and unmodified, and removes at least the
// requested excess unless every tool result is already down to its marker
// or too small to cut.
func TestTruncateToolResults_GeneratedHistories(t *testing.T) {
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 667))
		msgs := generateHistory(r, r.IntN(14))
		original := cloneHistory(msgs)
		excess := r.IntN(historyTokens(msgs) + 1)

		result, _ := TruncateToolResults(msgs, excess)
		if err := checkCompactedHistory(msgs, result); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !reflect.DeepEqual(msgs, original) {
			t.Fatalf("seed %d: the stored history was modified", seed)
		}
		if len(result) != len(msgs) {
			t.Fatalf("seed %d: truncation changed the message count from %d to %d", seed, len(msgs), len(result))
		}
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
				if !minimal && !small {
					t.Fatalf("seed %d: the excess of %d tokens was not met, yet message %d keeps %d bytes of a tool result", seed, excess, i, len(block.Content))
				}
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
				if err != nil {
					t.Fatalf("seed %d %s: Prepare() error: %v", seed, name, err)
				}
				tokens := overhead + historyTokens(prepared)
				if _, cut := TruncateToolResults(prepared, tokens-limit); cut != 0 {
					t.Fatalf("seed %d %s max=%d: prepared history estimates at %d against a fit limit of %d; %d tool results would be truncated",
						seed, name, budget.MaxTokens, tokens, limit, cut)
				}
			}
		}
	}
}

// TestCompaction_TriggersAtFitLimit pins that every strategy compacts a
// request that fits the room after the response reserve but not FitLimit.
func TestCompaction_TriggersAtFitLimit(t *testing.T) {
	msgs := parallelCallHistory(3)
	budget := TokenBudget{MaxTokens: 1300, CurrentTokens: 1100, ReserveForResponse: 100}
	if historyTokens(msgs) != budget.CurrentTokens || budget.CurrentTokens <= FitLimit(budget) ||
		budget.CurrentTokens > budget.MaxTokens-budget.ReserveForResponse {
		t.Fatalf("history of %d tokens does not sit between FitLimit %d and the room", historyTokens(msgs), FitLimit(budget))
	}
	for name, newStrategy := range compactionStrategies() {
		s := newStrategy()
		if _, err := s.Prepare(context.Background(), msgs, budget); err != nil {
			t.Fatalf("%s: Prepare() error: %v", name, err)
		}
		if s.LastCompaction() == nil {
			t.Errorf("%s did not compact a request over FitLimit", name)
		}
	}
}
