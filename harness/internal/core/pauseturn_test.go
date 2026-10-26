package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// pendingSearchEvent is a native search call the server loop paused
// before answering.
func pendingSearchEvent(id, query string) types.StreamEvent {
	return types.StreamEvent{Type: "tool_search", ID: id, Name: "tool_search_tool_bm25", Input: map[string]any{"query": query}}
}

// pausedTurn is a response that searched once, then paused on a second
// search with no result.
func pausedTurn() []types.StreamEvent {
	return turnEvents("pause_turn",
		[]types.StreamEvent{{Type: "text_delta", Text: "Searching."}},
		nativeSearchEvents("srvtoolu_1", "github"),
		[]types.StreamEvent{pendingSearchEvent("srvtoolu_2", "jira")},
	)
}

// continuedTurn answers the paused search, then stops with stop.
func continuedTurn(stop string, groups ...[]types.StreamEvent) []types.StreamEvent {
	head := []types.StreamEvent{{Type: "tool_search_result", ID: "srvtoolu_2"}}
	return turnEvents(stop, append([][]types.StreamEvent{head}, groups...)...)
}

func pausedTurnBlockTypes() []string {
	return []string{"text", "tool_search", "tool_search_result", "tool_search"}
}

func turnCommittedStopReasons(tr *cancellableTransport) []string {
	var out []string
	for _, ev := range tr.Events() {
		if ev.Type == "turn_committed" {
			out = append(out, ev.StopReason)
		}
	}
	return out
}

func TestLoop_PauseTurn_ContinuesWithoutUserMessage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
		scriptEndTurn,
	}}
	loop, tr := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	calls := prov.params()
	must.Len(calls, 3)

	continuation := calls[1].Messages
	must.Len(continuation, 2, "the continuation adds no user message")
	paused := continuation[1]
	check.Equal("assistant", paused.Role)
	check.Equal(pausedTurnBlockTypes(), blockTypes(paused))

	history := calls[2].Messages
	must.Len(history, 3, "the continuation extends the paused assistant message")
	check.Equal("assistant", history[1].Role)
	check.Equal(append(pausedTurnBlockTypes(), "tool_search_result", "tool_use"), blockTypes(history[1]))
	check.Equal("srvtoolu_2", history[1].Content[4].ToolUseID)
	check.Equal("user", history[2].Role)
	check.Equal([]string{"tool_result"}, blockTypes(history[2]))
	check.Equal([]string{"pause_turn", "tool_use", "end_turn"}, turnCommittedStopReasons(tr))
}

// TestLoop_PauseTurn_ContinuationDoesNotMutateEarlierRequests relies on
// recordingScriptProvider recording each request's params without copying
// them: a commit that extended the paused message in place would show up
// in the recorded continuation request.
func TestLoop_PauseTurn_ContinuationDoesNotMutateEarlierRequests(t *testing.T) {
	must := require.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("end_turn", []types.StreamEvent{{Type: "text_delta", Text: "Found it."}}),
	}}
	loop, _ := buildUserInputTestLoop(prov)

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	calls := prov.params()
	must.Len(calls, 2)
	must.Equal(pausedTurnBlockTypes(), blockTypes(calls[1].Messages[1]), "the sent continuation request keeps the paused message as sent")
}

func TestLoop_PauseTurn_KeepsQueuedUserInputForTheNextBoundary(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var tr *cancellableTransport
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
		scriptEndTurn,
	}}
	prov.onCall = func(call int) {
		if call == 0 {
			tr.FireControl(userResponse("Also check Jira.", "r1"))
		}
	}
	loop, transport := buildUserInputTestLoop(prov)
	tr = transport

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	calls := prov.params()
	must.Len(calls, 3)
	check.Equal("assistant", calls[1].Messages[len(calls[1].Messages)-1].Role, "queued input waits while a paused turn continues")
	last := lastMessage(t, calls[2])
	check.Equal("user", last.Role)
	check.Equal([]string{"Also check Jira."}, textBlocks(last))
}

