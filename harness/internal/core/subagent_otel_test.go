package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// multiCallProviderForOTel returns a different stream on each call,
// scripted by the events slice. It mirrors the multiCallProvider used
// elsewhere in core tests but is duplicated here to keep this file
// self-contained — one source of truth lives in loop_test.go but this
// avoids exporting an internal-only helper across files.
type multiCallProviderForOTel struct {
	calls [][]types.StreamEvent
	idx   int
}

func (p *multiCallProviderForOTel) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	events := p.calls[p.idx]
	p.idx++
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestSpawnSubAgent_OTelSpansNestUnderToolSpan pins the sub-agent span
// tree: the child run is an "invoke_agent stirrup.subagent" span under
// the parent's execute_tool spawn_agent span, and the child's chat and
// provider.stream spans nest beneath it rather than under the parent
// root.
func TestSpawnSubAgent_OTelSpansNestUnderToolSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	emitter := tracepkg.NewOTelTraceEmitterForTest(tp)
	tracer := emitter.Tracer()

	// Parent provider: turn 0 spawns the sub-agent; turn 1 ends.
	parentProv := &multiCallProviderForOTel{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_spawn_1", Name: "spawn_agent", Input: map[string]any{
					"prompt": "do a subtask",
				}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done."},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	// Child provider: a single turn that emits text and ends.
	childProv := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent reply."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	registry := tool.NewRegistry()
	// Placeholder spawn_agent; the handler is wired below with a closure
	// that calls SpawnSubAgent directly, so the test does not depend on
	// factory.Build.
	registry.Register(&tool.Tool{
		Name:              "spawn_agent",
		Description:       "Spawn a sub-agent",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler:           nil, // wired below
	})

	parentTp := &recordingTransport{}
	parentLoop := &AgenticLoop{
		Provider:    parentProv,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   parentTp,
		Trace:       emitter,
		Tracer:      tracer,
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
		GuardRail:   &phaseSpecificDenyGuard{denyPhase: "none"},
	}

	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-otel"

	// Uses childProv as the child's provider so the child run completes
	// deterministically without an outbound API call.
	registry.Resolve("spawn_agent").Handler = func(ctx context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
		// SpawnSubAgent reuses parent.Provider for the child; saving and
		// restoring is fine here since the parent has already returned
		// from its current turn before this handler runs.
		origProv := parentLoop.Provider
		parentLoop.Provider = childProv
		defer func() { parentLoop.Provider = origProv }()

		result, err := SpawnSubAgent(ctx, parentLoop, parentConfig, SubAgentConfig{
			Prompt:   args.Prompt,
			MaxTurns: 1,
		})
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(result)
		return string(out), nil
	}

	_, err := parentLoop.Run(context.Background(), parentConfig)
	require.NoError(t, err, "parent Run")

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans, "expected at least one OTel span")

	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		if len(found) != 1 {
			for _, s := range spans {
				t.Logf("  %-32s parent=%s span=%s", s.Name, s.Parent.SpanID(), s.SpanContext.SpanID())
			}
		}
		require.Len(t, found, 1, "spans named %q", name)
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	requireParent(t, spawn, root)
	requireParent(t, sub, spawn)

	started := eventsOfType(parentTp, "run_started")
	require.Len(t, started, 1, "only the parent run should emit run_started")
	require.NotNil(t, started[0].TraceReference, "parent run_started trace reference")
	require.Equal(t, "parent-run-otel", started[0].ID, "run_started id")
	require.Equal(t, root.SpanContext.SpanID().String(), started[0].TraceReference.RunSpanID, "run_started root span id")

	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	require.Len(t, chats, 3, "two parent requests and one child request")

	t.Run("chat spans parent on their run span", func(t *testing.T) {
		var childChats int
		for _, chat := range chats {
			agent, _ := spanAttr(chat, "gen_ai.agent.name")
			switch chat.Parent.SpanID() {
			case sub.SpanContext.SpanID():
				childChats++
				require.Equal(t, "stirrup.subagent", agent, "child chat gen_ai.agent.name")
			case root.SpanContext.SpanID():
				require.Equal(t, "stirrup", agent, "parent chat gen_ai.agent.name")
			default:
				require.Failf(t, "unexpected chat parent", "chat span parented on %s, neither run span", chat.Parent.SpanID())
			}
		}
		require.Equal(t, 1, childChats, "chat spans under the sub-agent span")
	})

	t.Run("provider stream spans nest under chat spans", func(t *testing.T) {
		for _, stream := range spansNamed(spans, "provider.stream") {
			parentIsChat := false
			for _, chat := range chats {
				if stream.Parent.SpanID() == chat.SpanContext.SpanID() {
					parentIsChat = true
				}
			}
			require.True(t, parentIsChat, "provider.stream %s is not a child of a chat span", stream.SpanContext.SpanID())
		}
	})

	t.Run("no legacy span names and no sub-agent span on the parent root", func(t *testing.T) {
		for _, s := range spans {
			legacy := strings.HasPrefix(s.Name, "turn[") || strings.HasPrefix(s.Name, "tool.") || s.Name == "run"
			require.False(t, legacy, "legacy span %q exported", s.Name)
			if s.Parent.SpanID() == root.SpanContext.SpanID() {
				runID, _ := spanAttr(s, "run.parent_id")
				require.Empty(t, runID, "sub-agent span %q attached to the parent root", s.Name)
			}
		}
	})

	t.Run("sub-agent span records run attributes", func(t *testing.T) {
		runParentID, _ := spanAttr(sub, "run.parent_id")
		require.Equal(t, "parent-run-otel", runParentID, "run.parent_id")
		outcome, _ := spanAttr(sub, "run.outcome")
		require.Equal(t, "success", outcome, "run.outcome")
	})

	// The child's run context descends from the spawn tool call's
	// context, but its turn-level guard spans belong to the child run.
	for _, name := range []string{"guard.pre_turn", "guard.post_turn"} {
		t.Run(name, func(t *testing.T) {
			var underSub, underSpawn int
			for _, g := range spansNamed(spans, name) {
				switch g.Parent.SpanID() {
				case sub.SpanContext.SpanID():
					underSub++
				case spawn.SpanContext.SpanID():
					underSpawn++
				}
			}
			require.Equal(t, 1, underSub, "%s spans under the sub-agent span", name)
			require.Zero(t, underSpawn, "%s spans under the spawn tool span", name)
		})
	}
}

