package trace

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

type spawnKey struct{}

func TestNestedJSONLEmitter_JSONLParentHasNoLiveSpans(t *testing.T) {
	spawnCtx := context.WithValue(context.Background(), spawnKey{}, "spawn")
	child := NewNestedJSONLEmitter(NewJSONLTraceEmitter(&bytes.Buffer{}, false), "run-p", spawnCtx)
	child.Start("run-c", &types.RunConfig{RunID: "run-c"})

	if got := child.RootContext(); got != spawnCtx {
		t.Error("RootContext without an agent span should return the spawn context")
	}
	if h := child.StartChatSpan(spawnCtx, ChatSpanStart{Model: "m"}); h != nil {
		t.Error("StartChatSpan over a JSONL parent returned a handle")
	}
	if h := child.StartToolSpan(spawnCtx, ToolSpanStart{Name: "read_file"}); h != nil {
		t.Error("StartToolSpan over a JSONL parent returned a handle")
	}
	if _, err := child.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestNestedJSONLEmitter_SubAgentSpanTree(t *testing.T) {
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
	if _, err := child.Finish(context.Background(), "max_turns"); err != nil {
		t.Fatalf("child Finish: %v", err)
	}
	spawn.End(ToolSpanEnd{Summary: &types.ToolCallTrace{Success: true}})
	finishLiveRun(t, parent)

	root := findSpan(t, exporter, "invoke_agent stirrup")
	spawnSpan := findSpan(t, exporter, "execute_tool spawn_agent")
	sub := findSpan(t, exporter, "invoke_agent stirrup.subagent")
	chat := findSpan(t, exporter, "chat m")
	tool := findSpan(t, exporter, "execute_tool read_file")

	for _, pair := range []struct{ child, parent string }{
		{spawnSpan.Name, root.Name}, {sub.Name, spawnSpan.Name}, {chat.Name, sub.Name}, {tool.Name, sub.Name},
	} {
		c, p := findSpan(t, exporter, pair.child), findSpan(t, exporter, pair.parent)
		if c.Parent.SpanID() != p.SpanContext.SpanID() {
			t.Errorf("%s is not a child of %s", pair.child, pair.parent)
		}
	}

	assertAttribute(t, sub, genAIAgentNameKey, agentNameSubAgent)
	assertAttribute(t, sub, "run.id", "run-child")
	assertAttribute(t, sub, "run.parent_id", "run-live")
	assertAttribute(t, sub, "run.mode", "research")
	assertAttribute(t, sub, "run.outcome", "max_turns")
	assertAttribute(t, sub, errorTypeKey, "max_turns")
	if sub.Status.Code != codes.Error {
		t.Errorf("sub-agent status = %v, want Error", sub.Status.Code)
	}
	for _, s := range []string{chat.Name, tool.Name} {
		assertAttribute(t, findSpan(t, exporter, s), genAIAgentNameKey, agentNameSubAgent)
	}

	in, _ := spanAttrString(sub, genAIInputMessagesKey)
	out, _ := spanAttrString(sub, genAIOutputMessagesKey)
	if !strings.Contains(in, "child seed prompt") || !strings.Contains(out, "child final answer") {
		t.Errorf("sub-agent span I/O = %q / %q, want the child's prompt and answer", in, out)
	}
	assertAttributesAbsent(t, chat, genAISystemInstructionsKey)
	rootIn, _ := spanAttrString(root, genAIInputMessagesKey)
	if strings.Contains(rootIn, "child") {
		t.Errorf("child content leaked into the root span: %q", rootIn)
	}
}

func TestNestedJSONLEmitter_SubAgentSpanContentScrubbed(t *testing.T) {
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
	if _, err := child.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("child Finish: %v", err)
	}
	spawn.End(ToolSpanEnd{Summary: &types.ToolCallTrace{Success: true}})
	finishLiveRun(t, parent)

	sub := findSpan(t, exporter, "invoke_agent stirrup.subagent")
	for _, span := range []string{sub.Name, "chat m"} {
		for _, attr := range findSpan(t, exporter, span).Attributes {
			if strings.Contains(attr.Value.String(), liveSecret) {
				t.Errorf("%s attribute %s leaks the secret", span, attr.Key)
			}
		}
	}
	in, _ := spanAttrString(sub, genAIInputMessagesKey)
	out, _ := spanAttrString(sub, genAIOutputMessagesKey)
	if !strings.Contains(in, "[REDACTED") || !strings.Contains(out, "[REDACTED") {
		t.Errorf("sub-agent span I/O = %q / %q, want redaction markers", in, out)
	}
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
	rec := &startRecorder{JSONLTraceEmitter: NewJSONLTraceEmitter(&bytes.Buffer{}, false)}
	child := NewNestedJSONLEmitter(rec, "run-p", context.Background())
	child.Start("run-c", nil)
	child.StartChatSpan(context.Background(), ChatSpanStart{Model: "m"})
	child.StartChatSpan(context.Background(), ChatSpanStart{Model: "m", RunID: "run-g", ParentRunID: "run-c"})
	child.StartToolSpan(context.Background(), ToolSpanStart{Name: "read_file"})

	if len(rec.chats) != 2 || len(rec.tools) != 1 {
		t.Fatalf("forwarded %d chat and %d tool starts, want 2 and 1", len(rec.chats), len(rec.tools))
	}
	if got := rec.chats[0]; got.RunID != "run-c" || got.ParentRunID != "run-p" {
		t.Errorf("untagged chat forwarded as %q/%q, want run-c/run-p", got.RunID, got.ParentRunID)
	}
	if got := rec.chats[1]; got.RunID != "run-g" || got.ParentRunID != "run-c" {
		t.Errorf("pre-tagged chat forwarded as %q/%q, want its own tags", got.RunID, got.ParentRunID)
	}
	if got := rec.tools[0]; got.RunID != "run-c" || got.ParentRunID != "run-p" {
		t.Errorf("tool forwarded as %q/%q, want run-c/run-p", got.RunID, got.ParentRunID)
	}
}

func contextWithSpan(h ToolSpan) context.Context {
	return oteltrace.ContextWithSpan(context.Background(), h.Span())
}
