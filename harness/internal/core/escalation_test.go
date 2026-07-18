package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// fakeCapabilityResolver returns a fixed ToolChoiceCapability for every
// (provider, model) so the native-vs-prompt fallback can be exercised
// without a real quirks registry.
type fakeCapabilityResolver struct {
	cap quirks.ToolChoiceCapability
}

func (f fakeCapabilityResolver) Resolve(_, _ string) quirks.ProviderQuirks {
	return quirks.ProviderQuirks{ToolChoice: f.cap}
}

// sequencedProvider returns a different slice of stream events on each
// successive Stream call and captures the ToolChoice of every call. It
// lets a single loop run see "no-tool answer, then a tool call" without
// any live provider.
type sequencedProvider struct {
	mu          sync.Mutex
	scripts     [][]types.StreamEvent
	call        int
	toolChoices []types.ToolChoiceMode
}

func (p *sequencedProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolChoices = append(p.toolChoices, params.ToolChoice)
	idx := p.call
	if idx >= len(p.scripts) {
		idx = len(p.scripts) - 1
	}
	events := p.scripts[idx]
	p.call++
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *sequencedProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call
}

func (p *sequencedProvider) choiceAt(i int) types.ToolChoiceMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.toolChoices) {
		return types.ToolChoiceAuto
	}
	return p.toolChoices[i]
}

// noToolAnswer is the first-turn missed-tool stream: a final text answer
// with no tool call.
func noToolAnswer() []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "text_delta", Text: "Here is the answer without looking."},
		{Type: "message_complete", StopReason: "end_turn"},
	}
}

// toolCallThenDone is the recovered turn: the model calls a tool.
func toolCallTurn() []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "tool_call", ID: "tc1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use"},
	}
}

// buildEscalationLoop wires a loop with a scripted provider, an escalation
// policy, and a manual-reader metric provider so tests can assert both the
// retry behaviour and the no_tool_when_required emission. A nil policy
// leaves escalation disabled.
func buildEscalationLoop(prov *sequencedProvider, policy EscalationPolicy) (*AgenticLoop, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, _ := observability.NewMetricsForTesting(mp)

	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "test_tool",
		Description:       "A test tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})

	return &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Escalation:  policy,
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     metrics,
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}, reader
}

// buildEscalationLoopNoTools is buildEscalationLoop with an empty tool
// registry so the "no tools available" branch can be exercised.
func buildEscalationLoopNoTools(prov *sequencedProvider, policy EscalationPolicy) *AgenticLoop {
	loop, _ := buildEscalationLoop(prov, policy)
	loop.Tools = tool.NewRegistry()
	return loop
}

func escalationConfig(mode string) *types.RunConfig {
	timeout := 60
	return &types.RunConfig{
		RunID:            "test-escalation",
		Mode:             mode,
		Prompt:           "Fix the bug in the workspace.",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: "/tmp"},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         20,
		Timeout:          &timeout,
	}
}

func countNoToolWhenRequired(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.tool_failures" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "category" &&
						kv.Value.String() == observability.ToolFailureNoToolWhenRequired.String() {
						total += dp.Value
					}
				}
			}
		}
	}
	return total
}

// nativeCaps advertises native required tool choice; promptCaps does not.
var (
	nativeCaps = fakeCapabilityResolver{cap: quirks.ToolChoiceCapability{Supported: true, Required: true}}
	promptCaps = fakeCapabilityResolver{cap: quirks.ToolChoiceCapability{Supported: true, Required: false}}
)

