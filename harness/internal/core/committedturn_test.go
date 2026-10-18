package core

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

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
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rt.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", rt.Outcome, tc.wantOutcome)
			}
			if w := h.writtenPaths(); len(w) != 0 {
				t.Errorf("a tool ran for a turn that was not dispatched: %v", w)
			}

			kept := h.loop.carried.messages
			var assistants []types.Message
			for _, m := range kept {
				if m.Role == "assistant" {
					assistants = append(assistants, m)
				}
			}
			commits := eventsOfTypeIn(h.transport.Events(), "turn_committed")
			if len(commits) != len(assistants) {
				t.Fatalf("turn_committed events = %d, assistant messages in history = %d; the stream and history disagree", len(commits), len(assistants))
			}
			if tc.wantText == nil {
				if len(assistants) != 0 {
					t.Errorf("history kept an assistant message: %+v", assistants)
				}
				return
			}
			if len(assistants) != 1 {
				t.Fatalf("assistant messages = %d, want 1", len(assistants))
			}
			if got := blockTypes(assistants[0]); !reflect.DeepEqual(got, []string{"text"}) {
				t.Errorf("assistant blocks = %v, want the text alone", got)
			}
			if got := textBlocks(assistants[0]); !reflect.DeepEqual(got, tc.wantText) {
				t.Errorf("assistant text = %q, want %q", got, tc.wantText)
			}
			if c := commits[0]; c.StopReason != tc.events[len(tc.events)-1].StopReason || c.ToolResultCount != 0 {
				t.Errorf("turn_committed = stop %q results %d", c.StopReason, c.ToolResultCount)
			}
		})
	}
}

func TestLoop_RefusalTurnCommitsWithStopReason(t *testing.T) {
	h := newOutputLimitHarness([]types.StreamEvent{{Type: "text_delta", Text: "I can't help with that."}, stopWith("refusal")})

	rt, err := h.loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "refusal" {
		t.Fatalf("outcome = %q, want refusal", rt.Outcome)
	}
	commits := eventsOfTypeIn(h.transport.Events(), "turn_committed")
	if len(commits) != 1 || *commits[0].Turn != 0 || commits[0].StopReason != "refusal" {
		t.Errorf("turn_committed events = %+v, want turn 0 with stop reason refusal", commits)
	}
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
	if traces[0].Outcome != "max_tokens" || traces[1].Outcome != "success" {
		t.Fatalf("outcomes = %q, %q, want max_tokens then success", traces[0].Outcome, traces[1].Outcome)
	}
	calls := h.provider.params()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	followUp := calls[1].Messages
	for i, m := range followUp {
		if m.Role == "assistant" && len(m.Content) == 0 {
			t.Errorf("follow-up message %d is an assistant message with no content", i)
		}
	}
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
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("%s: messages %d and %d are both %s: %v", label, i-1, i, msgs[i].Role, rolesOf(msgs))
		}
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
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rt.Outcome != "success" {
				t.Fatalf("outcome = %q, want success", rt.Outcome)
			}
			calls := prov.params()
			if len(calls) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(calls))
			}
			retry := calls[1].Messages
			assertRolesAlternate(t, "retry request", retry)
			text := strings.Join(textBlocks(retry[len(retry)-1]), "\n")
			wrapped := base64.StdEncoding.EncodeToString([]byte("tests fail in parser_test.go"))
			if !strings.HasPrefix(text, buildTestConfig().Prompt) || strings.Count(text, wrapped) != 1 || strings.Count(text, guard.SpotlightOpenTag) != 1 {
				t.Errorf("retry's last message = %q, want the prompt then the feedback, spotlighted once", text)
			}
			if n := len(g.feedbackChecks()); n != 1 {
				t.Errorf("feedback screened %d times, want 1", n)
			}
		})
	}

	t.Run("schema correction after an empty end_turn", func(t *testing.T) {
		loop, prov, _ := structuredTestLoop("", `{"title":"Fix login","labels":["bug"]}`)

		rt, err := loop.Run(context.Background(), structuredTestConfig())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rt.Outcome != "success" {
			t.Fatalf("outcome = %q, want success (error %q)", rt.Outcome, rt.Error)
		}
		retry := prov.requests[1]
		if len(retry) != 1 {
			t.Fatalf("correction request roles = %v, want the correction merged into the prompt", rolesOf(retry))
		}
		if text := strings.Join(textBlocks(retry[len(retry)-1]), "\n"); !strings.Contains(text, "has no text") {
			t.Errorf("correction request's last message = %q, want the missing-answer correction", text)
		}
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

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := prov.params()
	if len(calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(calls))
	}
	if n := len(g.feedbackChecks()); n != 1 {
		t.Errorf("feedback screened %d times, want 1", n)
	}
	last := calls[2].Messages[len(calls[2].Messages)-1]
	text := strings.Join(textBlocks(last), "\n")
	if strings.Count(text, guard.SpotlightOpenTag) != 1 || !strings.Contains(text, "Call a tool now.") {
		t.Errorf("third request's last message = %q, want the feedback spotlighted once and the escalation prompt merged", text)
	}
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

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := prov.params()
	if len(calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(calls))
	}
	if g.matches() != 1 {
		t.Errorf("tool result screened %d times, want 1", g.matches())
	}
	results := toolResultContents(calls[2].Messages)
	if len(results) != 1 || strings.Count(results[0], guard.SpotlightOpenTag) != 1 {
		t.Errorf("third request's tool result = %q, want it spotlighted once", results)
	}
}
