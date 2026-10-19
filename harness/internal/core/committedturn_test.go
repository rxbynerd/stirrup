package core

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/types"
)

// TestLoop_TerminalTurnCommitsOnlyAValidAssistantMessage pins that a turn
// ending the run leaves history and the stream in agreement: tool calls
// of a turn that was not dispatched never reach history, a turn left with
// nothing to say adds no assistant message, and turn_committed is sent
// exactly when the assistant message is in the history a follow-up
// continues.
func TestLoop_TerminalTurnCommitsOnlyAValidAssistantMessage(t *testing.T) {
	cases := []struct {
		name        string
		events      []types.StreamEvent
		wantOutcome string
		wantText    []string
	}{
		{
			name:        "output limit cut a tool call with no text",
			events:      []types.StreamEvent{writeFileCall("tc_1", "a.go"), stopWith("max_tokens")},
			wantOutcome: "max_tokens",
		},
		{
			name:        "output limit cut a turn of thinking alone",
			events:      []types.StreamEvent{{Type: "thinking", Text: "long", ThoughtSignature: "sig"}, stopWith("max_tokens")},
			wantOutcome: "max_tokens",
		},
		{
			name:        "output limit cut a tool call after text",
			events:      []types.StreamEvent{{Type: "text_delta", Text: "Writing it now."}, writeFileCall("tc_1", "a.go"), stopWith("max_tokens")},
			wantOutcome: "max_tokens",
			wantText:    []string{"Writing it now."},
		},
		{
			name:        "end_turn carrying a tool call after text",
			events:      []types.StreamEvent{{Type: "text_delta", Text: "Done."}, writeFileCall("tc_1", "a.go"), stopWith("end_turn")},
			wantOutcome: "success",
			wantText:    []string{"Done."},
		},
		{
			name:        "end_turn carrying only a tool call",
			events:      []types.StreamEvent{writeFileCall("tc_1", "a.go"), stopWith("end_turn")},
			wantOutcome: "success",
		},
		{
			name:        "refusal after a tool call",
			events:      []types.StreamEvent{{Type: "text_delta", Text: "I can't."}, writeFileCall("tc_1", "a.go"), stopWith("refusal")},
			wantOutcome: "refusal",
			wantText:    []string{"I can't."},
		},
		{
			name:        "stop_sequence carrying only a tool call",
			events:      []types.StreamEvent{writeFileCall("tc_1", "a.go"), stopWith("stop_sequence")},
			wantOutcome: "stop_sequence",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOutputLimitHarness(tc.events)
			config := buildTestConfig()
			config.MaxTurns = 1
			config.FollowUpHistory = types.FollowUpHistoryContinue

			rt, err := h.loop.Run(context.Background(), config)
			require.NoError(t, err)
			require.Equal(t, tc.wantOutcome, rt.Outcome)
			require.Empty(t, h.writtenPaths(), "a tool ran for a turn that was not dispatched")

			kept := h.loop.carried.messages
			var assistants []types.Message
			for _, m := range kept {
				if m.Role == "assistant" {
					assistants = append(assistants, m)
				}
			}
			commits := eventsOfTypeIn(h.transport.Events(), "turn_committed")
			require.Len(t, commits, len(assistants), "turn_committed events and assistant messages in history disagree")
			if tc.wantText == nil {
				require.Empty(t, assistants, "history kept an assistant message")
				return
			}
			require.Len(t, assistants, 1)
			require.Equal(t, []string{"text"}, blockTypes(assistants[0]), "assistant message should hold only a text block")
			require.Equal(t, tc.wantText, textBlocks(assistants[0]))
			commit := commits[0]
			require.Equal(t, tc.events[len(tc.events)-1].StopReason, commit.StopReason, "turn_committed stop reason")
			require.Zero(t, commit.ToolResultCount, "turn_committed tool result count")
		})
	}
}