func TestEscalationPolicy_Decide(t *testing.T) {
	base := EscalationInput{
		Mode:           "execution",
		Provider:       "anthropic",
		Model:          "claude-sonnet-4-6",
		StopReason:     "end_turn",
		ToolsAvailable: true,
		PriorToolCalls: 0,
		Turn:           0,
	}
	cases := []struct {
		name     string
		policy   *defaultEscalationPolicy
		mutate   func(*EscalationInput)
		wantKind EscalationKind
	}{
		{
			name:     "native_when_supported",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			wantKind: EscalationNative,
		},
		{
			name:     "prompt_when_native_unsupported",
			policy:   newDefaultEscalationPolicy(1, promptCaps),
			wantKind: EscalationPrompt,
		},
		{
			name:     "no_escalation_when_disabled",
			policy:   newDefaultEscalationPolicy(0, nativeCaps),
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_when_cap_reached",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.EscalationsSoFar = 1 },
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_on_tool_use",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.StopReason = "tool_use" },
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_when_no_tools",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.ToolsAvailable = false },
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_after_prior_tool_calls",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.PriorToolCalls = 1 },
			wantKind: EscalationNone,
		},
		{
			// Turn is not a guard: a later turn with no prior tool calls
			// is still a missed-tool failure and must escalate; only
			// PriorToolCalls > 0 stops it.
			name:     "escalates_on_later_turn_when_no_prior_tool_calls",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.Turn = 2 },
			wantKind: EscalationNative,
		},
		{
			name:     "no_escalation_for_mode_without_requirement",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.Mode = "no-such-mode" },
			wantKind: EscalationNone,
		},
		{
			name:     "planning_mode_escalates",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.Mode = "planning" },
			wantKind: EscalationNative,
		},
		{
			name:     "review_mode_escalates",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.Mode = "review" },
			wantKind: EscalationNative,
		},
		{
			name:     "toil_mode_escalates",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.Mode = "toil" },
			wantKind: EscalationNative,
		},
		{
			name:     "prompt_when_resolver_nil",
			policy:   newDefaultEscalationPolicy(1, nil),
			wantKind: EscalationPrompt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := tc.policy.Decide(in)
			if got.Kind != tc.wantKind {
				t.Errorf("Decide kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Kind == EscalationPrompt && got.PromptMessage == "" {
				t.Error("prompt escalation must carry a non-empty PromptMessage")
			}
			if got.Kind == EscalationNative && got.PromptMessage != "" {
				t.Error("native escalation must not carry a PromptMessage")
			}
		})
	}
}

