package core

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/types"
)

func TestTokenCalibration_Observe(t *testing.T) {
	cases := []struct {
		name         string
		observations [][2]int
		want         float64
	}{
		{"no observation applies no correction", nil, 1},
		{"first observation replaces the prior", [][2]int{{2_000, 3_000}}, 1.5},
		{"later observations are smoothed", [][2]int{{2_000, 4_000}, {2_000, 2_000}}, 1.5},
		{"a high ratio is clamped", [][2]int{{2_000, 10_000}}, calibrationMaxRatio},
		{"a low ratio is clamped", [][2]int{{4_000, 100}}, calibrationMinRatio},
		{"a small request is ignored", [][2]int{{calibrationMinEstimate - 1, 5_000}}, 1},
		{"an unreported input is ignored", [][2]int{{2_000, 0}}, 1},
		{"an ignored observation keeps the ratio", [][2]int{{2_000, 3_000}, {10, 10_000}}, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c tokenCalibration
			for _, o := range tc.observations {
				c.observe(o[0], o[1])
			}
			if got := c.factor(); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("factor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewTurnBudget_ScalesRoomByRatio(t *testing.T) {
	window := contextWindow{tokens: 100_000, source: "test window"}
	var c tokenCalibration
	c.observe(10_000, 20_000)

	b := newTurnBudget(window, 20_000, c, 5_000)
	if b.prepare.ReserveForResponse != 20_000 || b.prepare.CurrentTokens != 5_000 {
		t.Fatalf("reserve or current tokens changed: %+v", b.prepare)
	}
	if room := b.prepare.MaxTokens - b.prepare.ReserveForResponse; room != 40_000 {
		t.Errorf("room = %d estimated tokens, want 40000 (80000 provider tokens at 2.0 per estimated token)", room)
	}
	if !strings.Contains(b.describeLimit(), "2.00 provider tokens per estimated token") {
		t.Errorf("describeLimit() does not name the ratio: %q", b.describeLimit())
	}

	neutral := newTurnBudget(window, 20_000, tokenCalibration{}, 5_000)
	if neutral.prepare.MaxTokens != window.tokens {
		t.Errorf("uncalibrated MaxTokens = %d, want the window %d", neutral.prepare.MaxTokens, window.tokens)
	}
	if strings.Contains(neutral.describeLimit(), "per estimated token") {
		t.Errorf("uncalibrated describeLimit() names a ratio: %q", neutral.describeLimit())
	}
}

// usageReportingProvider answers toolTurns tool calls, then ends the run.
// Each response reports factor times the loop's estimate of the request it
// answers as the input token count.
type usageReportingProvider struct {
	mu        sync.Mutex
	toolTurns int
	factor    float64
	requests  []types.StreamParams
}

func (p *usageReportingProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	params.Messages = append([]types.Message(nil), params.Messages...)
	p.requests = append(p.requests, params)
	input := int(p.factor * float64(requestTokens(params)))
	var events []types.StreamEvent
	if i := len(p.requests) - 1; i < p.toolTurns {
		events = []types.StreamEvent{
			{Type: "tool_call", ID: fmt.Sprintf("tc_%d", i), Name: "test_tool", Input: map[string]any{"n": i}},
			{Type: "message_complete", StopReason: "tool_use", InputTokens: input},
		}
	} else {
		events = []types.StreamEvent{{Type: "message_complete", StopReason: "end_turn", InputTokens: input}}
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// firstCompactedRequest returns the index of the first request that sends
// fewer messages than the full history (task plus two per earlier turn),
// or -1 when none does.
func firstCompactedRequest(requests []types.StreamParams) int {
	for i, req := range requests {
		if len(req.Messages) < 1+2*i {
			return i
		}
	}
	return -1
}

// TestLoop_CalibrationCompactsEarlierWhenProviderCountsMore pins that a
// provider reporting twice the estimated input makes the loop compact
// on an earlier turn than a provider reporting the estimate itself.
func TestLoop_CalibrationCompactsEarlierWhenProviderCountsMore(t *testing.T) {
	run := func(t *testing.T, factor float64) []types.StreamParams {
		t.Helper()
		prov := &usageReportingProvider{toolTurns: 8, factor: factor}
		loop := buildTestLoop(nil)
		registerLargeTestTool(t, loop)
		loop.Provider = prov
		config := buildTestConfig()
		config.ContextStrategy.MaxTokens = 6_000

		runTrace, err := loop.Run(context.Background(), config)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if runTrace.Outcome != "success" {
			t.Fatalf("outcome = %q, want success", runTrace.Outcome)
		}
		return prov.requests
	}

	matched := firstCompactedRequest(run(t, 1))
	doubled := firstCompactedRequest(run(t, 2))
	if matched < 0 {
		t.Fatal("the run whose provider matches the estimate never compacted; the budget no longer exercises compaction")
	}
	if doubled < 0 || doubled >= matched {
		t.Fatalf("first compacted request with doubled input = %d, want earlier than %d with matching input", doubled, matched)
	}

	// The doubled run's compacted requests fit half the room in estimate.
	reserve := effectiveReserveForResponse(6_000)
	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: reserve + (6_000-reserve)/2, ReserveForResponse: reserve})
	for i, req := range run(t, 2)[doubled:] {
		if got := requestTokens(req); got > limit {
			t.Errorf("request %d estimates %d tokens, over the calibrated fit limit of %d", doubled+i, got, limit)
		}
	}
}