// pauseRecordingPolicy escalates the first no-tool turn of a run that
// has made no tool call, and records every stop reason it is consulted on.
type pauseRecordingPolicy struct {
	mu    sync.Mutex
	stops []string
}

func (p *pauseRecordingPolicy) Decide(in EscalationInput) EscalationDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops = append(p.stops, in.StopReason)
	if in.PriorToolCalls > 0 || in.EscalationsSoFar > 0 {
		return EscalationDecision{Kind: EscalationNone}
	}
	return EscalationDecision{Kind: EscalationPrompt, PromptMessage: "Call a tool.", Reason: "test policy"}
}

func TestLoop_PauseTurn_DoesNotTriggerEscalation(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)
	policy := &pauseRecordingPolicy{}
	loop.Escalation = policy

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	check.Equal([]string{"end_turn"}, policy.stops)
	calls := prov.params()
	must.Len(calls, 3)
	check.Equal("assistant", lastMessage(t, calls[1]).Role, "no escalation prompt follows the paused turn")
}

// alternatingRouter returns a different model on every Select call.
type alternatingRouter struct {
	mu    sync.Mutex
	calls int
}

func (r *alternatingRouter) Select(context.Context, router.RouterContext) router.ModelSelection {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return router.ModelSelection{Provider: "anthropic", Model: fmt.Sprintf("model-%d", r.calls)}
}

func TestLoop_PauseTurn_PinsRouterSelection(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)
	rtr := &alternatingRouter{}
	loop.Router = rtr

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	calls := prov.params()
	must.Len(calls, 3)
	check.Equal([]string{"model-1", "model-1", "model-2"}, []string{calls[0].Model, calls[1].Model, calls[2].Model})
	check.Equal(2, rtr.calls, "the continuation does not consult the router")
}

func TestLoop_PauseTurn_CapEndsRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{pausedTurn()}}
	loop, tr := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("pause_turn", runTrace.Outcome)
	check.Len(prov.params(), maxPauseContinuations+1)
	warnings := warningEvents(tr)
	must.Len(warnings, 1)
	check.Contains(warnings[0].Message, fmt.Sprintf("after %d consecutive continuations", maxPauseContinuations))
}

func TestLoop_PauseTurn_CapCountsOnlyConsecutivePauses(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var script [][]types.StreamEvent
	for range maxPauseContinuations {
		script = append(script, pausedTurn())
	}
	script = append(script, continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}))
	for range maxPauseContinuations {
		script = append(script, pausedTurn())
	}
	script = append(script, continuedTurn("end_turn", []types.StreamEvent{{Type: "text_delta", Text: "Done."}}))
	prov := &recordingScriptProvider{script: script}
	loop, tr := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	check.Len(prov.params(), len(script))
	check.Empty(warningEvents(tr))
}

func TestLoop_PauseTurn_LastTurnEndsRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{pausedTurn()}}
	loop, tr := buildUserInputTestLoop(prov)
	config := buildTestConfig()
	config.MaxTurns = 2

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("pause_turn", runTrace.Outcome)
	check.Equal(2, runTrace.Turns, "each continuation counts as a turn")
	warnings := warningEvents(tr)
	must.Len(warnings, 1)
	check.Contains(warnings[0].Message, "on the last turn the run allows")
}

func TestLoop_PauseTurn_OutputLimitRecoveryExtendsPausedMessage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("max_tokens", []types.StreamEvent{{Type: "text_delta", Text: "Partial"}}),
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	calls := prov.params()
	must.Len(calls, 3)
	history := calls[2].Messages
	must.Len(history, 3)
	check.Equal(append(pausedTurnBlockTypes(), "tool_search_result", "text"), blockTypes(history[1]))
	check.Equal("user", history[2].Role)
}

