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

// TestSpawnSubAgent_OTelSpansNestUnderToolSpan is the load-bearing test
// for issue #55 acceptance criterion #2: spans emitted by the sub-agent
// loop must hang off the parent's tool.spawn_agent span, not the parent
// run root.
//
// The child loop creates its own provider.stream, context.prepare,
// permission.check, and tool.<name> spans via l.Tracer.Start(
// l.traceCtx(ctx), ...). Those calls resolve the parent context via
// the loop's TraceContext field; SpawnSubAgent now sets that field to
// the parent's tool-span ctx so child spans nest correctly. (The
// turn[N] spans synthesized inside OTelTraceEmitter.RecordTurn are
// parented from the emitter's rootCtx and do not exercise this path,
// so the assertion targets the child loop's directly-emitted spans.)
//
// Setup:
//
//   - parent loop runs through one turn that emits a tool_call(spawn_agent),
//     then a second turn that ends.
//   - the parent dispatches spawn_agent → SpawnSubAgent → child loop.
//   - the child runs one turn that emits text_delta + end_turn.
//   - both parent and child use the same OTel TracerProvider so all spans
//     land in the same in-memory exporter.
//
// Assertion: at least one of the child loop's directly-emitted spans
// (provider.stream, tool.<name>, permission.check, context.prepare)
// has the tool.spawn_agent span's SpanID as its Parent.SpanID. Without
// the ctx threading and TraceContext inheritance shipped in #55, the
// child's spans are rooted at the parent's run span instead.
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
	// Register a placeholder spawn_agent — we replace the handler below
	// with a closure that calls SpawnSubAgent (the production factory
	// does the same at construction time; here we wire it manually so
	// the test does not depend on factory.Build).
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

	// Wire the spawn_agent handler now that parentLoop exists. It mirrors
	// the closure factory.go installs at production build time but uses
	// childProv as the child's provider so the child run completes
	// deterministically without an outbound API call.
	registry.Resolve("spawn_agent").Handler = func(ctx context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
		// Swap the parent's provider for the child during SpawnSubAgent's
		// call: SpawnSubAgent reuses parent.Provider for the child.
		// Saving/restoring is fine because the parent loop has already
		// returned from its current turn before this handler runs.
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

	// Find the parent's tool.spawn_agent span and capture its SpanID.
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

	// Walk all spans created INSIDE the child loop and assert at least
	// one has tool.spawn_agent as its parent. Without the ctx threading
	// + TraceContext inheritance shipped in #55, the child loop's spans
	// are parented under the parent's run span, not under
	// tool.spawn_agent.
	//
	// The child-loop spans we expect to see are:
	//
	//   - provider.stream (per turn)
	//   - context.prepare (per turn)
	//   - tool.<name>     (when the child dispatches a tool — not exercised here)
	//   - permission.check (when the child dispatches a permissioned tool)
	//
	// The child runs one turn here, so provider.stream and
	// context.prepare are the load-bearing checks.
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
