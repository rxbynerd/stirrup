package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func fileExcerptBlock(t *testing.T) types.ContentBlock {
	t.Helper()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("y", 40)
	}
	payload, err := json.Marshal(struct {
		Path      string   `json:"path"`
		StartLine int      `json:"start_line"`
		EndLine   int      `json:"end_line"`
		Truncated bool     `json:"truncated"`
		Lines     []string `json:"lines"`
	}{Path: "a.go", StartLine: 1, EndLine: 100, Truncated: true, Lines: lines})
	require.NoError(t, err, "marshal excerpt")
	return types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "tc_1",
		Content:    strings.Repeat("x", 40),
		Structured: payload,
		Kind:       types.ToolResultKindFileExcerpt,
	}
}

// TestEstimateCurrentTokens_CountsWireEnvelope pins that the estimate counts
// the structured view the provider sends: the lean wire envelope when the
// provider sends structured results, nothing when it sends text only, and
// never the full envelope kept in history.
func TestEstimateCurrentTokens_CountsWireEnvelope(t *testing.T) {
	block := fileExcerptBlock(t)
	msgs := []types.Message{{Role: "user", Content: []types.ContentBlock{block}}}

	// 4 (msg) + 3 (block) + 10 (Content) + 1 (ToolUseID)
	const base = 18
	wire := types.ToolResultWireEnvelope(block)
	want := base + len(wire)/tokenEstimationDivisor
	full := base + len(block.Structured)/tokenEstimationDivisor

	t.Run("text-only provider counts no envelope", func(t *testing.T) {
		require.Equal(t, base, estimateCurrentTokens(msgs, false))
	})
	t.Run("structured provider counts the wire envelope", func(t *testing.T) {
		require.Equal(t, want, estimateCurrentTokens(msgs, true), "wire envelope is %d bytes", len(wire))
	})
	t.Run("structured estimate drops the duplicated body", func(t *testing.T) {
		require.Less(t, want, full, "full envelope would give %d", full)
	})
}

// paramsRecordingProvider replays one scripted turn per Stream call and
// records the messages of every request.
type paramsRecordingProvider struct {
	mu       sync.Mutex
	turns    [][]types.StreamEvent
	requests [][]types.Message
}

func (p *paramsRecordingProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := len(p.requests)
	if idx >= len(p.turns) {
		idx = len(p.turns) - 1
	}
	p.requests = append(p.requests, params.Messages)
	events := p.turns[idx]
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func structuredToolRun(t *testing.T, result tool.StructuredResult, support StructuredResultSupport) (*paramsRecordingProvider, *budgetRecordingStrategy, *recordingTraceEmitter) {
	t.Helper()
	prov := &paramsRecordingProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "structured_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	strategy := &budgetRecordingStrategy{inner: loop.Context}
	loop.Context = strategy
	recorder := &recordingTraceEmitter{}
	loop.Trace = recorder
	loop.StructuredResults = support
	registry, ok := loop.Tools.(*tool.Registry)
	require.Truef(t, ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
	registry.Register(&tool.Tool{
		Name:        "structured_tool",
		Description: "returns a structured payload",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		StructuredHandler: func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return result, nil
		},
	})
	config := buildTestConfig()
	config.MaxTurns = 4
	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Len(t, prov.requests, 2, "expected a tool turn and a final turn")
	return prov, strategy, recorder
}

// TestLoop_EstimateFollowsStructuredResultSupport pins that the loop asks
// StructuredResults about each turn's routing selection and counts the
// envelope only when the selected provider sends it.
func TestLoop_EstimateFollowsStructuredResultSupport(t *testing.T) {
	result := tool.StructuredResult{
		Text:       "ok",
		Structured: json.RawMessage(`{"blob":"` + strings.Repeat("z", 40000) + `"}`),
		Kind:       "mcp_tool_result",
	}
	tests := []struct {
		name  string
		sends bool
	}{
		{name: "provider sends the envelope", sends: true},
		{name: "provider sends text only", sends: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			support := func(provider, model string) bool {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, provider+"/"+model)
				return tt.sends
			}
			_, strategy, _ := structuredToolRun(t, result, support)
			require.GreaterOrEqual(t, len(strategy.budgets), 2, "expected at least 2 Prepare calls")
			afterTool := strategy.budgets[1].CurrentTokens
			if tt.sends {
				require.GreaterOrEqual(t, afterTool, 10000, "the 40 KB envelope should be counted")
			} else {
				require.LessOrEqual(t, afterTool, 5000, "the envelope should be left out")
			}
			require.NotEmpty(t, asked, "StructuredResults was never asked")
			require.Equal(t, "anthropic/claude-sonnet-4-6", asked[0], "asked about %v, want the router selection", asked)
		})
	}
}

