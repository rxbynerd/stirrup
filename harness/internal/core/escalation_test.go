package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	histories   [][]types.Message
}

func (p *sequencedProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolChoices = append(p.toolChoices, params.ToolChoice)
	p.histories = append(p.histories, append([]types.Message(nil), params.Messages...))
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

// historyAt returns the messages sent on call i.
func (p *sequencedProvider) historyAt(i int) []types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.histories) {
		return nil
	}
	return p.histories[i]
}

// noToolAnswer is the first-turn missed-tool stream: a final text answer
// with no tool call.
func noToolAnswer() []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "text_delta", Text: "Here is the answer without looking."},
		{Type: "message_complete", StopReason: "end_turn"},
	}
}

// toolCallTurn is the recovered turn: the model calls a tool.
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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
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

// countNoToolWhenRequired returns the total no_tool_when_required tool
// failures recorded on the metric reader.
func countNoToolWhenRequired(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
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

// TestEscalationPolicy_DecideByInput checks the escalation decision for each
// combination of mode, stop reason, tool availability, and prior history.
func TestEscalationPolicy_DecideByInput(t *testing.T) {
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
			name:     "no_escalation_on_refusal",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.StopReason = "refusal" },
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_on_context_window_exceeded",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.StopReason = "model_context_window_exceeded" },
			wantKind: EscalationNone,
		},
		{
			name:     "no_escalation_on_max_tokens",
			policy:   newDefaultEscalationPolicy(1, nativeCaps),
			mutate:   func(in *EscalationInput) { in.StopReason = "max_tokens" },
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
			must := require.New(t)
			in := base
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := tc.policy.Decide(in)
			must.Equal(tc.wantKind, got.Kind)
			if got.Kind == EscalationPrompt {
				must.NotEmpty(got.PromptMessage, "prompt escalation must carry a PromptMessage")
			}
			if got.Kind == EscalationNative {
				must.Empty(got.PromptMessage, "native escalation must not carry a PromptMessage")
			}
		})
	}
}

// TestEscalationKind_StringMatchesWireForm pins the wire form of each
// EscalationKind. The strings are written verbatim into the escalation.kind
// span attribute.
func TestEscalationKind_StringMatchesWireForm(t *testing.T) {
	cases := []struct {
		name string
		kind EscalationKind
		want string
	}{
		{name: "none", kind: EscalationNone, want: "none"},
		{name: "native", kind: EscalationNative, want: "native"},
		{name: "prompt", kind: EscalationPrompt, want: "prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, tc.kind.String())
		})
	}
}

// TestEscalation_NativeRetryOnMissedTool pins that a first-turn no-tool answer
// on a workspace-dependent task, with a native-capable provider, triggers a
// retry that forces ToolChoiceRequired.
func TestEscalation_NativeRetryOnMissedTool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	must.GreaterOrEqual(prov.calls(), 2, "expected an answer followed by a forced retry")
	// The first call is auto; the retry must force required tool choice.
	check.Equal(types.ToolChoiceAuto, prov.choiceAt(0), "first call")
	check.Equal(types.ToolChoiceRequired, prov.choiceAt(1), "retry call")
	check.Equal(int64(1), countNoToolWhenRequired(t, reader))
}

// TestEscalation_PromptFallback covers the prompt-fallback path: a
// provider that cannot force tool choice natively gets a stronger-prompt
// retry instead, and ToolChoiceRequired is never set on the wire.
func TestEscalation_PromptFallback(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, promptCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	must.GreaterOrEqual(prov.calls(), 2, "expected a retry")
	for i := 0; i < prov.calls(); i++ {
		check.NotEqual(types.ToolChoiceRequired, prov.choiceAt(i),
			"prompt fallback must not force required tool choice on call %d", i)
	}
	check.Equal(int64(1), countNoToolWhenRequired(t, reader))
}

// TestEscalation_DisabledNoRetry pins that with no escalation policy a
// first-turn no-tool answer is accepted as the final answer: no retry and no
// metric emission.
func TestEscalation_DisabledNoRetry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, nil)

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	check.Equal(1, prov.calls(), "disabled escalation must not retry")
	check.Equal(int64(0), countNoToolWhenRequired(t, reader))
}

