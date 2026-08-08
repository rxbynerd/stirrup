package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

// TestSpawnSubAgent_OTelSpansNestUnderToolSpan pins that spans emitted by
// the sub-agent loop nest under the parent's tool.spawn_agent span, not
// the parent run root: SpawnSubAgent sets the child loop's TraceContext
// to the parent's tool-span ctx so provider.stream/context.prepare/
// permission.check/tool.<name> spans parent correctly. (Turn[N] spans
// synthesized in OTelTraceEmitter.RecordTurn are parented from the
// emitter's rootCtx and are not exercised here; see otel.go:RecordTurn.)
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
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       emitter,
		Tracer:      tracer,
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
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

	var toolSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "tool.spawn_agent" {
			toolSpan = s
			break
		}
	}
	if toolSpan.Name == "" {
		t.Fatal("no tool.spawn_agent span found in exported spans")
	}
	toolSpanID := toolSpan.SpanContext.SpanID()
	if !toolSpanID.IsValid() {
		t.Fatal("tool.spawn_agent span has invalid SpanID")
	}

	// Assert at least one child-loop span (provider.stream / context.prepare,
	// the only ones exercised by the child's single turn) has tool.spawn_agent
	// as its parent, not the parent's run span.
	childSpanNames := map[string]bool{
		"provider.stream": true,
		"context.prepare": true,
	}
	var nestedChildSpans int
	for _, s := range spans {
		if !childSpanNames[s.Name] {
			continue
		}
		if s.Parent.SpanID() == toolSpanID {
			nestedChildSpans++
		}
	}
	if nestedChildSpans == 0 {
		t.Errorf("expected at least one child loop span (provider.stream / context.prepare) parented under tool.spawn_agent; found %d", nestedChildSpans)
		t.Log("tool.spawn_agent SpanID:", toolSpanID)
		t.Log("emitted spans (name → parent.SpanID → span.SpanID):")
		for _, s := range spans {
			t.Logf("  %-24s  parent=%s  span=%s", s.Name, s.Parent.SpanID(), s.SpanContext.SpanID())
		}
	}
}

// TestLoop_PreToolGuardSpanNestsUnderToolSpan pins that the guard.pre_tool
// span created inside guardCheck is parented to the tool.<name> span, not
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
		case "tool.test_tool":
			toolSpan = s
		case "guard.pre_tool":
			guardSpan = s
		}
	}
	if toolSpan.Name == "" {
		t.Fatal("no tool.test_tool span found in exported spans")
	}
	if guardSpan.Name == "" {
		t.Fatal("no guard.pre_tool span found in exported spans")
	}

	if guardSpan.Parent.SpanID() != toolSpan.SpanContext.SpanID() {
		t.Errorf("guard.pre_tool.Parent.SpanID = %s, want tool.test_tool SpanID %s",
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