// TestEscalationKindString pins the wire form of each EscalationKind.
// These strings are written verbatim into the escalation.kind OTel span
// attribute, so a rename of an iota constant that forgot to update
// String() would silently produce a wrong span label. EscalationNone's
// "none" reaches String() only via the default fall-through, so the value
// is asserted explicitly here rather than relying on indirect coverage.
func TestEscalationKindString(t *testing.T) {
	cases := []struct {
		kind EscalationKind
		want string
	}{
		{EscalationNone, "none"},
		{EscalationNative, "native"},
		{EscalationPrompt, "prompt"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("EscalationKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestEscalation_NativeRetryOnMissedTool covers the acceptance criterion:
// a first-turn no-tool answer on a workspace-dependent task with a
// native-capable provider triggers a retry that forces ToolChoiceRequired.
func TestEscalation_NativeRetryOnMissedTool(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() < 2 {
		t.Fatalf("expected at least 2 provider calls (answer + forced retry), got %d", prov.calls())
	}
	// The first call is auto; the retry must force required tool choice.
	if got := prov.choiceAt(0); got != types.ToolChoiceAuto {
		t.Errorf("first call ToolChoice = %v, want auto", got)
	}
	if got := prov.choiceAt(1); got != types.ToolChoiceRequired {
		t.Errorf("retry call ToolChoice = %v, want required", got)
	}
	if n := countNoToolWhenRequired(t, reader); n != 1 {
		t.Errorf("no_tool_when_required count = %d, want 1", n)
	}
}

// TestEscalation_PromptFallback covers the prompt-fallback path: a
// provider that cannot force tool choice natively gets a stronger-prompt
// retry instead, and ToolChoiceRequired is never set on the wire.
func TestEscalation_PromptFallback(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, promptCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() < 2 {
		t.Fatalf("expected a retry, got %d provider calls", prov.calls())
	}
	for i := 0; i < prov.calls(); i++ {
		if prov.choiceAt(i) == types.ToolChoiceRequired {
			t.Errorf("prompt fallback must not force required tool choice; call %d did", i)
		}
	}
	if n := countNoToolWhenRequired(t, reader); n != 1 {
		t.Errorf("no_tool_when_required count = %d, want 1", n)
	}
}

// TestEscalation_DisabledNoRetry pins the OFF-by-default safety: with no
// escalation policy a first-turn no-tool answer is accepted as the final
// answer — no retry, no emission. This is the "legitimate no-tool answer"
// / "bare run unchanged" acceptance case.
func TestEscalation_DisabledNoRetry(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, nil)

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() != 1 {
		t.Errorf("disabled escalation must not retry; got %d provider calls", prov.calls())
	}
	if n := countNoToolWhenRequired(t, reader); n != 0 {
		t.Errorf("no_tool_when_required count = %d, want 0 when disabled", n)
	}
}

// TestEscalation_NoToolsNoRetry pins that escalation never fires when no
// tools are available, even when the policy is enabled — a conceptual
// question on a tool-less run is a legitimate no-tool answer.
func TestEscalation_NoToolsNoRetry(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop := buildEscalationLoopNoTools(prov, newDefaultEscalationPolicy(1, nativeCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() != 1 {
		t.Errorf("no-tools run must not escalate; got %d provider calls", prov.calls())
	}
}

// TestEscalation_ResearchAllowsFetchTool pins the mode-aware policy for
// research: a no-tool first-turn answer is escalated (research expects a
// read/search/fetch first), and a recovered turn that calls a tool is
// accepted. This exercises a read-only mode end-to-end so the recovery is
// proven not to depend on write tools.
func TestEscalation_ResearchAllowsFetchTool(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, promptCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("research")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() < 2 {
		t.Errorf("research mode must escalate a first-turn no-tool answer; got %d calls", prov.calls())
	}
	if n := countNoToolWhenRequired(t, reader); n != 1 {
		t.Errorf("no_tool_when_required count = %d, want 1", n)
	}
}

// TestEscalation_MaxRetryCapEnforced is the unbounded-loop guard at the
// default cap: a model that keeps answering without a tool is escalated
// at most once, bounded solely by EscalationsSoFar >= maxRetries.
func TestEscalation_MaxRetryCapEnforced(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One escalation (cap=1): the original answer + exactly one forced
	// retry. The retry also answers with no tool, but the cap prevents any
	// further escalation, so the run terminates at 2 provider calls.
	if prov.calls() != 2 {
		t.Errorf("cap=1 must yield exactly 2 provider calls (answer + 1 retry), got %d", prov.calls())
	}
	if n := countNoToolWhenRequired(t, reader); n != 1 {
		t.Errorf("no_tool_when_required count = %d, want exactly 1 (cap enforced)", n)
	}
}

// TestEscalation_MaxRetryCapEnforcedAtTwo pins that the cap ceiling is
// reachable: with maxRetries=2 and a model that never calls a tool,
// escalation fires exactly twice (3 provider calls, 2 emissions) before
// the cap stops it.
func TestEscalation_MaxRetryCapEnforcedAtTwo(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(2, nativeCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.calls() != 3 {
		t.Errorf("cap=2 must yield exactly 3 provider calls (answer + 2 retries), got %d", prov.calls())
	}
	if n := countNoToolWhenRequired(t, reader); n != 2 {
		t.Errorf("no_tool_when_required count = %d, want exactly 2 (cap=2 enforced)", n)
	}
	// Every forced retry must carry ToolChoiceRequired (native-capable).
	for i := 1; i < prov.calls(); i++ {
		if prov.choiceAt(i) != types.ToolChoiceRequired {
			t.Errorf("retry call %d ToolChoice = %v, want required", i, prov.choiceAt(i))
		}
	}
}

// TestEscalation_RecoveryStopsAfterToolCall pins that PriorToolCalls, not
// Turn, gates escalation: once a retry calls a tool, recovery stops even
// if the cap would otherwise allow another retry.
func TestEscalation_RecoveryStopsAfterToolCall(t *testing.T) {
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(2, nativeCaps))

	if _, err := loop.Run(context.Background(), escalationConfig("execution")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// answer (escalate) -> tool call (PriorToolCalls becomes 1) ->
	// final no-tool answer accepted (PriorToolCalls > 0 blocks escalation).
	if n := countNoToolWhenRequired(t, reader); n != 1 {
		t.Errorf("no_tool_when_required count = %d, want 1 (recovery stops once a tool is called)", n)
	}
}
