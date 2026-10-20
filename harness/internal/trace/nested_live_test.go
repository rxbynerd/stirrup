package trace

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

type spawnKey struct{}

func TestNestedJSONLEmitter_JSONLParentHasNoLiveSpans(t *testing.T) {
	must := require.New(t)
	spawnCtx := context.WithValue(context.Background(), spawnKey{}, "spawn")
	child := NewNestedJSONLEmitter(NewJSONLTraceEmitter(&bytes.Buffer{}, false), "run-p", spawnCtx)
	child.Start("run-c", &types.RunConfig{RunID: "run-c"})

	must.Same(spawnCtx, child.RootContext(), "RootContext without an agent span should return the spawn context")
	must.Nil(child.StartChatSpan(spawnCtx, ChatSpanStart{Model: "m"}), "StartChatSpan over a JSONL parent returned a handle")
	must.Nil(child.StartToolSpan(spawnCtx, ToolSpanStart{Name: "read_file"}), "StartToolSpan over a JSONL parent returned a handle")
	_, err := child.Finish(context.Background(), "success")
	must.NoError(err, "child Finish")
}

func TestNestedJSONLEmitter_SubAgentSpanTree(t *testing.T) {
	must := require.New(t)
	parent, exporter := startLiveRun(t, true)
	parent.RecordSystemInstructions("parent system prompt")
	spawn := parent.StartToolSpan(parent.RootContext(), ToolSpanStart{CallID: "tu-spawn", Name: "spawn_agent", Known: true})
	spawnCtx := contextWithSpan(spawn)

	child := NewNestedJSONLEmitter(parent, "run-live", spawnCtx)
	child.Start("run-child", &types.RunConfig{RunID: "run-child", Mode: "research", Provider: types.ProviderConfig{Type: "anthropic"}})
	childCtx := child.RootContext()

	record := types.TurnRecord{
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "child seed prompt"}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "child final answer"}},
	}
	child.StartChatSpan(childCtx, ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
		Record:  &record,
	})
	child.StartToolSpan(childCtx, ToolSpanStart{Name: "read_file", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Success: true},
	})
	child.RecordTurn(types.TurnTrace{StopReason: "end_turn"})
	_, err := child.Finish(context.Background(), "max_turns")
	must.NoError(err, "child Finish")
	spawn.End(ToolSpanEnd{Summary: &types.ToolCallTrace{Success: true}})
	finishLiveRun(t, parent)

	root := findSpan(t, exporter, "invoke_agent stirrup")
	spawnSpan := findSpan(t, exporter, "execute_tool spawn_agent")
	sub := findSpan(t, exporter, "invoke_agent stirrup.subagent")
	chat := findSpan(t, exporter, "chat m")
	tool := findSpan(t, exporter, "execute_tool read_file")

	links := []struct {
		name, child, parent string
	}{
		{"spawn span under root", spawnSpan.Name, root.Name},
		{"sub-agent span under spawn tool", sub.Name, spawnSpan.Name},
		{"chat span under sub-agent", chat.Name, sub.Name},
		{"tool span under sub-agent", tool.Name, sub.Name},
	}
	for _, link := range links {
		t.Run(link.name, func(t *testing.T) {
			must := require.New(t)
			c := findSpan(t, exporter, link.child)
			p := findSpan(t, exporter, link.parent)
			must.Equal(p.SpanContext.SpanID(), c.Parent.SpanID())
		})
	}

	assertAttribute(t, sub, genAIAgentNameKey, agentNameSubAgent)
	assertAttribute(t, sub, "run.id", "run-child")
	assertAttribute(t, sub, "run.parent_id", "run-live")
	assertAttribute(t, sub, "run.mode", "research")
	assertAttribute(t, sub, "run.outcome", "max_turns")
	assertAttribute(t, sub, errorTypeKey, "max_turns")
	must.Equal(codes.Error, sub.Status.Code, "sub-agent span status")
	for _, s := range []string{chat.Name, tool.Name} {
		assertAttribute(t, findSpan(t, exporter, s), genAIAgentNameKey, agentNameSubAgent)
	}

	in, _ := spanAttrString(sub, genAIInputMessagesKey)
	out, _ := spanAttrString(sub, genAIOutputMessagesKey)
	must.Contains(in, "child seed prompt", "sub-agent span input should hold the child prompt")
	must.Contains(out, "child final answer", "sub-agent span output should hold the child answer")
	assertAttributesAbsent(t, chat, genAISystemInstructionsKey)
	rootIn, _ := spanAttrString(root, genAIInputMessagesKey)
	must.NotContains(rootIn, "child", "child content leaked into the root span")
}