// TestLoop_PreToolGuardSpanNestsUnderToolSpan pins that the guard.pre_tool
// span created inside guardCheck is parented to the execute_tool span, not
// the run root, so traces show "tool dispatch -> guard pre-tool denied"
// as a nested operation.
func TestLoop_PreToolGuardSpanNestsUnderToolSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	emitter := tracepkg.NewOTelTraceEmitterForTest(tp)
	tracer := emitter.Tracer()

	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}

	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "test_tool",
		Description:       "A test tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "should not be called", nil
		},
	})

	loop := &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       emitter,
		Tracer:      tracer,
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
		// PreTurn allows turn 0 (so the loop reaches tool dispatch); only
		// the PreTool phase is denied.
		GuardRail: &phaseSpecificDenyGuard{denyPhase: guard.PhasePreTool},
	}

	config := buildTestConfig()
	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err, "Run")

	spans := exporter.GetSpans()

	var toolSpan, guardSpan tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "execute_tool test_tool":
			toolSpan = s
		case "guard.pre_tool":
			guardSpan = s
		}
	}
	require.NotEmpty(t, toolSpan.Name, "no execute_tool test_tool span exported")
	require.NotEmpty(t, guardSpan.Name, "no guard.pre_tool span exported")

	if guardSpan.Parent.SpanID() != toolSpan.SpanContext.SpanID() {
		t.Log("emitted spans (name -> parent.SpanID -> span.SpanID):")
		for _, s := range spans {
			t.Logf("  %-24s  parent=%s  span=%s", s.Name, s.Parent.SpanID(), s.SpanContext.SpanID())
		}
	}
	require.Equal(t, toolSpan.SpanContext.SpanID(), guardSpan.Parent.SpanID(), "guard.pre_tool parent span")
}

// phaseSpecificDenyGuard denies a specific guard phase and allows all
// others.
type phaseSpecificDenyGuard struct {
	denyPhase guard.Phase
}

func (g *phaseSpecificDenyGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	if in.Phase == g.denyPhase {
		return &guard.Decision{Verdict: guard.VerdictDeny, GuardID: "phase-deny", Reason: "denied"}, nil
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "phase-deny"}, nil
}
