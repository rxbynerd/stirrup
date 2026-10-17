package core

import (
	"context"
	"reflect"
	"testing"

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
