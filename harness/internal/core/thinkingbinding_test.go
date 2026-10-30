package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func TestBindRequestThinking_StripsOnlyUnderADifferentBinding(t *testing.T) {
	readTool := types.ToolDefinition{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	writeTool := types.ToolDefinition{Name: "write_file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	sent := &thinkingBinding{system: "system", tools: []types.ToolDefinition{readTool}}
	history := []types.Message{
		userText("task"),
		{Role: "assistant", Content: []types.ContentBlock{{Type: "thinking", ThoughtSignature: "sig"}, {Type: "text", Text: "done"}}},
		userText("carry on"),
	}
	cases := []struct {
		name      string
		previous  *thinkingBinding
		system    string
		tools     []types.ToolDefinition
		wantSigns []string
	}{
		{name: "nothing sent yet", previous: nil, system: "other", tools: nil, wantSigns: []string{"sig"}},
		{name: "same binding", previous: sent, system: "system", tools: []types.ToolDefinition{readTool}, wantSigns: []string{"sig"}},
		{name: "system prompt differs", previous: sent, system: "other", tools: []types.ToolDefinition{readTool}, wantSigns: nil},
		{name: "tool added", previous: sent, system: "system", tools: []types.ToolDefinition{readTool, writeTool}, wantSigns: nil},
		{name: "tools removed", previous: sent, system: "system", tools: nil, wantSigns: nil},
		{name: "tool schema differs", previous: sent, system: "system", tools: []types.ToolDefinition{{Name: "read_file", InputSchema: json.RawMessage(`{}`)}}, wantSigns: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			loop := buildTestLoop(&mockProvider{})
			loop.requestBinding = tc.previous

			got := loop.bindRequestThinking(context.Background(), tc.system, tc.tools, history)

			check.Equal(tc.wantSigns, thinkingSignatures(got))
			check.Equal([]string{"sig"}, thinkingSignatures(history), "the input history was modified")
			must.NotNil(loop.requestBinding)
			check.True(loop.requestBinding.equal(&thinkingBinding{system: tc.system, tools: tc.tools}), "the request's binding was not recorded")
		})
	}
}

// TestLoop_DropsThinkingWhenTheToolListChangesMidRun pins that a run whose
// tool list grows between turns, as harness-strategy lazy tool loading
// does after a search, replays none of the thinking produced under the
// shorter list, and keeps replaying the thinking produced after it.
func TestLoop_DropsThinkingWhenTheToolListChangesMidRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &thinkingOnCallsProvider{thinkOn: map[int]bool{1: true, 2: true}, tool: "grow_tool"}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = prov
	registry, ok := loop.Tools.(*tool.Registry)
	must.True(ok, "test loop Tools is %T, want *tool.Registry", loop.Tools)
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	grown := false
	registry.Register(&tool.Tool{
		Name:        "grow_tool",
		Description: "registers late_tool on its first call",
		InputSchema: schema,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			if !grown {
				grown = true
				registry.Register(&tool.Tool{Name: "late_tool", Description: "registered mid-run", InputSchema: schema,
					Handler: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
			}
			return "ok", nil
		},
	})

	trace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", trace.Outcome)
	must.Len(prov.sent, 3, "provider requests")
	check.Len(prov.sent[1].Tools, len(prov.sent[0].Tools)+1, "the second request should carry the registered tool")
	check.Empty(thinkingSignatures(prov.sent[1].Messages), "thinking produced under the shorter tool list was replayed")
	check.Equal([]string{"sig-2"}, thinkingSignatures(prov.sent[2].Messages), "thinking produced under an unchanged tool list was dropped")
}
