package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

func newStallHarness(t *testing.T, script ...[]types.StreamEvent) (*outputLimitHarness, *sdkmetric.ManualReader) {
	t.Helper()
	must := require.New(t)
	h := newOutputLimitHarness(script...)
	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetricsForTesting(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	must.NoError(err)
	h.loop.Metrics = metrics
	return h, reader
}

func counterTotal(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
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
	must := require.New(t)
	n := len(msgs)
	must.True(n >= 2 && msgs[n-2].Role == "assistant",
		"request does not end with an assistant turn followed by its results: %+v", msgs)
	last := msgs[n-1]
	wantTypes := strings.Repeat("tool_result,", len(ids)) + "text"
	must.Equal("user", last.Role, "trailing message: %+v", last)
	must.False(last.Synthetic, "trailing message is synthetic: %+v", last)
	must.Equal(wantTypes, strings.Join(blockTypes(last), ","), "trailing message blocks: %+v", last)
	for i, id := range ids {
		must.Equal(id, last.Content[i].ToolUseID, "tool_result %d", i)
	}
	must.Equal(wantNudge, strings.Join(textBlocks(last), ""), "nudge text")
}

func TestLoop_StallNudge_AlternatingCycleNudgesOnceThenStalls(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	threshold := 2 * stallRepeats
	must.Equal("stalled", runTrace.Outcome)
	calls := h.provider.params()
	must.Len(calls, 2*threshold, "one threshold of requests before the nudge and one after")

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
		t.Run(fmt.Sprintf("request %d", i), func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, nudges, "nudges carried by the request")
		})
	}
	assertAllRequestsPaired(t, calls)

	for i, f := range stallNudgeFlags(h.emitter) {
		t.Run(fmt.Sprintf("turn record %d", i), func(t *testing.T) {
			must := require.New(t)
			must.Equal(i == threshold-1, f, "stallNudge is set only on turn %d", threshold-1)
		})
	}
	must.Equal(int64(1), counterTotal(t, reader, "stirrup.harness.stall_nudges"))
	must.Equal(int64(1), counterTotal(t, reader, "stirrup.harness.stalls"))
	must.Len(stallWarnings(h), 1, "stall warnings")
}

func TestLoop_StallNudge_ModelThatChangesApproachFinishes(t *testing.T) {
	must := require.New(t)
	h, reader := newStallHarness(t, testToolTurn("tc_0"), testToolTurn("tc_1"), testToolTurn("tc_2"), scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	calls := h.provider.params()
	must.Len(calls, 4)
	assertNudgeJoinsToolResults(t, calls[3].Messages, fmt.Sprintf(stallNudgeText, stallRepeats), "tc_2")
	must.Equal([]bool{false, false, true, false}, stallNudgeFlags(h.emitter))
	must.Equal(int64(1), counterTotal(t, reader, "stirrup.harness.stall_nudges"))
	must.Zero(counterTotal(t, reader, "stirrup.harness.stalls"))
}

func TestLoop_StallNudge_KeepsEveryResultOfTheBatch(t *testing.T) {
	must := require.New(t)
	batch := []types.StreamEvent{
		{Type: "tool_call", ID: "tc_a", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_b", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_c", Name: "test_tool", Input: map[string]any{}},
		{Type: "tool_call", ID: "tc_d", Name: "test_tool", Input: map[string]any{}},
		stopWith("tool_use"),
	}
	h, _ := newStallHarness(t, batch, scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	calls := h.provider.params()
	must.Len(calls, 2)
	assertNudgeJoinsToolResults(t, calls[1].Messages, fmt.Sprintf(stallNudgeText, stallRepeats), "tc_a", "tc_b", "tc_c", "tc_d")
	assertAllRequestsPaired(t, calls)
}

func TestLoop_StallNudge_PollWithChangingOutputIsNotNudged(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Zero(counterTotal(t, reader, "stirrup.harness.stall_nudges"),
		"polls whose output changes are not nudged")
}

// largeOutputExecutor streams the same oversized stdout for every command.
type largeOutputExecutor struct {
	executor.Executor
	stdout string
}

func (e largeOutputExecutor) ExecStream(_ context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (*executor.ExecResult, error) {
	if _, err := io.WriteString(stdout, e.stdout); err != nil {
		return nil, err
	}
	return &executor.ExecResult{ExitCode: 1}, nil
}

func TestLoop_StallNudge_RepeatedSpilledCommandOutputStalls(t *testing.T) {
	must := require.New(t)
	const calls = 2 * stallRepeats
	var script [][]types.StreamEvent
	for i := 0; i < calls; i++ {
		script = append(script, []types.StreamEvent{
			{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "run_command", Input: map[string]any{"command": "go test ./..."}},
			stopWith("tool_use"),
		})
	}
	script = append(script, scriptEndTurn)
	h, reader := newStallHarness(t, script...)

	cfg := (types.ToolsConfig{}).EffectiveCommandOutput()
	store, err := commandoutput.New(commandoutput.Options{RunID: "test-run-1", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	must.NoError(err)
	t.Cleanup(func() { _ = store.Close() })
	exec := largeOutputExecutor{stdout: strings.Repeat("FAIL TestX\n", int(cfg.InlineMaxBytes)/10+1)}
	h.loop.Tools.(*tool.Registry).Register(builtins.RunCommandToolWithStore(exec, store, cfg, types.DefaultToolMaxResultBytes))

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	requests := h.provider.params()
	refs := map[string]bool{}
	for _, m := range requests[len(requests)-1].Messages {
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			for _, line := range strings.Split(b.Content, "\n") {
				if strings.HasPrefix(line, "stdout_ref=") {
					refs[line] = true
				}
			}
		}
	}
	must.Len(refs, len(requests)-1, "every call must spill with its own stdout reference")
	must.Equal("stalled", runTrace.Outcome)
	must.Len(requests, calls)
	must.Equal(int64(1), counterTotal(t, reader, "stirrup.harness.stall_nudges"))
	must.Equal([]bool{false, false, true, false, false, false}, stallNudgeFlags(h.emitter),
		"the nudge is sent on the third turn only")
}

func TestLoop_StallNudge_PendingNudgeIsDroppedWhenTheBatchEndsTheRun(t *testing.T) {
	must := require.New(t)
	batch := make([]types.StreamEvent, 0, maxConsecutiveFailures+1)
	for i := 0; i < maxConsecutiveFailures; i++ {
		batch = append(batch, types.StreamEvent{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "broken_tool", Input: map[string]any{}})
	}
	batch = append(batch, stopWith("tool_use"))
	h, reader := newStallHarness(t, batch, scriptEndTurn)
	h.loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "broken_tool",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", fmt.Errorf("broken")
		},
	})

	runTrace, err := h.loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("tool_failures", runTrace.Outcome)
	must.Equal([]bool{false}, stallNudgeFlags(h.emitter), "the queued nudge was never sent")
	must.Zero(counterTotal(t, reader, "stirrup.harness.stall_nudges"))
	must.Empty(stallWarnings(h), "stall warnings")
}
