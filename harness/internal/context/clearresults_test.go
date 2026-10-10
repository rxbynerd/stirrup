package context

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// toolHistory is a task followed by n exchanges of one tool call and its
// result of size bytes. Call i is made to names[i%len(names)], or
// read_file when no names are given.
func toolHistory(n, size int, names ...string) []types.Message {
	if len(names) == 0 {
		names = []string{"read_file"}
	}
	messages := []types.Message{makeMessage("user", "task: inspect the repository")}
	for i := range n {
		id := fmt.Sprintf("call_%d", i)
		messages = append(messages,
			types.Message{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: id, Name: names[i%len(names)], Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			types.Message{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: id, Content: fmt.Sprintf("result %d ", i) + strings.Repeat("x", size)},
			}},
		)
	}
	return messages
}

func clearBudget(messages []types.Message, overhead, room int) TokenBudget {
	const reserve = 1000
	return TokenBudget{MaxTokens: reserve + room, CurrentTokens: overhead + estimateHistory(messages), ReserveForResponse: reserve}
}

func mustPrepare(t *testing.T, s ContextStrategy, messages []types.Message, budget TokenBudget) []types.Message {
	t.Helper()
	out, err := s.Prepare(context.Background(), messages, budget)
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if err := checkCompactedHistory(messages, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// clearedIDs returns the tool-use IDs whose result in prepared carries the
// placeholder.
func clearedIDs(prepared []types.Message) map[string]bool {
	ids := map[string]bool{}
	for _, msg := range prepared {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.Content == ClearedToolResultPlaceholder {
				ids[block.ToolUseID] = true
			}
		}
	}
	return ids
}

// TestClearToolResults_StableBetweenBatches drives the strategy over a
// history growing to 40 tool results. Between batches each prepared
// history must start with the previous one byte for byte; every batch must
// reclaim at least clearAtLeast and fit; and the keep most recent results
// are never cleared.
func TestClearToolResults_StableBetweenBatches(t *testing.T) {
	const (
		results      = 40
		keep         = 3
		clearAtLeast = 3000
		overhead     = 800
	)
	full := toolHistory(results, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: keep, ClearAtLeast: clearAtLeast})

	var previous []types.Message
	batches := 0
	for n := 1; n <= results; n++ {
		msgs := full[:1+2*n]
		budget := clearBudget(msgs, overhead, 10_000)
		out := mustPrepare(t, s, msgs, budget)
		if len(out) != len(msgs) {
			t.Fatalf("turn %d: %d messages prepared from %d; clearing must not drop messages here", n, len(out), len(msgs))
		}

		cleared := clearedIDs(out)
		for i := max(n-keep, 0); i < n; i++ {
			if cleared[fmt.Sprintf("call_%d", i)] {
				t.Fatalf("turn %d: result %d is one of the %d most recent and was cleared", n, i, keep)
			}
		}

		if ev := s.LastCompaction(); ev != nil {
			batches++
			if got := ev.TokensBefore - ev.TokensAfter; got < clearAtLeast {
				t.Errorf("turn %d: batch reclaimed %d tokens, want at least %d", n, got, clearAtLeast)
			}
			if ev.ToolResultsCleared == 0 || ev.ToolResultsCleared != len(cleared)-len(clearedIDs(previous)) {
				t.Errorf("turn %d: event reports %d results cleared, prepared history shows %d new", n, ev.ToolResultsCleared, len(cleared)-len(clearedIDs(previous)))
			}
			if ev.TokensAfter > FitLimit(budget) {
				t.Errorf("turn %d: %d tokens after the batch exceed the fit limit %d", n, ev.TokensAfter, FitLimit(budget))
			}
			if ev.Strategy != "clear-tool-results" {
				t.Errorf("turn %d: strategy = %q", n, ev.Strategy)
			}
		} else if previous != nil {
			if got, want := mustJSON(t, out[:len(previous)]), mustJSON(t, previous); got != want {
				t.Fatalf("turn %d: prepared history changed between batches", n)
			}
		}
		previous = out
	}
	if batches < 2 {
		t.Fatalf("%d clearing batches over %d results; the test needs at least two to compare across", batches, results)
	}
}

