package context

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	out, err := s.Prepare(context.Background(), messages, budget)
	must.NoError(err)
	must.NoError(checkCompactedHistory(messages, out))
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	must := require.New(t)
	b, err := json.Marshal(v)
	must.NoError(err)
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
	check := assert.New(t)
	must := require.New(t)
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
		must.Len(out, len(msgs), "turn %d: clearing must not drop messages", n)

		cleared := clearedIDs(out)
		for i := max(n-keep, 0); i < n; i++ {
			id := fmt.Sprintf("call_%d", i)
			must.False(cleared[id], "turn %d: result %s is one of the %d most recent", n, id, keep)
		}

		if ev := s.LastCompaction(); ev != nil {
			batches++
			check.GreaterOrEqual(ev.TokensBefore-ev.TokensAfter, clearAtLeast, "turn %d: batch reclaimed too few tokens", n)
			check.NotZero(ev.ToolResultsCleared, "turn %d: batch cleared no results", n)
			check.Equal(len(cleared)-len(clearedIDs(previous)), ev.ToolResultsCleared, "turn %d: event count differs from the prepared history", n)
			check.LessOrEqual(ev.TokensAfter, FitLimit(budget), "turn %d: batch exceeds the fit limit", n)
			check.Equal("clear-tool-results", ev.Strategy, "turn %d: unexpected strategy", n)
		} else if previous != nil {
			want := mustJSON(t, previous)
			got := mustJSON(t, out[:len(previous)])
			must.Equal(want, got, "turn %d: prepared history changed between batches", n)
		}
		previous = out
	}
	must.GreaterOrEqual(batches, 2, "the test needs at least two clearing batches over %d results", results)
}

func TestClearToolResults_FitsWithoutBatch(t *testing.T) {
	check := assert.New(t)
	msgs := toolHistory(5, 400)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 0})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 100_000))
	check.Same(&msgs[0], &out[0], "a history within the budget must be returned as is")
	check.Nil(s.LastCompaction(), "no batch ran, so LastCompaction must be nil")
}

func TestClearToolResults_ExcludedToolsNeverCleared(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NotEmpty(cleared, "nothing was cleared; the budget does not exercise the exclusion")

	// Calls cycle through read_file, bash and web_fetch, so only the calls
	// at indices divisible by 3 use a tool that is not excluded.
	for i := range 12 {
		if i%3 != 0 {
			id := fmt.Sprintf("call_%d", i)
			check.False(cleared[id], "result %s of an excluded tool was cleared", id)
		}
	}
}

func TestClearToolResults_LeavesToolSearchResultsAlone(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	msgs := toolHistory(6, 2000)
	search := []types.ContentBlock{
		{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github"}`)},
		{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: []string{"mcp_github_create_issue"}},
	}
	msgs[1].Content = append(append([]types.ContentBlock(nil), search...), msgs[1].Content...)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 0, ClearAtLeast: 1})

	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))

	must.True(clearedIDs(out)["call_0"], "the budget does not clear the result beside the search")
	must.Equal(msgs[1].Content[:2], out[1].Content[:2])
	check.Equal(search, msgs[1].Content[:2], "the history is not modified")
}

func TestClearToolResults_UnseenResultsNeverCleared(t *testing.T) {
	check := assert.New(t)
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
	check.False(cleared["call_3"], "results the model has not seen were cleared: %v", cleared)
	check.False(cleared["call_extra"], "results the model has not seen were cleared: %v", cleared)
	check.True(cleared["call_0"], "the oldest result was not cleared: %v", cleared)
}

func TestClearToolResults_InsufficientReclaimFallsBack(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	msgs := toolHistory(10, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 1_000_000})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	check.Empty(clearedIDs(out), "a batch that cannot reclaim clearAtLeast must not clear anything")
	must.Less(len(out), len(msgs), "expected the sliding-window fallback to drop exchanges")

	ev := s.LastCompaction()
	must.NotNil(ev, "expected a compaction event for the fallback")
	must.Zero(ev.ToolResultsCleared, "fallback cleared results: event %+v", ev)
	must.Len(ev.Warnings, 1, "fallback should report one warning: event %+v", ev)

	more := toolHistory(11, 2000)
	mustPrepare(t, s, more, clearBudget(more, 0, 2000))
	ev = s.LastCompaction()
	must.NotNil(ev, "expected a compaction event for the second call")
	must.Empty(ev.Warnings, "the fallback warning must be reported once per history, got %+v", ev)
}

// TestClearToolResults_FallbackDropsToTheFitLimit pins that the sliding
// window fallback keeps as much history as the sliding window strategy
// would for the same budget.
func TestClearToolResults_FallbackDropsToTheFitLimit(t *testing.T) {
	msgs := toolHistory(10, 2000)
	for room := 1500; room <= 6000; room += 250 {
		t.Run(fmt.Sprintf("room %d", room), func(t *testing.T) {
			must := require.New(t)
			budget := clearBudget(msgs, 0, room)
			s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 1_000_000})
			out := mustPrepare(t, s, msgs, budget)
			want := mustPrepare(t, NewSlidingWindowStrategy(), msgs, budget)
			must.Len(out, len(want), "the fallback should keep as many messages as the sliding window")
		})
	}
}