// TestEscalation_TerminalStopReasonsNoRetry pins that an enabled policy
// accepts a first-turn refusal or full context window as the run outcome
// instead of re-prompting for a tool call.
func TestEscalation_TerminalStopReasonsNoRetry(t *testing.T) {
	// A context-window stop with nothing left to compact ends the run as
	// context_exhausted rather than with the stop reason.
	cases := []struct {
		stop        string
		wantOutcome string
	}{
		{stop: "refusal", wantOutcome: "refusal"},
		{stop: "model_context_window_exceeded", wantOutcome: "context_exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.stop, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			prov := &sequencedProvider{scripts: [][]types.StreamEvent{
				{{Type: "text_delta", Text: "I can't help with that."}, {Type: "message_complete", StopReason: tc.stop}},
				toolCallTurn(),
			}}
			loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))

			runTrace, err := loop.Run(context.Background(), escalationConfig("execution"))
			must.NoError(err)
			check.Equal(1, prov.calls(), "stop must not escalate")
			check.Equal(tc.wantOutcome, runTrace.Outcome)
			check.Equal(int64(0), countNoToolWhenRequired(t, reader))
		})
	}
}

// maxTokensEscalatePolicy escalates only on an output-limit stop.
type maxTokensEscalatePolicy struct{}

func (maxTokensEscalatePolicy) Decide(in EscalationInput) EscalationDecision {
	if in.StopReason != "max_tokens" {
		return EscalationDecision{Kind: EscalationNone}
	}
	return EscalationDecision{Kind: EscalationNative, Reason: "test policy"}
}

// TestEscalation_MaxTokensStopRecoversWithoutEscalating pins that a
// first-turn output-limit stop is continued by the loop's recovery and
// never also escalated, whichever policy is installed.
func TestEscalation_MaxTokensStopRecoversWithoutEscalating(t *testing.T) {
	policies := map[string]EscalationPolicy{
		"default":            newDefaultEscalationPolicy(1, nativeCaps),
		"escalates_on_limit": maxTokensEscalatePolicy{},
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			prov := &sequencedProvider{scripts: [][]types.StreamEvent{
				{{Type: "text_delta", Text: "Partial"}, {Type: "message_complete", StopReason: "max_tokens"}},
				toolCallTurn(),
				{{Type: "text_delta", Text: "Done."}, {Type: "message_complete", StopReason: "end_turn"}},
			}}
			loop, reader := buildEscalationLoop(prov, policy)

			runTrace, err := loop.Run(context.Background(), escalationConfig("execution"))
			must.NoError(err)
			check.Equal("success", runTrace.Outcome)
			check.Equal(types.ToolChoiceAuto, prov.choiceAt(1), "tool choice after the recovery")
			check.Equal(int64(0), countNoToolWhenRequired(t, reader))
		})
	}
}

// TestEscalation_TerminalMaxTokensStopNoRetry pins that the default policy
// accepts an output-limit stop the loop cannot recover from as the run
// outcome instead of re-prompting for a tool call.
func TestEscalation_TerminalMaxTokensStopNoRetry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		{{Type: "text_delta", Text: "Partial"}, {Type: "message_complete", StopReason: "max_tokens"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))
	config := escalationConfig("execution")
	config.MaxTurns = 1

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("max_tokens", runTrace.Outcome)
	check.Equal(int64(0), countNoToolWhenRequired(t, reader))
}

// TestEscalation_NoToolsNoRetry pins that escalation never fires when no
// tools are available, even when the policy is enabled — a conceptual
// question on a tool-less run is a legitimate no-tool answer.
func TestEscalation_NoToolsNoRetry(t *testing.T) {
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop := buildEscalationLoopNoTools(prov, newDefaultEscalationPolicy(1, nativeCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	must.Equal(1, prov.calls(), "no-tools run must not escalate")
}

// TestEscalation_ResearchAllowsFetchTool pins the mode-aware policy for
// research: a no-tool first-turn answer is escalated (research expects a
// read/search/fetch first), and a recovered turn that calls a tool is
// accepted. This exercises a read-only mode end-to-end so the recovery is
// proven not to depend on write tools.
func TestEscalation_ResearchAllowsFetchTool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, promptCaps))

	_, err := loop.Run(context.Background(), escalationConfig("research"))
	must.NoError(err)
	check.GreaterOrEqual(prov.calls(), 2, "research mode must escalate a first-turn no-tool answer")
	check.Equal(int64(1), countNoToolWhenRequired(t, reader))
}

// TestEscalation_MaxRetryCapEnforced is the unbounded-loop guard at the
// default cap: a model that keeps answering without a tool is escalated
// at most once, bounded solely by EscalationsSoFar >= maxRetries.
func TestEscalation_MaxRetryCapEnforced(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, nativeCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	// One escalation (cap=1): the original answer + exactly one forced
	// retry. The retry also answers with no tool, but the cap prevents any
	// further escalation, so the run terminates at 2 provider calls.
	check.Equal(2, prov.calls(), "cap=1 must yield exactly 2 provider calls (answer + 1 retry)")
	check.Equal(int64(1), countNoToolWhenRequired(t, reader))
}

