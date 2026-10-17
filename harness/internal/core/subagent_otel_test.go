package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

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

	if _, err := parentLoop.Run(context.Background(), parentConfig); err != nil {
		t.Fatalf("parent Run: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one OTel span, got none")
	}

	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		if len(found) != 1 {
			for _, s := range spans {
				t.Logf("  %-32s parent=%s span=%s", s.Name, s.Parent.SpanID(), s.SpanContext.SpanID())
			}
			t.Fatalf("found %d %q spans, want 1", len(found), name)
		}
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	requireParent(t, spawn, root)
	requireParent(t, sub, spawn)

	started := eventsOfType(parentTp, "run_started")
	if len(started) != 1 {
		t.Fatalf("parent transport received %d run_started events, want only the parent run's", len(started))
	}
	if got := started[0]; got.ID != "parent-run-otel" || got.TraceReference.SpanID != root.SpanContext.SpanID().String() {
		t.Errorf("run_started = id %q span %s, want the parent run and its root span %s", got.ID, got.TraceReference.SpanID, root.SpanContext.SpanID())
	}

	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	if len(chats) != 3 {
		t.Fatalf("found %d chat spans, want 3 (two parent requests, one child)", len(chats))
	}
	var childChats int
	for _, chat := range chats {
		agent, _ := spanAttr(chat, "gen_ai.agent.name")
		switch chat.Parent.SpanID() {
		case sub.SpanContext.SpanID():
			childChats++
			if agent != "stirrup.subagent" {
				t.Errorf("child chat gen_ai.agent.name = %q", agent)
			}
		case root.SpanContext.SpanID():
			if agent != "stirrup" {
				t.Errorf("parent chat gen_ai.agent.name = %q", agent)
			}
		default:
			t.Errorf("chat span parented on %s, neither run span", chat.Parent.SpanID())
		}
	}
	if childChats != 1 {
		t.Errorf("found %d chat spans under the sub-agent span, want 1", childChats)
	}
	for _, stream := range spansNamed(spans, "provider.stream") {
		parentIsChat := false
		for _, chat := range chats {
			if stream.Parent.SpanID() == chat.SpanContext.SpanID() {
				parentIsChat = true
			}
		}
		if !parentIsChat {
			t.Errorf("provider.stream %s is not a child of a chat span", stream.SpanContext.SpanID())
		}
	}
	for _, s := range spans {
		if strings.HasPrefix(s.Name, "turn[") || strings.HasPrefix(s.Name, "tool.") || s.Name == "run" {
			t.Errorf("legacy span %q exported", s.Name)
		}
		if s.Parent.SpanID() == root.SpanContext.SpanID() {
			if runID, _ := spanAttr(s, "run.parent_id"); runID != "" {
				t.Errorf("sub-agent span %q attached to the parent root", s.Name)
			}
		}
	}
	if got, _ := spanAttr(sub, "run.parent_id"); got != "parent-run-otel" {
		t.Errorf("sub-agent run.parent_id = %q", got)
	}
	if got, _ := spanAttr(sub, "run.outcome"); got != "success" {
		t.Errorf("sub-agent run.outcome = %q", got)
	}
	// The child's run context descends from the spawn tool call's
	// context, but its turn-level guard spans belong to the child run.
	for _, name := range []string{"guard.pre_turn", "guard.post_turn"} {
		var underSub, underSpawn int
		for _, g := range spansNamed(spans, name) {
			switch g.Parent.SpanID() {
			case sub.SpanContext.SpanID():
				underSub++
			case spawn.SpanContext.SpanID():
				underSpawn++
			}
		}
		if underSub != 1 || underSpawn != 0 {
			t.Errorf("%s: %d under the sub-agent span and %d under the spawn tool span, want 1 and 0", name, underSub, underSpawn)
		}
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
	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

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
	if toolSpan.Name == "" {
		t.Fatal("no execute_tool test_tool span found in exported spans")
	}
	if guardSpan.Name == "" {
		t.Fatal("no guard.pre_tool span found in exported spans")
	}

	if guardSpan.Parent.SpanID() != toolSpan.SpanContext.SpanID() {
		t.Errorf("guard.pre_tool.Parent.SpanID = %s, want execute_tool test_tool SpanID %s",
			guardSpan.Parent.SpanID(), toolSpan.SpanContext.SpanID())
		t.Log("emitted spans (name -> parent.SpanID -> span.SpanID):")
		for _, s := range spans {
			t.Logf("  %-24s  parent=%s  span=%s", s.Name, s.Parent.SpanID(), s.SpanContext.SpanID())
		}
	}
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