func TestClearToolResults_FitsWithoutBatch(t *testing.T) {
	msgs := toolHistory(5, 400)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 0})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 100_000))
	if &out[0] != &msgs[0] {
		t.Error("a history within the budget must be returned as is")
	}
	if s.LastCompaction() != nil {
		t.Error("no batch ran, but LastCompaction is set")
	}
}

func TestClearToolResults_ExcludedToolsNeverCleared(t *testing.T) {
	msgs := toolHistory(12, 2000, "read_file", "bash", "web_fetch")
	internal := map[string]string{"bash": "run_command"}
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{
		Keep:         0,
		ClearAtLeast: 1,
		ExcludeTools: []string{"run_command", "web_fetch"},
		InternalName: func(name string) string {
			if v, ok := internal[name]; ok {
				return v
			}
			return name
		},
	})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	cleared := clearedIDs(out)
	if len(cleared) == 0 {
		t.Fatal("nothing was cleared; the budget does not exercise the exclusion")
	}
	for i := range 12 {
		id := fmt.Sprintf("call_%d", i)
		if i%3 != 0 && cleared[id] {
			t.Errorf("result %s of an excluded tool was cleared", id)
		}
	}
}

func TestClearToolResults_UnseenResultsNeverCleared(t *testing.T) {
	msgs := toolHistory(4, 2000)
	msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, types.ContentBlock{
		Type: "tool_result", ToolUseID: "call_extra", Content: strings.Repeat("y", 4000),
	})
	msgs[len(msgs)-2].Content = append(msgs[len(msgs)-2].Content, types.ContentBlock{
		Type: "tool_use", ID: "call_extra", Name: "read_file", Input: json.RawMessage(`{}`),
	})
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 0, ClearAtLeast: 1})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	cleared := clearedIDs(out)
	if cleared["call_3"] || cleared["call_extra"] {
		t.Errorf("results the model has not seen were cleared: %v", cleared)
	}
	if !cleared["call_0"] {
		t.Errorf("the oldest result was not cleared: %v", cleared)
	}
}

func TestClearToolResults_InsufficientReclaimFallsBack(t *testing.T) {
	msgs := toolHistory(10, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 1_000_000})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	if len(clearedIDs(out)) != 0 {
		t.Error("a batch that cannot reclaim clearAtLeast must not clear anything")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("expected the sliding-window fallback to drop exchanges, kept %d of %d", len(out), len(msgs))
	}
	ev := s.LastCompaction()
	if ev == nil || ev.ToolResultsCleared != 0 || len(ev.Warnings) != 1 {
		t.Fatalf("fallback event = %+v, want one warning and no results cleared", ev)
	}

	more := toolHistory(11, 2000)
	mustPrepare(t, s, more, clearBudget(more, 0, 2000))
	if ev := s.LastCompaction(); ev == nil || len(ev.Warnings) != 0 {
		t.Errorf("the fallback warning must be reported once per history, got %+v", ev)
	}
}

// TestClearToolResults_FallbackDropsToTheFitLimit pins that the sliding
// window fallback keeps as much history as the sliding window strategy
// would for the same budget.
func TestClearToolResults_FallbackDropsToTheFitLimit(t *testing.T) {
	msgs := toolHistory(10, 2000)
	for room := 1500; room <= 6000; room += 250 {
		budget := clearBudget(msgs, 0, room)
		s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 1_000_000})
		out := mustPrepare(t, s, msgs, budget)
		want := mustPrepare(t, NewSlidingWindowStrategy(), msgs, budget)
		if len(out) != len(want) {
			t.Errorf("room %d: fallback kept %d messages, the sliding window keeps %d", room, len(out), len(want))
		}
	}
}