// TestLoop_FullEnvelopeKeptInHistoryAndTrace pins that only the provider
// wire is lean: the next request's history and the trace's tool-call
// record both carry the tool's complete envelope.
func TestLoop_FullEnvelopeKeptInHistoryAndTrace(t *testing.T) {
	block := fileExcerptBlock(t)
	result := tool.StructuredResult{Text: block.Content, Structured: block.Structured, Kind: block.Kind}
	prov, _, recorder := structuredToolRun(t, result, nil)

	t.Run("history keeps the full envelope", func(t *testing.T) {
		var inHistory json.RawMessage
		for _, msg := range prov.requests[1] {
			for _, b := range msg.Content {
				if b.Type == "tool_result" {
					inHistory = b.Structured
				}
			}
		}
		require.Equal(t, string(block.Structured), string(inHistory), "history envelope differs from the tool's")
	})

	t.Run("trace keeps the full envelope", func(t *testing.T) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		var traced json.RawMessage
		for _, tr := range recorder.turnRecords {
			for _, tc := range tr.ToolCalls {
				traced = tc.Structured
			}
		}
		require.Equal(t, string(block.Structured), string(traced), "trace envelope differs from the tool's")
	})
}

func TestBuildStructuredResultSupport_RoutesByProviderAndModel(t *testing.T) {
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "anthropic"},
		Providers: map[string]types.ProviderConfig{
			"cheap":  {Type: "openai-compatible"},
			"vertex": {Type: "gemini"},
		},
	}
	support := buildStructuredResultSupport(config, quirks.DefaultRegistry())
	tests := []struct {
		name            string
		provider, model string
		want            bool
	}{
		{"empty provider name uses the default provider", "", "claude-sonnet-4-6", true},
		{"anthropic provider sends structured results", "anthropic", "claude-sonnet-4-6", true},
		{"gemini provider sends structured results", "vertex", "gemini-2.5-pro", true},
		{"openai-compatible provider does not send structured results", "cheap", "gpt-4o", false},
		{"openai-responses provider does not send structured results", "openai-responses", "gpt-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, support(tt.provider, tt.model))
		})
	}
}

type structuredCapsResolver quirks.StructuredToolResultCapability

func (r structuredCapsResolver) Resolve(_, _ string) quirks.ProviderQuirks {
	return quirks.ProviderQuirks{StructuredToolResults: quirks.StructuredToolResultCapability(r)}
}

func TestBuildStructuredResultSupport_RequiresAWireShape(t *testing.T) {
	config := &types.RunConfig{Provider: types.ProviderConfig{Type: "anthropic"}}
	tests := []struct {
		name string
		caps quirks.StructuredToolResultCapability
		want bool
	}{
		{"supported without a shape", quirks.StructuredToolResultCapability{Supported: true}, false},
		{"content-block array", quirks.StructuredToolResultCapability{Supported: true, ContentBlockArray: true}, true},
		{"object response", quirks.StructuredToolResultCapability{Supported: true, ObjectResponse: true}, true},
		{"shape without the master switch", quirks.StructuredToolResultCapability{ContentBlockArray: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			support := buildStructuredResultSupport(config, structuredCapsResolver(tt.caps))
			require.Equal(t, tt.want, support("", "m"))
		})
	}
}
