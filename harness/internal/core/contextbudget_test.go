package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
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

// windowResolver registers context windows by "providerType/model".
type windowResolver map[string]int

func (r windowResolver) Resolve(providerType, model string) quirks.ProviderQuirks {
	return quirks.ProviderQuirks{ContextWindow: r[providerType+"/"+model]}
}

func TestEffectiveContextWindow(t *testing.T) {
	cases := []struct{ explicit, registered, want int }{
		{0, 0, 0},
		{100_000, 0, 100_000},
		{0, 128_000, 128_000},
		{64_000, 128_000, 64_000},
		{300_000, 128_000, 128_000},
	}
	for _, tc := range cases {
		if got := effectiveContextWindow(tc.explicit, tc.registered); got != tc.want {
			t.Errorf("effectiveContextWindow(%d, %d) = %d, want %d", tc.explicit, tc.registered, got, tc.want)
		}
	}
}

func TestResolveContextWindow(t *testing.T) {
	resolver := windowResolver{"openai-compatible/gpt-4o": 128_000}
	cases := []struct {
		name         string
		resolver     capabilityResolver
		explicit     int
		providerName string
		model        string
		want         int
		wantSource   string
		wantCapped   int
	}{
		{"registered window without an explicit one", resolver, 0, "openai-compatible", "gpt-4o", 128_000, "registered for gpt-4o", 0},
		{"a smaller explicit window wins", resolver, 64_000, "openai-compatible", "gpt-4o", 64_000, "contextStrategy.maxTokens=64000", 0},
		{"a larger explicit window is capped", resolver, 300_000, "openai-compatible", "gpt-4o", 128_000, "registered for gpt-4o", 300_000},
		{"a named provider resolves by its type", resolver, 0, "fast", "gpt-4o", 128_000, "registered for gpt-4o", 0},
		{"an empty provider name is the default provider", resolver, 0, "", "gpt-4o", 128_000, "registered for gpt-4o", 0},
		{"a model without an entry keeps the default", resolver, 0, "openai-compatible", "gpt-4.1", defaultMaxContextTokens, "default", 0},
		{"no registry keeps the explicit window", nil, 300_000, "openai-compatible", "gpt-4o", 300_000, "contextStrategy.maxTokens=300000", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := &AgenticLoop{}
			if tc.resolver != nil {
				loop.ContextWindows = tc.resolver
			}
			config := &types.RunConfig{
				Provider:        types.ProviderConfig{Type: "openai-compatible"},
				Providers:       map[string]types.ProviderConfig{"fast": {Type: "openai-compatible"}},
				ContextStrategy: types.ContextStrategyConfig{MaxTokens: tc.explicit},
			}
			got := loop.resolveContextWindow(config, tc.providerName, tc.model)
			if got.tokens != tc.want || !strings.Contains(got.source, tc.wantSource) || got.cappedFrom != tc.wantCapped {
				t.Errorf("resolveContextWindow = %+v, want %d tokens, source containing %q, cappedFrom %d", got, tc.want, tc.wantSource, tc.wantCapped)
			}
		})
	}
}

// TestLoop_RegisteredWindowBoundsEveryRequest pins that a model with a
// 128k registry entry and no contextStrategy.maxTokens never sends a
// request estimated above 128k, though its history grows past that.
func TestLoop_RegisteredWindowBoundsEveryRequest(t *testing.T) {
	const window = 128_000
	prov := &usageReportingProvider{toolTurns: 6, factor: 1}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.ContextWindows = windowResolver{"anthropic/claude-sonnet-4-6": window}
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 120_000), nil
		},
	})
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 0

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: window, ReserveForResponse: effectiveReserveForResponse(window)})
	for i, req := range prov.requests {
		if got := requestTokens(req); got > limit || got > window {
			t.Errorf("request %d estimates %d tokens, over the fit limit %d of the %d-token window", i, got, limit, window)
		}
	}
	last := prov.requests[len(prov.requests)-1]
	if full := 6*30_000 + requestTokens(types.StreamParams{System: last.System, Tools: last.Tools}); full <= window {
		t.Fatalf("history of about %d tokens never outgrows the window; the test no longer exercises it", full)
	}
}

// TestLoop_ExplicitWindowAboveRegisteredWarns pins the warning when
// contextStrategy.maxTokens exceeds the model's registered window.
func TestLoop_ExplicitWindowAboveRegisteredWarns(t *testing.T) {
	prov := &usageReportingProvider{factor: 1}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.ContextWindows = windowResolver{"anthropic/claude-sonnet-4-6": 128_000}
	var events bytes.Buffer
	loop.Transport = transport.NewStdioTransport(&events, &bytes.Buffer{})

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "contextStrategy.maxTokens=200000 exceeds the 128000-token context window registered for claude-sonnet-4-6"
	if !strings.Contains(events.String(), want) {
		t.Errorf("transport events do not contain %q:\n%s", want, events.String())
	}
}
