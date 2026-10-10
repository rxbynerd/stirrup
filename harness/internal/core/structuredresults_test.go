package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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
	if err != nil {
		t.Fatalf("marshal excerpt: %v", err)
	}
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
	if got := estimateCurrentTokens(msgs, false); got != base {
		t.Errorf("text-only provider: got %d, want %d", got, base)
	}
	wire := types.ToolResultWireEnvelope(block)
	want := base + len(wire)/tokenEstimationDivisor
	if got := estimateCurrentTokens(msgs, true); got != want {
		t.Errorf("structured provider: got %d, want %d (wire envelope %d bytes)", got, want, len(wire))
	}
	if full := base + len(block.Structured)/tokenEstimationDivisor; want >= full {
		t.Errorf("estimate %d does not drop the duplicated body (full envelope would give %d)", want, full)
	}
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
	if !ok {
		t.Fatalf("test loop Tools is %T, want *tool.Registry", loop.Tools)
	}
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
	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("expected a tool turn and a final turn, got %d requests", len(prov.requests))
	}
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
	for _, sends := range []bool{true, false} {
		var mu sync.Mutex
		var asked []string
		support := func(provider, model string) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, provider+"/"+model)
			return sends
		}
		_, strategy, _ := structuredToolRun(t, result, support)
		if len(strategy.budgets) < 2 {
			t.Fatalf("sends=%t: expected at least 2 Prepare calls, got %d", sends, len(strategy.budgets))
		}
		afterTool := strategy.budgets[1].CurrentTokens
		if sends && afterTool < 10000 {
			t.Errorf("sends=true: CurrentTokens = %d, want the 40 KB envelope counted", afterTool)
		}
		if !sends && afterTool > 5000 {
			t.Errorf("sends=false: CurrentTokens = %d, want the envelope left out", afterTool)
		}
		if len(asked) == 0 || asked[0] != "anthropic/claude-sonnet-4-6" {
			t.Errorf("sends=%t: StructuredResults asked about %v, want the router selection anthropic/claude-sonnet-4-6", sends, asked)
		}
	}
}

// TestLoop_FullEnvelopeKeptInHistoryAndTrace pins that only the provider
// wire is lean: the next request's history and the trace's tool-call
// record both carry the tool's complete envelope.
func TestLoop_FullEnvelopeKeptInHistoryAndTrace(t *testing.T) {
	block := fileExcerptBlock(t)
	result := tool.StructuredResult{Text: block.Content, Structured: block.Structured, Kind: block.Kind}
	prov, _, recorder := structuredToolRun(t, result, nil)

	var inHistory json.RawMessage
	for _, msg := range prov.requests[1] {
		for _, b := range msg.Content {
			if b.Type == "tool_result" {
				inHistory = b.Structured
			}
		}
	}
	if !bytes.Equal(inHistory, block.Structured) {
		t.Errorf("history envelope differs from the tool's\n got: %.120s\nwant: %.120s", inHistory, block.Structured)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var traced json.RawMessage
	for _, tr := range recorder.turnRecords {
		for _, tc := range tr.ToolCalls {
			traced = tc.Structured
		}
	}
	if !bytes.Equal(traced, block.Structured) {
		t.Errorf("trace envelope differs from the tool's\n got: %.120s\nwant: %.120s", traced, block.Structured)
	}
}

func TestBuildStructuredResultSupport(t *testing.T) {
	config := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "anthropic"},
		Providers: map[string]types.ProviderConfig{
			"cheap":  {Type: "openai-compatible"},
			"vertex": {Type: "gemini"},
		},
	}
	support := buildStructuredResultSupport(config, quirks.DefaultRegistry())
	tests := []struct {
		provider, model string
		want            bool
	}{
		{"", "claude-sonnet-4-6", true},
		{"anthropic", "claude-sonnet-4-6", true},
		{"vertex", "gemini-2.5-pro", true},
		{"cheap", "gpt-4o", false},
		{"openai-responses", "gpt-5", false},
	}
	for _, tt := range tests {
		if got := support(tt.provider, tt.model); got != tt.want {
			t.Errorf("support(%q, %q) = %t, want %t", tt.provider, tt.model, got, tt.want)
		}
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
			if got := support("", "m"); got != tt.want {
				t.Errorf("support = %t, want %t", got, tt.want)
			}
		})
	}
}