func TestLoop_RefusalTurnCommitsWithStopReason(t *testing.T) {
	h := newOutputLimitHarness([]types.StreamEvent{{Type: "text_delta", Text: "I can't help with that."}, stopWith("refusal")})

	rt, err := h.loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "refusal", rt.Outcome)
	commits := eventsOfTypeIn(h.transport.Events(), "turn_committed")
	require.Len(t, commits, 1)
	require.NotNil(t, commits[0].Turn)
	require.Equal(t, 0, *commits[0].Turn, "turn_committed turn number")
	require.Equal(t, "refusal", commits[0].StopReason)
}

// TestRunFollowUpLoop_ContinueAfterTerminalOutputLimitSendsAValidHistory
// pins that a follow-up continuing a run whose last turn was cut off at
// the output limit mid tool call sends neither an empty assistant
// message nor an unanswered tool call.
func TestRunFollowUpLoop_ContinueAfterTerminalOutputLimitSendsAValidHistory(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{writeFileCall("tc_1", "a.go"), stopWith("max_tokens")},
		scriptEndTurn,
	)
	h.loop.ensureControlRouting()
	config := buildTestConfig()
	config.MaxTurns = 1
	config.FollowUpHistory = types.FollowUpHistoryContinue

	traces := followUpSession(t, h.loop, h.transport, config, "carry on")
	require.Equal(t, "max_tokens", traces[0].Outcome, "first run outcome")
	require.Equal(t, "success", traces[1].Outcome, "follow-up run outcome")
	calls := h.provider.params()
	require.Len(t, calls, 2)
	followUp := calls[1].Messages
	var emptyAssistants []int
	for i, m := range followUp {
		if m.Role == "assistant" && len(m.Content) == 0 {
			emptyAssistants = append(emptyAssistants, i)
		}
	}
	require.Empty(t, emptyAssistants, "indexes of assistant messages with no content in the follow-up request")
	assertToolUsePaired(t, "follow-up request", followUp)
}

func eventsOfTypeIn(events []types.HarnessEvent, typ string) []types.HarnessEvent {
	var out []types.HarnessEvent
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func assertRolesAlternate(t *testing.T, label string, msgs []types.Message) {
	t.Helper()
	for i := 1; i < len(msgs); i++ {
		require.NotEqual(t, msgs[i-1].Role, msgs[i].Role, "%s: messages %d and %d are both %s: %v", label, i-1, i, msgs[i].Role, rolesOf(msgs))
	}
}

// TestRun_FeedbackAfterNoFinalAnswerMergesIntoTheTrailingUserMessage pins
// that verifier feedback and a schema correction following a turn that
// left no assistant message join the trailing user message, so the next
// request never holds two user messages in a row, and that merged
// feedback is screened exactly once.
func TestRun_FeedbackAfterNoFinalAnswerMergesIntoTheTrailingUserMessage(t *testing.T) {
	empty := []types.StreamEvent{stopWith("end_turn")}
	toolOnly := []types.StreamEvent{{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}}, stopWith("end_turn")}
	for name, first := range map[string][]types.StreamEvent{"empty end_turn": empty, "end_turn with only a tool call": toolOnly} {
		t.Run("verifier feedback after "+name, func(t *testing.T) {
			prov := &recordingScriptProvider{script: [][]types.StreamEvent{first, scriptEndTurn}}
			loop, _ := buildUserInputTestLoop(prov)
			loop.Verifier = &feedbackVerifier{feedback: "tests fail in parser_test.go", failures: 1}
			g := &feedbackVerdictGuard{verdict: guard.VerdictAllowSpot}
			loop.GuardRail = g

			rt, err := loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, err)
			require.Equal(t, "success", rt.Outcome)
			calls := prov.params()
			require.Len(t, calls, 2)
			retry := calls[1].Messages
			assertRolesAlternate(t, "retry request", retry)
			text := strings.Join(textBlocks(retry[len(retry)-1]), "\n")
			wrapped := base64.StdEncoding.EncodeToString([]byte("tests fail in parser_test.go"))
			require.True(t, strings.HasPrefix(text, buildTestConfig().Prompt), "retry's last message should start with the prompt; got %q", text)
			require.Equal(t, 1, strings.Count(text, wrapped), "feedback should appear once in the retry's last message")
			require.Equal(t, 1, strings.Count(text, guard.SpotlightOpenTag), "feedback should be spotlighted once")
			require.Len(t, g.feedbackChecks(), 1, "merged feedback should be screened once")
			blocks := retry[len(retry)-1].Content
			feedback := blocks[len(blocks)-1]
			require.Equal(t, "text", feedback.Type, "merged feedback block type")
			require.Equal(t, types.MessageOriginVerifierFeedback, feedback.Origin, "merged feedback block origin")
		})
	}

	t.Run("schema correction after an empty end_turn", func(t *testing.T) {
		loop, prov, _ := structuredTestLoop("", `{"title":"Fix login","labels":["bug"]}`)

		rt, err := loop.Run(context.Background(), structuredTestConfig())
		require.NoError(t, err)
		require.Equal(t, "success", rt.Outcome, "run error: %q", rt.Error)
		retry := prov.requests[1]
		require.Len(t, retry, 1, "correction should be merged into the prompt; roles %v", rolesOf(retry))
		text := strings.Join(textBlocks(retry[len(retry)-1]), "\n")
		require.Contains(t, text, "has no text", "correction request's last message")
	})
}