func TestLoop_PauseTurn_ContinuationResultNamesThePausedSearch(t *testing.T) {
	must := require.New(t)
	prov := &scriptedParamsProvider{calls: [][]types.StreamEvent{
		turnEvents("pause_turn", []types.StreamEvent{pendingSearchEvent("srvtoolu_1", "github")}),
		turnEvents("end_turn", []types.StreamEvent{{Type: "tool_search_result", ID: "srvtoolu_1", ToolReferences: []string{lazyMCPTool}}}),
	}}
	loop, tp, _, _ := buildLazyTestLoop(t, prov, types.LazyToolLoadingConfig{})
	loop.ToolSearch = nativeToolSearch("bm25")

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	loaded := toolsLoadedEvents(tp)
	must.Len(loaded, 1)
	must.Equal("tool_search_tool_bm25", loaded[0].Name)
	must.JSONEq(`{"query":"github"}`, string(loaded[0].Input))
	must.Equal([]string{lazyMCPTool}, loaded[0].Tools)
}

func TestLoop_ToolUseStopWithOnlyServerBlocksIsError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		turnEvents("tool_use", nativeSearchEvents("srvtoolu_1", "github")),
	}}
	loop, _ := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("error", runTrace.Outcome)
	check.Len(prov.params(), 1)
	check.Contains(runTrace.Error, "tool_use stop without any tool calls")
}

// withUsage returns a copy of events whose final message_complete reports
// the given token usage.
func withUsage(events []types.StreamEvent, input, output int) []types.StreamEvent {
	out := append([]types.StreamEvent(nil), events...)
	out[len(out)-1].InputTokens = input
	out[len(out)-1].OutputTokens = output
	return out
}

func TestLoop_PauseTurn_HoldsWindDownForTheNextBoundary(t *testing.T) {
	must := require.New(t)
	budget := 1000
	h, _ := newStallHarness(t,
		withUsage(pausedTurn(), 500, 400),
		withUsage(continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}), 1, 1),
		scriptEndTurn,
	)
	cfg := windDownConfig(20)
	cfg.MaxTokenBudget = &budget

	runTrace, err := h.loop.Run(context.Background(), cfg)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	calls := h.provider.params()
	assertWindDownCounts(t, calls, []int{0, 0, 1})
	must.Equal("assistant", lastMessage(t, calls[1]).Role, "the continuation request ends on the paused message")
	assertNudgeJoinsToolResults(t, calls[2].Messages, windDownText, "call-1")
	assertTriggers(t, h.emitter, []string{"", "", windDownTriggerTokens})
	must.Len(windDownWarnings(h), 1, "wind-down warnings")
}

func TestLoop_PauseTurn_SendsWindDownOnTheLastTurn(t *testing.T) {
	must := require.New(t)
	h, _ := newStallHarness(t, pausedTurn(), pausedTurn(), pausedTurn(), scriptEndTurn)

	runTrace, err := h.loop.Run(context.Background(), windDownConfig(4))
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	calls := h.provider.params()
	assertWindDownCounts(t, calls, []int{0, 0, 0, 1})
	must.Equal("assistant", lastMessage(t, calls[2]).Role, "a wind-down due on a continuation before the last turn waits")
	last := lastMessage(t, calls[3])
	must.Equal("user", last.Role, "the last turn sends the wind-down instead of continuing the pause")
	must.Equal([]string{windDownText}, textBlocks(last))
	assertTriggers(t, h.emitter, []string{"", "", "", windDownTriggerTurns})
}

func TestCommitAssistantContent_ContinuationReplacesReplayFields(t *testing.T) {
	task := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "task"}}}
	paused := types.Message{
		Role:         "assistant",
		Content:      []types.ContentBlock{{Type: "text", Text: "paused"}},
		ReplayFields: map[string]json.RawMessage{"k": json.RawMessage(`"paused"`)},
	}
	blocks := []types.ContentBlock{{Type: "text", Text: "continued"}}
	continued := map[string]json.RawMessage{"k": json.RawMessage(`"continued"`)}
	cases := []struct {
		name         string
		history      []types.Message
		replayFields map[string]json.RawMessage
		want         []types.Message
	}{
		{
			name:         "non-nil replay fields replace the paused message's",
			history:      []types.Message{task, paused},
			replayFields: continued,
			want: []types.Message{task, {
				Role:         "assistant",
				Content:      []types.ContentBlock{{Type: "text", Text: "paused"}, {Type: "text", Text: "continued"}},
				ReplayFields: continued,
			}},
		},
		{
			name:    "nil replay fields leave the paused message's",
			history: []types.Message{task, paused},
			want: []types.Message{task, {
				Role:         "assistant",
				Content:      []types.ContentBlock{{Type: "text", Text: "paused"}, {Type: "text", Text: "continued"}},
				ReplayFields: paused.ReplayFields,
			}},
		},
		{
			name:         "a history not ending on an assistant message gains a new one",
			history:      []types.Message{task},
			replayFields: continued,
			want:         []types.Message{task, {Role: "assistant", Content: blocks, ReplayFields: continued}},
		},
		{
			name:         "an empty history gains a new assistant message",
			replayFields: continued,
			want:         []types.Message{{Role: "assistant", Content: blocks, ReplayFields: continued}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			check.Equal(tc.want, commitAssistantContent(tc.history, blocks, tc.replayFields, true))
			check.Equal([]types.ContentBlock{{Type: "text", Text: "paused"}}, paused.Content, "the paused message is not mutated")
		})
	}
}

