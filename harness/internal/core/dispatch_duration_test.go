package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
	if outcome != "" {
		t.Fatalf("unexpected stall outcome %q", outcome)
	}
	if records[1].DurationMs < 60 {
		t.Fatalf("prerequisite: slow sync call took %dms, want at least 60ms", records[1].DurationMs)
	}
	if records[0].DurationMs >= 60 {
		t.Errorf("async call recorded %dms, which includes the sync call it queued behind", records[0].DurationMs)
	}
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
			if len(records) != 1 {
				t.Fatalf("got %d records, want 1", len(records))
			}
			if records[0].DurationMs < 0 {
				t.Errorf("DurationMs = %d, want non-negative", records[0].DurationMs)
			}
			spans := exporter.GetSpans()
			var found bool
			for _, s := range spans {
				if s.Name != "execute_tool" && s.Name != "execute_tool "+tc.call.Name {
					continue
				}
				found = true
				if s.EndTime.Before(s.StartTime) {
					t.Errorf("span ends at %v, before it starts at %v", s.EndTime, s.StartTime)
				}
			}
			if !found {
				t.Fatalf("no execute_tool span exported (spans=%v)", spanNames(spans))
			}
		})
	}
}
