package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestBoundToolResult_WithinBudgetUnchanged(t *testing.T) {
	out := strings.Repeat("a", 1024)
	got, cut := boundToolResult(out, 1024, "read_file")
	if cut || got != out {
		t.Fatalf("output at the budget must pass through unchanged (cut=%v, len=%d)", cut, len(got))
	}
}

func TestBoundToolResult_CutsHeadWithinBudget(t *testing.T) {
	// Three-byte runes force the cut to back off to a rune boundary.
	out := strings.Repeat("€", 10_000)
	const budget = 4096
	got, cut := boundToolResult(out, budget, "grep_files")
	if !cut {
		t.Fatal("expected a cut")
	}
	if len(got) > budget {
		t.Fatalf("bounded result is %d bytes, want <= %d", len(got), budget)
	}
	if !utf8.ValidString(got) {
		t.Fatal("bounded result split a UTF-8 rune")
	}
	head, marker, ok := strings.Cut(got, "\n[tool result truncated: ")
	if !ok {
		t.Fatalf("missing truncation marker: %q", got[len(got)-200:])
	}
	if !strings.HasPrefix(out, head) {
		t.Fatal("bounded result must keep the head of the original output")
	}
	wantCounts := fmt.Sprintf("showing the first %d of %d bytes; the omitted part starts at byte offset %d.", len(head), len(out), len(head))
	if !strings.HasPrefix(marker, wantCounts) {
		t.Fatalf("marker %q does not start with %q", marker, wantCounts)
	}
	if !strings.Contains(marker, "Narrow the request") {
		t.Fatalf("generic marker must carry the generic hint: %q", marker)
	}

	again, cutAgain := boundToolResult(got, budget, "grep_files")
	if cutAgain || again != got {
		t.Fatal("re-bounding an already bounded result must be a no-op")
	}
}

func TestBoundToolResult_ReadFileHint(t *testing.T) {
	got, _ := boundToolResult(strings.Repeat("x\n", 4096), 2048, "read_file")
	if !strings.Contains(got, "read_file, setting start_line") {
		t.Fatalf("read_file marker must say how to read on: %q", got[len(got)-250:])
	}
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
	results, records, stall := loop.planAndDispatch(
		context.Background(),
		config,
		[]types.ToolCall{{ID: "tc1", Name: name, Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic",
		"claude-sonnet-4-6",
	)
	if stall != "" {
		t.Fatalf("unexpected stall outcome %q", stall)
	}
	if len(results) != 1 || len(records) != 1 {
		t.Fatalf("expected 1 result/record, got %d/%d", len(results), len(records))
	}
	return results[0], records[0]
}

func toolResultEventContent(t *testing.T, tr *asyncTestTransport) string {
	t.Helper()
	for _, ev := range tr.Events() {
		if ev.Type == "tool_result" {
			return ev.Content
		}
	}
	t.Fatal("no tool_result event emitted")
	return ""
}

func TestPlanAndDispatch_TruncatesOversizedResultAndDropsEnvelope(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("big", 3<<20, false))

	result, record := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "big")
	if len(result.Content) > types.DefaultToolMaxResultBytes {
		t.Fatalf("result is %d bytes, want <= %d", len(result.Content), types.DefaultToolMaxResultBytes)
	}
	if !strings.Contains(result.Content, fmt.Sprintf("of %d bytes", 3<<20)) {
		t.Fatal("marker must name the original byte count")
	}
	if result.Structured != nil || result.Kind != "" {
		t.Fatalf("truncated result must carry no structured envelope (kind %q, %d bytes)", result.Kind, len(result.Structured))
	}
	if record.Output != result.Content || record.Structured != nil || record.Kind != "" {
		t.Fatal("the turn record must carry the same bounded, envelope-free result")
	}
	if got := toolResultEventContent(t, tr); got != result.Content {
		t.Fatalf("tool_result event carries %d bytes, want the %d-byte bounded text", len(got), len(result.Content))
	}
}

func TestPlanAndDispatch_HonoursConfiguredMaxResultBytes(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("big", 64<<10, false))

	config := &types.RunConfig{Mode: "execution", Tools: types.ToolsConfig{MaxResultBytes: 4096}}
	result, _ := dispatchOne(t, loop, config, "big")
	if len(result.Content) > 4096 {
		t.Fatalf("result is %d bytes, want <= 4096", len(result.Content))
	}
}

func TestPlanAndDispatch_ResultWithinBudgetKeepsEnvelope(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("small", 1024, false))

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "small")
	if len(result.Content) != 1024 || result.Structured == nil || result.Kind != "file_excerpt" {
		t.Fatalf("a result within budget must be delivered unchanged (len %d, kind %q)", len(result.Content), result.Kind)
	}
}

func TestPlanAndDispatch_SelfBoundedResultPassesThrough(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, bigResultTool("self_bounded", 8192, true))

	config := &types.RunConfig{Mode: "execution", Tools: types.ToolsConfig{MaxResultBytes: 4096}}
	result, _ := dispatchOne(t, loop, config, "self_bounded")
	if len(result.Content) != 8192 || result.Structured == nil {
		t.Fatalf("a self-bounded result must not be altered (len %d)", len(result.Content))
	}
}

func TestPlanAndDispatch_TruncatesOversizedErrorResult(t *testing.T) {
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
	if !result.IsError {
		t.Fatal("expected an error result")
	}
	if len(result.Content) > types.DefaultToolMaxResultBytes {
		t.Fatalf("error result is %d bytes, want <= %d", len(result.Content), types.DefaultToolMaxResultBytes)
	}
}

func TestPlanAndDispatch_TruncatesOversizedAsyncResult(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())

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
		t.Errorf("tool_result_request was never emitted")
	}()

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "async_echo")
	if result.IsError {
		t.Fatalf("unexpected error result: %.200s", result.Content)
	}
	if len(result.Content) > types.DefaultToolMaxResultBytes {
		t.Fatalf("async result is %d bytes, want <= %d", len(result.Content), types.DefaultToolMaxResultBytes)
	}
	if !strings.Contains(result.Content, fmt.Sprintf("of %d bytes", 512<<10)) {
		t.Fatal("async marker must name the original byte count")
	}
}
