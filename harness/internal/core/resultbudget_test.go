package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestBoundToolResult_WithinBudgetUnchanged(t *testing.T) {
	must := require.New(t)
	out := strings.Repeat("a", 1024)
	got, cut := boundToolResult(out, 1024, "read_file")
	must.False(cut, "output at the budget must not be cut")
	must.Equal(out, got, "output at the budget must pass through unchanged")
}

func TestBoundToolResult_CutsHeadWithinBudget(t *testing.T) {
	must := require.New(t)
	// Three-byte runes force the cut to back off to a rune boundary.
	out := strings.Repeat("€", 10_000)
	const budget = 4096
	got, cut := boundToolResult(out, budget, "grep_files")
	must.True(cut, "expected a cut")
	must.LessOrEqual(len(got), budget, "bounded result must fit the budget")
	must.True(utf8.ValidString(got), "bounded result split a UTF-8 rune")

	head, marker, ok := strings.Cut(got, "\n[tool result truncated: ")
	must.True(ok, "missing truncation marker: %q", got[len(got)-200:])
	must.True(strings.HasPrefix(out, head), "bounded result must keep the head of the original output")

	wantCounts := fmt.Sprintf("showing the first %d of %d bytes; the omitted part starts at byte offset %d.", len(head), len(out), len(head))
	must.True(strings.HasPrefix(marker, wantCounts), "marker %q does not start with %q", marker, wantCounts)
	must.Contains(marker, "Narrow the request", "generic marker must carry the generic hint")

	again, cutAgain := boundToolResult(got, budget, "grep_files")
	must.False(cutAgain, "re-bounding an already bounded result must not cut again")
	must.Equal(got, again, "re-bounding an already bounded result must be a no-op")
}

func TestBoundToolResult_ReadFileHint(t *testing.T) {
	must := require.New(t)
	got, _ := boundToolResult(strings.Repeat("x\n", 4096), 2048, "read_file")
	must.Contains(got, "read_file, setting start_line", "read_file marker must say how to read on")
}

// bigResultTool returns a structured tool whose text is size bytes and
// whose envelope repeats it, the shape that duplicates an oversized result
// on the wire.
func bigResultTool(name string, size int, selfBounded bool) *tool.Tool {
	text := strings.Repeat("z", size)
	envelope, _ := json.Marshal(map[string]string{"text": text})
	return &tool.Tool{
		Name:              name,
		Description:       "test tool returning a large result",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		SelfBoundedResult: selfBounded,
		StructuredHandler: func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{Text: text, Structured: envelope, Kind: "file_excerpt"}, nil
		},
	}
}

func dispatchOne(t *testing.T, loop *AgenticLoop, config *types.RunConfig, name string) (types.ToolResult, types.ToolCallRecord) {
	t.Helper()
	must := require.New(t)
	results, records, stall := loop.planAndDispatch(
		context.Background(),
		config,
		[]types.ToolCall{{ID: "tc1", Name: name, Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic",
		"claude-sonnet-4-6",
	)
	must.Empty(stall, "unexpected stall outcome")
	must.Len(results, 1, "one tool result per call")
	must.Len(records, 1, "one tool call record per call")
	return results[0], records[0]
}

func toolResultEventContent(t *testing.T, tr *asyncTestTransport) string {
	t.Helper()
	must := require.New(t)
	for _, ev := range tr.Events() {
		if ev.Type == "tool_result" {
			return ev.Content
		}
	}
	must.Fail("no tool_result event emitted")
	return ""
}

func TestPlanAndDispatch_TruncatesOversizedResultAndDropsEnvelope(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("big", 3<<20, false))

	result, record := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "big")
	must.LessOrEqual(len(result.Content), types.DefaultToolMaxResultBytes, "bounded result must fit the default budget")
	must.Contains(result.Content, fmt.Sprintf("of %d bytes", 3<<20), "marker must name the original byte count")
	must.Nil(result.Structured, "truncated result must carry no structured envelope")
	must.Empty(result.Kind, "truncated result must carry no kind")

	must.Equal(result.Content, record.Output, "the turn record must carry the same bounded result")
	must.Nil(record.Structured, "the turn record must carry no structured envelope")
	must.Empty(record.Kind, "the turn record must carry no kind")

	must.Equal(result.Content, toolResultEventContent(t, tr), "tool_result event must carry the bounded text")
}

func TestPlanAndDispatch_HonoursConfiguredMaxResultBytes(t *testing.T) {
	must := require.New(t)
	const maxResultBytes = 4096
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("big", 64<<10, false))

	config := &types.RunConfig{Mode: "execution", Tools: types.ToolsConfig{MaxResultBytes: maxResultBytes}}
	result, _ := dispatchOne(t, loop, config, "big")
	must.LessOrEqual(len(result.Content), maxResultBytes, "result must fit the configured budget")
}

func TestPlanAndDispatch_ResultWithinBudgetKeepsEnvelope(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("small", 1024, false))

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "small")
	must.Len(result.Content, 1024, "a result within budget must be delivered unchanged")
	must.NotNil(result.Structured, "a result within budget must keep its envelope")
	must.Equal("file_excerpt", result.Kind)
}

func TestPlanAndDispatch_SelfBoundedResultPassesThrough(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("self_bounded", 8192, true))

	config := &types.RunConfig{Mode: "execution", Tools: types.ToolsConfig{MaxResultBytes: 4096}}
	result, _ := dispatchOne(t, loop, config, "self_bounded")
	must.Len(result.Content, 8192, "a self-bounded result must not be altered")
	must.NotNil(result.Structured, "a self-bounded result must keep its envelope")
}

func TestPlanAndDispatch_TruncatesOversizedErrorResult(t *testing.T) {
	must := require.New(t)
	failing := &tool.Tool{
		Name:        "failing",
		Description: "test tool returning a large error",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", fmt.Errorf("%s", strings.Repeat("e", 1<<20))
		},
	}
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, failing)

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "failing")
	must.True(result.IsError, "expected an error result")
	must.LessOrEqual(len(result.Content), types.DefaultToolMaxResultBytes, "error result must fit the default budget")
}

func TestPlanAndDispatch_TruncatesOversizedAsyncResult(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

	// Runs off the test goroutine, so it reports with assert rather than require.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   strings.Repeat("c", 512<<10),
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
		check.Fail("tool_result_request was never emitted")
	}()

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "async_echo")
	must.False(result.IsError, "unexpected error result: %.200s", result.Content)
	must.LessOrEqual(len(result.Content), types.DefaultToolMaxResultBytes, "async result must fit the default budget")
	must.Contains(result.Content, fmt.Sprintf("of %d bytes", 512<<10), "async marker must name the original byte count")
}