func TestLoop_PauseTurn_EmptyPausedResponseAbandonsTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		turnEvents("pause_turn", []types.StreamEvent{{Type: "text_delta", Text: ""}}),
	}}
	loop, tr := buildUserInputTestLoop(prov)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("pause_turn", runTrace.Outcome)
	check.Len(prov.params(), 1)
	check.Equal([]string{types.TurnAbandonedEmptyResponse}, abandonReasons(tr.Events()))
	check.Empty(turnCommittedStopReasons(tr))
	warnings := warningEvents(tr)
	must.Len(warnings, 1)
	check.Contains(warnings[0].Message, "with an empty response")
}

// providerAlternatingRouter selects a different provider key on every
// Select call.
type providerAlternatingRouter struct {
	mu    sync.Mutex
	calls int
	keys  []string
}

func (r *providerAlternatingRouter) Select(context.Context, router.RouterContext) router.ModelSelection {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.keys[r.calls%len(r.keys)]
	r.calls++
	return router.ModelSelection{Provider: key, Model: "claude-sonnet-4-6"}
}

func TestLoop_PauseTurn_ContinuationReusesPausedProvider(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	first := &recordingScriptProvider{script: [][]types.StreamEvent{
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
	}}
	second := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, _ := buildUserInputTestLoop(first)
	loop.Providers = map[string]provider.ProviderAdapter{"first": first, "second": second}
	loop.Router = &providerAlternatingRouter{keys: []string{"first", "second"}}
	config := buildTestConfig()
	config.Providers = map[string]types.ProviderConfig{
		"first":  {Type: "anthropic", APIKeyRef: "secret://TEST"},
		"second": {Type: "anthropic", APIKeyRef: "secret://TEST"},
	}

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	check.Len(first.params(), 2, "the paused provider serves the continuation")
	check.Len(second.params(), 1, "the router's next pick serves the turn after the continuation")
}

func TestLoop_PauseTurn_CompactionBetweenPauseAndContinuationKeepsPausedMessageLast(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		turnEvents("tool_use", []types.StreamEvent{toolCall("call-big", "big_tool", `{}`)}),
		pausedTurn(),
		continuedTurn("tool_use", []types.StreamEvent{toolCall("call-1", "test_tool", `{}`)}),
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "big_tool",
		Description: "Returns a large output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 40_000), nil
		},
	})
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 2000

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	calls := prov.params()
	must.Len(calls, 4)
	must.Len(calls[1].Messages, 3, "the paused turn's request is not compacted")

	continuation := calls[2].Messages
	must.Len(continuation, 2, "compaction drops the exchange before the paused message")
	check.Equal("user", continuation[0].Role)
	check.Equal("assistant", continuation[1].Role)
	check.Equal(pausedTurnBlockTypes(), blockTypes(continuation[1]))

	after := calls[3].Messages
	must.Len(after, 3)
	check.Equal(append(pausedTurnBlockTypes(), "tool_search_result", "tool_use"), blockTypes(after[1]))
	check.Equal([]string{"tool_result"}, blockTypes(after[2]))
}

