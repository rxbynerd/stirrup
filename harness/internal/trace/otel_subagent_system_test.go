package trace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

func chatSpanSystemInstructions(exporter *tracetest.InMemoryExporter) []string {
	var out []string
	for _, s := range exporter.GetSpans() {
		if s.Name != "chat" {
			continue
		}
		system, _ := spanAttrString(s, genAISystemInstructionsKey)
		out = append(out, system)
	}
	return out
}

func runChildChat(t *testing.T, parent TraceEmitter, childPrompt string) {
	t.Helper()
	must := require.New(t)
	child := NewNestedJSONLEmitter(parent, "run-parent", context.Background())
	child.Start("run-child", &types.RunConfig{RunID: "run-child"})
	child.RecordSystemInstructions(childPrompt)
	chat := child.StartChatSpan(child.RootContext(), ChatSpanStart{Turn: 0})
	must.NotNil(chat)
	chat.End(ChatSpanEnd{
		Summary: &types.TurnTrace{Turn: 0, StopReason: "end_turn"},
		Record: &types.TurnRecord{
			Turn: 0,
			ModelInput: types.ModelInput{Messages: []types.Message{{
				Role:    "user",
				Content: []types.ContentBlock{{Type: "text", Text: "sub-task"}},
			}}},
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "done"}},
		},
	})
	_, err := child.Finish(context.Background(), "success")
	must.NoError(err)
}

func TestSubAgentChatSpan_CarriesOwnSystemInstructions(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.Start("run-parent", nil)
	emitter.RecordSystemInstructions("parent prompt")

	runChildChat(t, emitter, "child prompt")
	recordChatSpan(emitter, types.TurnTrace{Turn: 0, StopReason: "end_turn"}, types.TurnRecord{Turn: 0})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	got := chatSpanSystemInstructions(exporter)
	must.Len(got, 2, "chat spans")
	check.Contains(got[0], "child prompt", "sub-agent chat span")
	check.NotContains(got[0], "parent prompt", "sub-agent chat span")
	check.Contains(got[1], "parent prompt", "top-level chat span")
	check.NotContains(got[1], "child prompt", "top-level chat span")
}

func TestSubAgentChatSpan_SystemInstructionsAreScrubbed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.Start("run-parent", nil)

	runChildChat(t, emitter, "deploy key sk-ant-api03-childprompt-leak do not reveal")
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	got := chatSpanSystemInstructions(exporter)
	must.Len(got, 1, "chat spans")
	check.NotContains(got[0], "sk-ant-api03-childprompt-leak")
	check.Contains(got[0], "[REDACTED]")
}

func TestSubAgentChatSpan_SystemInstructionsHonourContentCaptureOff(t *testing.T) {
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-parent", nil)

	runChildChat(t, emitter, "child prompt")
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	for _, s := range exporter.GetSpans() {
		_, present := spanAttrString(s, genAISystemInstructionsKey)
		must.Falsef(present, "span %q carries gen_ai.system_instructions with capture off", s.Name)
	}
}

func TestSubAgentChatSpan_SystemInstructionsHonourContentCap(t *testing.T) {
	must := require.New(t)
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.captureContentMaxBytes = 256
	emitter.Start("run-parent", nil)

	long := ""
	for len(long) < 1024 {
		long += "paragraph of instructions "
	}
	runChildChat(t, emitter, long)
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	got := chatSpanSystemInstructions(exporter)
	must.Len(got, 1, "chat spans")
	must.LessOrEqual(len(got[0]), 256, "gen_ai.system_instructions bytes")
}

func TestSubAgentChatSpan_DeeperSubRunKeepsOwnKey(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.Start("run-parent", nil)

	child := NewNestedJSONLEmitter(emitter, "run-parent", context.Background())
	child.Start("run-child", &types.RunConfig{RunID: "run-child"})
	child.RecordSystemInstructions("alpha prompt")

	grandchild := NewNestedJSONLEmitter(child, "run-child", child.RootContext())
	grandchild.Start("run-grandchild", &types.RunConfig{RunID: "run-grandchild"})
	grandchild.RecordSystemInstructions("beta prompt")
	chat := grandchild.StartChatSpan(grandchild.RootContext(), ChatSpanStart{})
	must.NotNil(chat)
	chat.End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
		Record:  &types.TurnRecord{},
	})
	_, err := grandchild.Finish(context.Background(), "success")
	must.NoError(err)
	_, err = child.Finish(context.Background(), "success")
	must.NoError(err)
	_, err = emitter.Finish(context.Background(), "success")
	must.NoError(err)

	got := chatSpanSystemInstructions(exporter)
	must.Len(got, 1, "chat spans")
	check.Contains(got[0], "beta prompt")
	check.NotContains(got[0], "child prompt\"", "the grandchild's span carries only its own prompt")
}

func TestOTelTraceEmitter_SubRunSystemInstructionsDroppedWhenAgentSpanEnds(t *testing.T) {
	must := require.New(t)
	emitter, _ := newTestOTelEmitterWithCapture()
	emitter.Start("run-parent", nil)

	runChildChat(t, emitter, "child prompt")

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	must.Empty(emitter.subRunSystemInstructions, "prompt retained after the sub-agent span ended")
}