// turnPromptPolicy escalates with a prompt on one turn only.
type turnPromptPolicy struct{ turn int }

func (p turnPromptPolicy) Decide(in EscalationInput) EscalationDecision {
	if in.Turn != p.turn {
		return EscalationDecision{}
	}
	return EscalationDecision{Kind: EscalationPrompt, PromptMessage: "Call a tool now.", Reason: "test"}
}

// TestRun_HarnessTextMergedIntoScreenedFeedbackIsNotRescreened pins that
// an escalation prompt merged into verifier feedback that was already
// screened does not send that feedback through screening again.
func TestRun_HarnessTextMergedIntoScreenedFeedbackIsNotRescreened(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		scriptEndTurn,
		{stopWith("end_turn")},
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)
	loop.Verifier = &feedbackVerifier{feedback: "tests fail in parser_test.go", failures: 1}
	loop.Escalation = turnPromptPolicy{turn: 1}
	g := &feedbackVerdictGuard{verdict: guard.VerdictAllowSpot}
	loop.GuardRail = g

	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	calls := prov.params()
	require.Len(t, calls, 3)
	require.Len(t, g.feedbackChecks(), 1, "merged feedback should be screened once")
	last := calls[2].Messages[len(calls[2].Messages)-1]
	text := strings.Join(textBlocks(last), "\n")
	require.Equal(t, 1, strings.Count(text, guard.SpotlightOpenTag), "feedback should be spotlighted once in the third request")
	require.Contains(t, text, "Call a tool now.", "escalation prompt should be merged into the third request's last message")
	assertRolesAlternate(t, "third request", calls[2].Messages)
}

func TestRun_HarnessTextMergedIntoScreenedToolResultsIsNotRescreened(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{
		emitToolScript[0],
		{stopWith("end_turn")},
		scriptEndTurn,
	}}
	loop, _ := buildUserInputTestLoop(prov)
	registerEmitTool(t, loop, "SPOT-MARKER output")
	loop.Escalation = turnPromptPolicy{turn: 1}
	g := &markerGuard{markers: []string{"SPOT-MARKER", guard.SpotlightOpenTag}, verdict: guard.VerdictAllowSpot}
	loop.GuardRail = g

	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	calls := prov.params()
	require.Len(t, calls, 3)
	require.Equal(t, 1, g.matches(), "tool result should be screened once")
	results := toolResultContents(calls[2].Messages)
	require.Len(t, results, 1)
	require.Equal(t, 1, strings.Count(results[0], guard.SpotlightOpenTag), "third request's tool result should be spotlighted once; got %q", results[0])
}
