package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func sleepingSyncTool(name string, d time.Duration) *tool.Tool {
	return &tool.Tool{
		Name:        name,
		Description: "sleeps then succeeds",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			time.Sleep(d)
			return "ok", nil
		},
	}
}

func TestPlanAndDispatch_AsyncDurationExcludesQueueBehindLaterSyncCalls(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildParallelDispatchLoop(t, tr, asyncEchoTool(), sleepingSyncTool("slow_sync", 60*time.Millisecond))
	calls := []types.ToolCall{
		{ID: "tc_async", Name: "async_echo", Input: json.RawMessage(`{}`)},
		{ID: "tc_sync", Name: "slow_sync", Input: json.RawMessage(`{}`)},
	}
	go fireResponseWhenEmitted(t, tr, "tc_async", 0, "async-ok")

	_, records, outcome := loop.planAndDispatch(context.Background(), configWithMaxParallel(2), calls, &stallDetector{}, "", "")
	require.Empty(t, outcome, "unexpected stall outcome")
	require.GreaterOrEqual(t, records[1].DurationMs, int64(60), "prerequisite: the slow sync call must take at least 60ms")
	require.Less(t, records[0].DurationMs, int64(60), "async call duration must not include the sync call it queued behind")
}

// Every way a call can settle must stamp its finish time, or its
// duration is measured from the zero time and goes negative.
func TestPlanAndDispatch_EverySettlementPathHasADuration(t *testing.T) {
	panicTool := &tool.Tool{
		Name:        "async_will_panic",
		Description: "async tool that panics",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(context.Context, json.RawMessage) (tool.AsyncDispatch, error) {
			panic("synthetic panic")
		},
	}
	cases := []struct {
		name string
		call types.ToolCall
		deny bool
	}{
		{name: "sync success", call: types.ToolCall{ID: "tc", Name: "slow_sync", Input: json.RawMessage(`{}`)}},
		{name: "unknown tool", call: types.ToolCall{ID: "tc", Name: "no_such_tool", Input: json.RawMessage(`{}`)}},
		{name: "invalid arguments", call: types.ToolCall{ID: "tc", Name: "slow_sync", Input: json.RawMessage(`{}`),
			InvalidInput: &types.InvalidToolInput{Raw: `{`, Error: "unexpected end of JSON input"}}},
		{name: "guard deny", call: types.ToolCall{ID: "tc", Name: "slow_sync", Input: json.RawMessage(`{}`)}, deny: true},
		{name: "async panic", call: types.ToolCall{ID: "tc", Name: "async_will_panic", Input: json.RawMessage(`{}`)}},
		{name: "async success", call: types.ToolCall{ID: "tc", Name: "async_echo", Input: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			tr := newAsyncTestTransport()
			loop := buildParallelDispatchLoop(t, tr, sleepingSyncTool("slow_sync", time.Millisecond), asyncEchoTool(), panicTool)
			loop.Tracer = tp.Tracer("test")
			if tc.deny {
				loop.GuardRail = &stubDenyGuardRail{reason: "blocked"}
			}
			if tc.call.Name == "async_echo" {
				go fireResponseWhenEmitted(t, tr, tc.call.ID, 0, "async-ok")
			}

			_, records, _ := loop.planAndDispatch(context.Background(), configWithMaxParallel(1), []types.ToolCall{tc.call}, &stallDetector{}, "", "")
			require.Len(t, records, 1)
			require.GreaterOrEqual(t, records[0].DurationMs, int64(0), "duration must be non-negative")

			spans := exporter.GetSpans()
			var found bool
			for _, s := range spans {
				if s.Name != "execute_tool" && s.Name != "execute_tool "+tc.call.Name {
					continue
				}
				found = true
				require.False(t, s.EndTime.Before(s.StartTime), "span ends at %v, before it starts at %v", s.EndTime, s.StartTime)
			}
			require.True(t, found, "no execute_tool span exported (spans=%v)", spanNames(spans))
		})
	}
}
