package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func newStallHarness(t *testing.T, script ...[]types.StreamEvent) (*outputLimitHarness, *sdkmetric.ManualReader) {
	t.Helper()
	h := newOutputLimitHarness(script...)
	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetricsForTesting(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	h.loop.Metrics = metrics
	return h, reader
}

func counterTotal(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func stallWarnings(h *outputLimitHarness) []string {
	var out []string
	for _, ev := range h.transport.Events() {
		if ev.Type == "warning" && strings.Contains(ev.Message, "repeated tool-call pattern") {
			out = append(out, ev.Message)
		}
	}
	return out
}

func stallNudgeFlags(e *turnCapturingEmitter) []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]bool, len(e.records))
	for i, r := range e.records {
		out[i] = r.StallNudge
	}
	return out
}

func testToolTurn(id string) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "tool_call", ID: id, Name: "test_tool", Input: map[string]any{}},
		stopWith("tool_use"),
	}
}

func writeFileTurn(id string) []types.StreamEvent {
	return []types.StreamEvent{writeFileCall(id, "same.txt"), stopWith("tool_use")}
}

// assertNudgeJoinsToolResults checks that msgs ends with one user
// message holding a tool_result for each id, then the nudge text, and
// that it follows an assistant message.
func assertNudgeJoinsToolResults(t *testing.T, msgs []types.Message, wantNudge string, ids ...string) {
	t.Helper()
	n := len(msgs)
	if n < 2 || msgs[n-2].Role != "assistant" {
		t.Fatalf("request does not end with an assistant turn followed by its results: %+v", msgs)
	}
	last := msgs[n-1]
	wantTypes := strings.Repeat("tool_result,", len(ids)) + "text"
	if last.Role != "user" || last.Synthetic || strings.Join(blockTypes(last), ",") != wantTypes {
		t.Fatalf("trailing message = %+v, want one unmarked user message with blocks %s", last, wantTypes)
	}
	for i, id := range ids {
		if last.Content[i].ToolUseID != id {
			t.Errorf("tool_result %d answers %q, want %q", i, last.Content[i].ToolUseID, id)
		}
	}
	if got := strings.Join(textBlocks(last), ""); got != wantNudge {
		t.Errorf("nudge text = %q, want %q", got, wantNudge)
	}
}

func TestLoop_StallNudge_AlternatingCycleNudgesOnceThenStalls(t *testing.T) {
	var script [][]types.StreamEvent
	for i := 0; i < 4*stallRepeats; i++ {
		id := fmt.Sprintf("tc_%d", i)
		if i%2 == 0 {
			script = append(script, testToolTurn(id))
		} else {
			script = append(script, writeFileTurn(id))
		}
	}
	h, reader := newStallHarness(t, script...)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	threshold := 2 * stallRepeats
	if runTrace.Outcome != "stalled" {
		t.Fatalf("Outcome = %q, want stalled", runTrace.Outcome)
	}
	calls := h.provider.params()
	if len(calls) != 2*threshold {
		t.Fatalf("provider calls = %d, want %d: one threshold before the nudge and one after", len(calls), 2*threshold)
	}

	assertNudgeJoinsToolResults(t, calls[threshold].Messages, fmt.Sprintf(stallNudgeText, threshold), fmt.Sprintf("tc_%d", threshold-1))
	for i, c := range calls {
		nudges := 0
		for _, m := range c.Messages {
			for _, text := range textBlocks(m) {
				nudges += strings.Count(text, "repeated the same pattern")
			}
		}
		want := 0
		if i >= threshold {
			want = 1
		}
		if nudges != want {
			t.Errorf("request %d carries %d nudges, want %d", i, nudges, want)
		}
	}
	assertAllRequestsPaired(t, calls)

	flags := stallNudgeFlags(h.emitter)
	for i, f := range flags {
		if f != (i == threshold-1) {
			t.Errorf("turn record %d stallNudge = %v, want true only on turn %d", i, f, threshold-1)
		}
	}
	if got := counterTotal(t, reader, "stirrup.harness.stall_nudges"); got != 1 {
		t.Errorf("stall_nudges = %d, want 1", got)
	}
	if got := counterTotal(t, reader, "stirrup.harness.stalls"); got != 1 {
		t.Errorf("stalls = %d, want 1", got)
	}
	if w := stallWarnings(h); len(w) != 1 {
		t.Errorf("stall warnings = %q, want one", w)
	}
}

func TestLoop_StallNudge_ModelThatChangesApproachFinishes(t *testing.T) {
	h, reader := newStallHarness(t, testToolTurn("tc_0"), testToolTurn("tc_1"), testToolTurn("tc_2"), scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	if len(calls) != 4 {
		t.Fatalf("provider calls = %d, want 4", len(calls))
	}
	assertNudgeJoinsToolResults(t, calls[3].Messages, fmt.Sprintf(stallNudgeText, stallRepeats), "tc_2")
	if got := fmt.Sprint(stallNudgeFlags(h.emitter)); got != "[false false true false]" {
		t.Errorf("stallNudge flags = %s, want [false false true false]", got)
	}
	if got := counterTotal(t, reader, "stirrup.harness.stall_nudges"); got != 1 {
		t.Errorf("stall_nudges = %d, want 1", got)
	}
	if got := counterTotal(t, reader, "stirrup.harness.stalls"); got != 0 {
		t.Errorf("stalls = %d, want 0", got)
	}
}

func TestLoop_StallNudge_KeepsEveryResultOfTheBatch(t *testing.T) {
	batch := []types.StreamEvent{
		{Type: "tool_call", ID: "tc_a", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_b", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_c", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_d", Name: "test_tool", Input: map[string]any{}},
		stopWith("tool_use"),
	}
	h, _ := newStallHarness(t, batch, scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	calls := h.provider.params()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	assertNudgeJoinsToolResults(t, calls[1].Messages, fmt.Sprintf(stallNudgeText, stallRepeats), "tc_a", "tc_b", "tc_c", "tc_d")
	assertAllRequestsPaired(t, calls)
}

func TestLoop_StallNudge_PollWithChangingOutputIsNotNudged(t *testing.T) {
	const polls = 3 * stallRepeats
	var script [][]types.StreamEvent
	for i := 0; i < polls; i++ {
		script = append(script, []types.StreamEvent{
			{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "poll", Input: map[string]any{"job": "build"}},
			stopWith("tool_use"),
		})
	}
	script = append(script, scriptEndTurn)
	h, reader := newStallHarness(t, script...)
	var n atomic.Int64
	h.loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "poll",
		Description: "Report job progress",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"job":{"type":"string"}}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return fmt.Sprintf("progress %d", n.Add(1)), nil
		},
	})

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if got := counterTotal(t, reader, "stirrup.harness.stall_nudges"); got != 0 {
		t.Errorf("stall_nudges = %d, want 0 for polls whose output changes", got)
	}
}