func TestClearToolResults_ClearsThenDropsWhenClearingIsNotEnough(t *testing.T) {
	msgs := toolHistory(10, 2000)
	for i := 1; i < len(msgs); i += 2 {
		msgs[i].Content = append([]types.ContentBlock{{Type: "text", Text: strings.Repeat("p", 3000)}}, msgs[i].Content...)
	}
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 100})
	budget := clearBudget(msgs, 0, 4000)
	out := mustPrepare(t, s, msgs, budget)
	ev := s.LastCompaction()
	if ev == nil || ev.ToolResultsCleared == 0 || len(out) >= len(msgs) {
		t.Fatalf("expected a batch followed by dropped exchanges, got %d messages and event %+v", len(out), ev)
	}
	if ev.TokensAfter > FitLimit(budget) {
		t.Errorf("%d tokens after the fallback exceed the fit limit %d", ev.TokensAfter, FitLimit(budget))
	}
}

func TestClearToolResults_NoRoomKeepsMinimalHistory(t *testing.T) {
	msgs := toolHistory(4, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1})
	budget := clearBudget(msgs, 0, 0)
	out := mustPrepare(t, s, msgs, budget)
	want, _ := minimalHistory(msgs)
	if !reflect.DeepEqual(out, want) {
		t.Errorf("with no room the strategy kept %d messages, want the task and the last exchange", len(out))
	}
}

func TestClearToolResults_DoesNotModifyHistory(t *testing.T) {
	msgs := toolHistory(8, 2000)
	msgs[2].Content[0].Structured = json.RawMessage(`{"lines":1}`)
	msgs[2].Content[0].Kind = "read_file"
	snapshot := mustJSON(t, msgs)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 1})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 1500))
	if mustJSON(t, msgs) != snapshot {
		t.Fatal("Prepare modified the stored history")
	}
	first := out[2].Content[0]
	if first.Content != ClearedToolResultPlaceholder || first.Structured != nil || first.Kind != "" {
		t.Errorf("cleared result kept its content or structured envelope: %+v", first)
	}
	if first.ToolUseID != "call_0" || first.Type != "tool_result" {
		t.Errorf("cleared result lost its pairing: %+v", first)
	}
}

// TestClearToolResults_ReappliesClearsAndForgetsThemForANewTask pins that
// a result stays cleared on later calls even when the history then fits,
// and that a history with a different task starts with nothing cleared.
func TestClearToolResults_ReappliesClearsAndForgetsThemForANewTask(t *testing.T) {
	msgs := toolHistory(8, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 1})
	first := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	if len(clearedIDs(first)) == 0 {
		t.Fatal("the first call cleared nothing")
	}

	again := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 100_000))
	if s.LastCompaction() != nil || mustJSON(t, again) != mustJSON(t, first) {
		t.Error("a later call within the budget must re-send the same cleared history without a new batch")
	}

	other := toolHistory(8, 2000)
	other[0] = makeMessage("user", "a different task")
	fresh := mustPrepare(t, s, other, clearBudget(other, 0, 100_000))
	if len(clearedIDs(fresh)) != 0 {
		t.Error("results cleared from another task's history were applied to a new one")
	}
}

// TestClearToolResults_ForcedCompactionClearsMore pins that a second call
// in the same turn with a lower MaxTokens, as the loop's overflow recovery
// makes, clears further results and keeps them cleared afterwards.
func TestClearToolResults_ForcedCompactionClearsMore(t *testing.T) {
	msgs := toolHistory(20, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 500})
	normal := clearBudget(msgs, 0, 9000)
	before := len(clearedIDs(mustPrepare(t, s, msgs, normal)))

	forced := normal
	forced.MaxTokens = forced.ReserveForResponse + normal.CurrentTokens*70/100/2
	after := len(clearedIDs(mustPrepare(t, s, msgs, forced)))
	if after <= before {
		t.Fatalf("forced call cleared %d results in total, normal call %d", after, before)
	}
	if got := len(clearedIDs(mustPrepare(t, s, msgs, normal))); got != after {
		t.Errorf("the next normal call shows %d cleared results, want the %d cleared so far", got, after)
	}
}

func TestClearToolResults_DefaultBatchMinimum(t *testing.T) {
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{})
	if got := s.batchMinimum(9000); got != 2250 {
		t.Errorf("default batch minimum for a 9000-token trigger = %d, want 2250", got)
	}
	if got := s.batchMinimum(1); got != 1 {
		t.Errorf("default batch minimum for a 1-token trigger = %d, want 1", got)
	}
}
