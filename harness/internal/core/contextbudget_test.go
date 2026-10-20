package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

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
			must := require.New(t)
			var c tokenCalibration
			for _, o := range tc.observations {
				c.observe(o[0], o[1])
			}
			must.InDelta(tc.want, c.factor(), 1e-9)
		})
	}
}

func TestNewTurnBudget_ScalesRoomByRatio(t *testing.T) {
	must := require.New(t)
	window := contextWindow{tokens: 100_000, source: "test window"}
	var c tokenCalibration
	c.observe(10_000, 20_000)

	b := newTurnBudget(window, 20_000, c, 5_000)
	must.Equal(20_000, b.prepare.ReserveForResponse)
	must.Equal(5_000, b.prepare.CurrentTokens)

	room := b.prepare.MaxTokens - b.prepare.ReserveForResponse
	must.Equal(40_000, room,
		"room in estimated tokens: 80000 provider tokens at 2.0 per estimated token")
	must.Contains(b.describeLimit(), "2.00 provider tokens per estimated token")

	neutral := newTurnBudget(window, 20_000, tokenCalibration{}, 5_000)
	must.Equal(window.tokens, neutral.prepare.MaxTokens, "uncalibrated budget uses the window")
	must.NotContains(neutral.describeLimit(), "per estimated token")
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
	must := require.New(t)
	run := func(t *testing.T, factor float64) []types.StreamParams {
		t.Helper()
		must := require.New(t)
		prov := &usageReportingProvider{toolTurns: 8, factor: factor}
		loop := buildTestLoop(nil)
		registerLargeTestTool(t, loop)
		loop.Provider = prov
		config := buildTestConfig()
		config.ContextStrategy.MaxTokens = 6_000

		runTrace, err := loop.Run(context.Background(), config)
		must.NoError(err)
		must.Equal("success", runTrace.Outcome)
		return prov.requests
	}

	matched := firstCompactedRequest(run(t, 1))
	doubled := firstCompactedRequest(run(t, 2))
	must.GreaterOrEqual(matched, 0,
		"the run whose provider matches the estimate never compacted; the budget no longer exercises compaction")
	must.GreaterOrEqual(doubled, 0, "the run whose provider doubles the input never compacted")
	must.Less(doubled, matched,
		"the doubled-input run should compact on an earlier request than the matched-input run")

	// The doubled run's compacted requests fit half the room in estimate.
	reserve := effectiveReserveForResponse(6_000)
	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: reserve + (6_000-reserve)/2, ReserveForResponse: reserve})
	for i, req := range run(t, 2)[doubled:] {
		must.LessOrEqual(requestTokens(req), limit, "request %d is over the calibrated fit limit", doubled+i)
	}
}

// windowResolver registers context windows by "providerType/model".
type windowResolver map[string]int

func (r windowResolver) Resolve(providerType, model string) quirks.ProviderQuirks {
	return quirks.ProviderQuirks{ContextWindow: r[providerType+"/"+model]}
}

func TestEffectiveContextWindow_PicksSmallerNonZeroWindow(t *testing.T) {
	cases := []struct {
		name                 string
		explicit, registered int
		want                 int
	}{
		{"no windows gives zero", 0, 0, 0},
		{"explicit window without a registered one", 100_000, 0, 100_000},
		{"registered window without an explicit one", 0, 128_000, 128_000},
		{"smaller explicit window wins", 64_000, 128_000, 64_000},
		{"larger explicit window is capped by the registered one", 300_000, 128_000, 128_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, effectiveContextWindow(tc.explicit, tc.registered))
		})
	}
}

func TestResolveContextWindow_SourceAndCap(t *testing.T) {
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
			must := require.New(t)
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
			must.Equal(tc.want, got.tokens, "window tokens")
			must.Contains(got.source, tc.wantSource)
			must.Equal(tc.wantCapped, got.cappedFrom, "explicit window that was capped")
		})
	}
}

// TestLoop_RegisteredWindowBoundsEveryRequest pins that a model with a
// 128k registry entry and no contextStrategy.maxTokens never sends a
// request estimated above 128k, though its history grows past that.
func TestLoop_RegisteredWindowBoundsEveryRequest(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)

	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: window, ReserveForResponse: effectiveReserveForResponse(window)})
	for i, req := range prov.requests {
		got := requestTokens(req)
		must.LessOrEqual(got, limit, "request %d is over the fit limit of the %d-token window", i, window)
		must.LessOrEqual(got, window, "request %d is over the %d-token window", i, window)
	}

	last := prov.requests[len(prov.requests)-1]
	full := 6*30_000 + requestTokens(types.StreamParams{System: last.System, Tools: last.Tools})
	must.Greater(full, window, "history never outgrows the window; the test no longer exercises it")
}

// TestSpawnSubAgent_InheritsRegisteredWindow pins that a sub-agent budgets
// against the parent's registered context window: with no
// contextStrategy.maxTokens, a child on a 128k model never sends a request
// above that window's fit limit, though its history grows past it.
func TestSpawnSubAgent_InheritsRegisteredWindow(t *testing.T) {
	must := require.New(t)
	const window = 128_000
	prov := &usageReportingProvider{toolTurns: 6, factor: 1}
	parent := buildTestLoop(nil)
	parent.Provider = prov
	parent.ContextWindows = windowResolver{"anthropic/claude-sonnet-4-6": window}
	parent.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 120_000), nil
		},
	})
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 0

	result, err := SpawnSubAgent(context.Background(), parent, config, SubAgentConfig{Prompt: "Do a subtask"})
	must.NoError(err)
	must.Equal("success", result.Outcome)
	must.Len(prov.requests, 7, "provider requests")

	limit := contextpkg.FitLimit(contextpkg.TokenBudget{MaxTokens: window, ReserveForResponse: effectiveReserveForResponse(window)})
	for i, req := range prov.requests {
		must.LessOrEqual(requestTokens(req), limit,
			"child request %d is over the fit limit of the %d-token window", i, window)
	}
}

// TestLoop_ExplicitWindowAboveRegisteredWarns pins the warning when
// contextStrategy.maxTokens exceeds the model's registered window.
func TestLoop_ExplicitWindowAboveRegisteredWarns(t *testing.T) {
	must := require.New(t)
	prov := &usageReportingProvider{factor: 1}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.ContextWindows = windowResolver{"anthropic/claude-sonnet-4-6": 128_000}
	var events bytes.Buffer
	loop.Transport = transport.NewStdioTransport(&events, &bytes.Buffer{})

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Contains(events.String(),
		"contextStrategy.maxTokens=200000 exceeds the 128000-token context window registered for claude-sonnet-4-6")
}