func TestNestedJSONLEmitter_SubAgentSpanContentScrubbed(t *testing.T) {
	must := require.New(t)
	parent, exporter := startLiveRun(t, true)
	spawn := parent.StartToolSpan(parent.RootContext(), ToolSpanStart{CallID: "tu-spawn", Name: "spawn_agent", Known: true})
	child := NewNestedJSONLEmitter(parent, "run-live", contextWithSpan(spawn))
	child.Start("run-child", &types.RunConfig{RunID: "run-child", Mode: "research", Prompt: "seed " + liveSecret})

	record := types.TurnRecord{
		ModelInput: types.ModelInput{Messages: []types.Message{{
			Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "seed " + liveSecret}},
		}}},
		ModelOutput: []types.ContentBlock{{Type: "text", Text: "answer " + liveSecret}},
	}
	child.StartChatSpan(child.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
		Record:  &record,
	})
	_, err := child.Finish(context.Background(), "success")
	must.NoError(err, "child Finish")
	spawn.End(ToolSpanEnd{Summary: &types.ToolCallTrace{Success: true}})
	finishLiveRun(t, parent)

	sub := findSpan(t, exporter, "invoke_agent stirrup.subagent")
	for _, span := range []string{sub.Name, "chat m"} {
		t.Run(span, func(t *testing.T) {
			must := require.New(t)
			for _, attr := range findSpan(t, exporter, span).Attributes {
				must.NotContains(attr.Value.String(), liveSecret, "attribute %s leaks the secret", attr.Key)
			}
		})
	}

	in, _ := spanAttrString(sub, genAIInputMessagesKey)
	out, _ := spanAttrString(sub, genAIOutputMessagesKey)
	must.Contains(in, "[REDACTED", "sub-agent span input should contain a redaction marker")
	must.Contains(out, "[REDACTED", "sub-agent span output should contain a redaction marker")
}

// startRecorder is a parent emitter that records the live-span starts
// forwarded to it.
type startRecorder struct {
	*JSONLTraceEmitter
	chats []ChatSpanStart
	tools []ToolSpanStart
}

func (r *startRecorder) StartChatSpan(_ context.Context, start ChatSpanStart) ChatSpan {
	r.chats = append(r.chats, start)
	return nil
}

func (r *startRecorder) StartToolSpan(_ context.Context, start ToolSpanStart) ToolSpan {
	r.tools = append(r.tools, start)
	return nil
}

func TestNestedJSONLEmitter_ForwardingTagsRunIDs(t *testing.T) {
	must := require.New(t)
	rec := &startRecorder{JSONLTraceEmitter: NewJSONLTraceEmitter(&bytes.Buffer{}, false)}
	child := NewNestedJSONLEmitter(rec, "run-p", context.Background())
	child.Start("run-c", nil)
	child.StartChatSpan(context.Background(), ChatSpanStart{Model: "m"})
	child.StartChatSpan(context.Background(), ChatSpanStart{Model: "m", RunID: "run-g", ParentRunID: "run-c"})
	child.StartToolSpan(context.Background(), ToolSpanStart{Name: "read_file"})

	must.Len(rec.chats, 2, "forwarded chat starts")
	must.Len(rec.tools, 1, "forwarded tool starts")

	t.Run("untagged chat takes the child run IDs", func(t *testing.T) {
		must := require.New(t)
		got := rec.chats[0]
		must.Equal("run-c", got.RunID)
		must.Equal("run-p", got.ParentRunID)
	})
	t.Run("pre-tagged chat keeps its own run IDs", func(t *testing.T) {
		must := require.New(t)
		got := rec.chats[1]
		must.Equal("run-g", got.RunID)
		must.Equal("run-c", got.ParentRunID)
	})
	t.Run("tool takes the child run IDs", func(t *testing.T) {
		must := require.New(t)
		got := rec.tools[0]
		must.Equal("run-c", got.RunID)
		must.Equal("run-p", got.ParentRunID)
	})
}

func contextWithSpan(h ToolSpan) context.Context {
	return oteltrace.ContextWithSpan(context.Background(), h.Span())
}