func TestLoop_PauseTurn_StateClearedAfterMaxTokensRecovery(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var script [][]types.StreamEvent
	for range maxPauseContinuations {
		script = append(script, pausedTurn())
	}
	script = append(script, continuedTurn("max_tokens", []types.StreamEvent{{Type: "text_delta", Text: "Partial"}}))
	for range maxPauseContinuations {
		script = append(script, pausedTurn())
	}
	script = append(script, continuedTurn("end_turn", []types.StreamEvent{{Type: "text_delta", Text: "Done."}}))
	prov := &recordingScriptProvider{script: script}
	loop, tr := buildUserInputTestLoop(prov)
	rtr := &alternatingRouter{}
	loop.Router = rtr

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome, "the recovery restarts the consecutive continuation count")
	calls := prov.params()
	must.Len(calls, len(script))
	for _, w := range warningEvents(tr) {
		check.NotContains(w.Message, "paused", "no paused turn ends the run")
	}
	check.Equal(2, rtr.calls, "the turn after the recovery consults the router")
	recovery := maxPauseContinuations
	check.Equal("model-1", calls[recovery].Model)
	check.Equal("model-2", calls[recovery+1].Model)
	check.Equal("model-2", calls[len(calls)-1].Model)
}

func TestLoop_PauseTurn_ClientToolUseBlocksInPausedTurnAreNotDispatched(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		turnEvents("pause_turn",
			[]types.StreamEvent{{Type: "text_delta", Text: "Searching."}, toolCall("call-1", "test_tool", `{}`)},
			[]types.StreamEvent{pendingSearchEvent("srvtoolu_2", "jira")},
		),
		continuedTurn("end_turn", []types.StreamEvent{{Type: "text_delta", Text: "Done."}}),
	}}
	loop, _ := buildUserInputTestLoop(prov)
	dispatched := countingTool(loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
	check.Zero(dispatched.Load(), "a paused turn's tool_use block is not dispatched")
	calls := prov.params()
	must.Len(calls, 2)
	continuation := calls[1].Messages
	must.Len(continuation, 2, "no tool result follows the paused message")
	check.Equal([]string{"text", "tool_search"}, blockTypes(continuation[1]), "the tool_use block is not committed")
}

// cancelOnPauseTransport cancels the run when a paused turn commits,
// before the loop starts the turn that would continue it.
type cancelOnPauseTransport struct {
	*cancellableTransport
	cancel context.CancelFunc
}

func (t *cancelOnPauseTransport) Emit(event types.HarnessEvent) error {
	err := t.cancellableTransport.Emit(event)
	if event.Type == "turn_committed" && event.StopReason == "pause_turn" {
		t.cancel()
	}
	return err
}

func TestLoop_PauseTurn_RunEndsBetweenPauseAndContinuation(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		prov := &recordingScriptProvider{script: [][]types.StreamEvent{pausedTurn(), scriptEndTurn}}
		loop, tr := buildUserInputTestLoop(prov)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loop.Transport = &cancelOnPauseTransport{cancellableTransport: tr, cancel: cancel}

		runTrace, err := loop.Run(ctx, buildTestConfig())
		must.NoError(err)
		check.Equal("cancelled", runTrace.Outcome)
		check.Len(prov.params(), 1, "the continuation is not requested")
		check.Equal([]string{"pause_turn"}, turnCommittedStopReasons(tr))
		check.Empty(warningEvents(tr))
		assertTurnsClosed(t, tr.Events())
	})
	t.Run("budget exhausted", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		budget := 100
		prov := &recordingScriptProvider{script: [][]types.StreamEvent{withUsage(pausedTurn(), 60, 50), scriptEndTurn}}
		loop, tr := buildUserInputTestLoop(prov)
		config := buildTestConfig()
		config.MaxTokenBudget = &budget

		runTrace, err := loop.Run(context.Background(), config)
		must.NoError(err)
		check.Equal("budget_exceeded", runTrace.Outcome)
		check.Len(prov.params(), 1, "the continuation is not requested")
		check.Equal([]string{"pause_turn"}, turnCommittedStopReasons(tr))
		assertTurnsClosed(t, tr.Events())
	})
}