// TestEscalation_MaxRetryCapEnforcedAtTwo pins that the cap ceiling is
// reachable: with maxRetries=2 and a model that never calls a tool,
// escalation fires exactly twice (3 provider calls, 2 emissions) before
// the cap stops it.
func TestEscalation_MaxRetryCapEnforcedAtTwo(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(2, nativeCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	check.Equal(3, prov.calls(), "cap=2 must yield exactly 3 provider calls (answer + 2 retries)")
	check.Equal(int64(2), countNoToolWhenRequired(t, reader))
	// Every forced retry must carry ToolChoiceRequired (native-capable).
	for i := 1; i < prov.calls(); i++ {
		check.Equal(types.ToolChoiceRequired, prov.choiceAt(i), "retry call %d", i)
	}
}

// TestEscalation_RecoveryStopsAfterToolCall pins that PriorToolCalls, not
// Turn, gates escalation: once a retry calls a tool, recovery stops even
// if the cap would otherwise allow another retry.
func TestEscalation_RecoveryStopsAfterToolCall(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &sequencedProvider{scripts: [][]types.StreamEvent{
		noToolAnswer(),
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, reader := buildEscalationLoop(prov, newDefaultEscalationPolicy(2, nativeCaps))

	_, err := loop.Run(context.Background(), escalationConfig("execution"))
	must.NoError(err)
	// answer (escalate) -> tool call (PriorToolCalls becomes 1) ->
	// final no-tool answer accepted (PriorToolCalls > 0 blocks escalation).
	check.Equal(int64(1), countNoToolWhenRequired(t, reader), "recovery stops once a tool is called")
}

// TestEscalationPolicy_NativeAddsUserTurnWhenTrailingAssistantRejected
// pins that the native decision carries a user message only for a
// provider that rejects a history ending on an assistant message.
func TestEscalationPolicy_NativeAddsUserTurnWhenTrailingAssistantRejected(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	in := EscalationInput{
		Mode:           "execution",
		Provider:       "anthropic",
		Model:          "claude-sonnet-4-6",
		StopReason:     "end_turn",
		ToolsAvailable: true,
	}
	rejecting := fakeCapabilityResolver{cap: quirks.ToolChoiceCapability{Supported: true, Required: true, RejectsTrailingAssistant: true}}
	got := newDefaultEscalationPolicy(1, rejecting).Decide(in)
	must.Equal(EscalationNative, got.Kind)
	check.NotEmpty(got.PromptMessage, "a provider rejecting a trailing assistant message needs a PromptMessage")

	accepting := newDefaultEscalationPolicy(1, nativeCaps).Decide(in)
	check.Empty(accepting.PromptMessage, "a provider accepting a trailing assistant message needs no PromptMessage")
}

// TestEscalation_RetryHistoryShapeByProvider drives the loop with the
// built-in quirks registry and asserts the history each forced retry
// sends: Anthropic's and Gemini's native retries end on a synthetic user
// message, other native providers end on the no-tool assistant answer, and auto-only
// Anthropic models keep the single prompt-strategy message.
func TestEscalation_RetryHistoryShapeByProvider(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		model        string
		wantChoice   types.ToolChoiceMode
		wantLastRole string
	}{
		{"anthropic native ends on a user turn", "anthropic", "claude-sonnet-4-6", types.ToolChoiceRequired, "user"},
		{"anthropic native keeps the user turn where a prefill is accepted", "anthropic", "claude-haiku-4-5", types.ToolChoiceRequired, "user"},
		{"openai-compatible native unchanged", "openai-compatible", "gpt-4o", types.ToolChoiceRequired, "assistant"},
		{"gemini native ends on a user turn", "gemini", "gemini-3.8-flash", types.ToolChoiceRequired, "user"},
		{"anthropic auto-only takes the prompt strategy", "anthropic", "claude-sonnet-5-5", types.ToolChoiceAuto, "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			prov := &sequencedProvider{scripts: [][]types.StreamEvent{
				noToolAnswer(),
				toolCallTurn(),
				{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
			}}
			loop, _ := buildEscalationLoop(prov, newDefaultEscalationPolicy(1, quirks.DefaultRegistry()))
			loop.Router = router.NewStaticRouter(tc.provider, tc.model)

			_, err := loop.Run(context.Background(), escalationConfig("execution"))
			must.NoError(err)

			got := prov.choiceAt(1)
			must.Equal(tc.wantChoice, got, "retry call tool choice")

			history := prov.historyAt(1)
			must.GreaterOrEqual(len(history), 2, "retry history should hold the prompt and the no-tool answer")
			last := history[len(history)-1]
			must.Equal(tc.wantLastRole, last.Role, "retry history last role")
			if last.Role == "user" {
				must.True(last.Synthetic, "trailing user message should be marked synthetic")
				prev := history[len(history)-2]
				must.Equal("assistant", prev.Role, "message before the synthetic user turn should be the no-tool answer")
			}
		})
	}
}