func TestClearToolResults_ClearsThenDropsWhenClearingIsNotEnough(t *testing.T) {
	must := require.New(t)
	msgs := toolHistory(10, 2000)
	for i := 1; i < len(msgs); i += 2 {
		msgs[i].Content = append([]types.ContentBlock{{Type: "text", Text: strings.Repeat("p", 3000)}}, msgs[i].Content...)
	}
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 100})
	budget := clearBudget(msgs, 0, 4000)
	out := mustPrepare(t, s, msgs, budget)

	ev := s.LastCompaction()
	must.NotNil(ev, "expected a compaction event")
	must.NotZero(ev.ToolResultsCleared, "expected a batch that cleared results, got event %+v", ev)
	must.Less(len(out), len(msgs), "expected dropped exchanges after the batch, got event %+v", ev)
	must.LessOrEqual(ev.TokensAfter, FitLimit(budget), "tokens after the fallback exceed the fit limit")
}

func TestClearToolResults_NoRoomKeepsMinimalHistory(t *testing.T) {
	must := require.New(t)
	msgs := toolHistory(4, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1})
	budget := clearBudget(msgs, 0, 0)
	out := mustPrepare(t, s, msgs, budget)
	want, _ := minimalHistory(msgs)
	must.Equal(want, out, "with no room the strategy should keep the task and the last exchange")
}

func TestClearToolResults_DoesNotModifyHistory(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	msgs := toolHistory(8, 2000)
	msgs[2].Content[0].Structured = json.RawMessage(`{"lines":1}`)
	msgs[2].Content[0].Kind = "read_file"
	snapshot := mustJSON(t, msgs)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 1})
	out := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 1500))
	must.Equal(snapshot, mustJSON(t, msgs), "Prepare modified the stored history")

	first := out[2].Content[0]
	check.Equal(ClearedToolResultPlaceholder, first.Content, "cleared result kept its content: %+v", first)
	check.Nil(first.Structured, "cleared result kept its structured envelope: %+v", first)
	check.Empty(first.Kind, "cleared result kept its kind: %+v", first)
	check.Equal("call_0", first.ToolUseID, "cleared result lost its pairing: %+v", first)
	check.Equal("tool_result", first.Type, "cleared result lost its pairing: %+v", first)
}

// TestClearToolResults_ReappliesClearsAndForgetsThemForANewTask pins that
// a result stays cleared on later calls even when the history then fits,
// and that a history with a different task starts with nothing cleared.
func TestClearToolResults_ReappliesClearsAndForgetsThemForANewTask(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	msgs := toolHistory(8, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 1, ClearAtLeast: 1})
	first := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 2000))
	must.NotEmpty(clearedIDs(first), "the first call cleared nothing")

	again := mustPrepare(t, s, msgs, clearBudget(msgs, 0, 100_000))
	check.Nil(s.LastCompaction(), "a later call within the budget must not run a new batch")
	check.Equal(mustJSON(t, first), mustJSON(t, again), "a later call within the budget must re-send the same cleared history")

	other := toolHistory(8, 2000)
	other[0] = makeMessage("user", "a different task")
	fresh := mustPrepare(t, s, other, clearBudget(other, 0, 100_000))
	must.Empty(clearedIDs(fresh), "results cleared from another task's history were applied to a new one")
}

// TestClearToolResults_ForcedCompactionClearsMore pins that a second call
// in the same turn with a lower MaxTokens, as the loop's overflow recovery
// makes, clears further results and keeps them cleared afterwards.
func TestClearToolResults_ForcedCompactionClearsMore(t *testing.T) {
	must := require.New(t)
	msgs := toolHistory(20, 2000)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{Keep: 2, ClearAtLeast: 500})
	normal := clearBudget(msgs, 0, 9000)
	before := len(clearedIDs(mustPrepare(t, s, msgs, normal)))

	forced := normal
	forced.MaxTokens = forced.ReserveForResponse + normal.CurrentTokens*70/100/2
	after := len(clearedIDs(mustPrepare(t, s, msgs, forced)))
	must.Greater(after, before, "forced call should clear more results than the normal call")

	got := len(clearedIDs(mustPrepare(t, s, msgs, normal)))
	must.Equal(after, got, "the next normal call should keep the %d results cleared so far", after)
}

func TestClearToolResults_DefaultBatchMinimum(t *testing.T) {
	check := assert.New(t)
	s := NewClearToolResultsStrategy(ClearToolResultsConfig{})
	check.Equal(2250, s.batchMinimum(9000), "default batch minimum for a 9000-token trigger")
	check.Equal(1, s.batchMinimum(1), "default batch minimum for a 1-token trigger")
}
