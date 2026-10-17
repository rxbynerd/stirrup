package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// TestLoop_ToolResultEventReportsNameOutcomeAndDuration pins that each
// tool_result event names the tool the matching tool_call named and
// reports whether the call failed and how long it ran.
func TestLoop_ToolResultEventReportsNameOutcomeAndDuration(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "failing_tool",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			time.Sleep(15 * time.Millisecond)
			return "", errors.New("boom")
		},
	})
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_ok", Name: "test_tool", Input: map[string]any{}},
			{Type: "tool_call", ID: "tc_bad", Name: "failing_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	tp := &recordingTransport{}
	loop.Transport = tp

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	results := eventsOfType(tp, "tool_result")
	if len(results) != 2 {
		t.Fatalf("got %d tool_result events, want 2", len(results))
	}
	ok, bad := results[0], results[1]
	if ok.ToolUseID != "tc_ok" || ok.Name != "test_tool" || ok.IsError {
		t.Errorf("first tool_result = id %q name %q isError %v, want tc_ok test_tool false", ok.ToolUseID, ok.Name, ok.IsError)
	}
	if bad.ToolUseID != "tc_bad" || bad.Name != "failing_tool" || !bad.IsError {
		t.Errorf("second tool_result = id %q name %q isError %v, want tc_bad failing_tool true", bad.ToolUseID, bad.Name, bad.IsError)
	}
	if bad.DurationMs < 15 {
		t.Errorf("failing_tool duration = %dms, want at least the 15ms it slept", bad.DurationMs)
	}
}

func userMessages(events []types.HarnessEvent) []types.HarnessEvent {
	var out []types.HarnessEvent
	for _, ev := range events {
		if ev.Type == "user_message" {
			out = append(out, ev)
		}
	}
	return out
}

func userMessageSources(events []types.HarnessEvent) []string {
	var out []string
	for _, ev := range userMessages(events) {
		out = append(out, ev.Source)
	}
	return out
}

func TestLoop_UserMessageReportsInitialPromptAfterRunStarted(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: scriptEndTurn})
	tp := &recordingTransport{}
	loop.Transport = tp
	config := buildTestConfig()

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(tp.events) < 2 || tp.events[0].Type != "run_started" || tp.events[1].Type != "user_message" {
		t.Fatalf("first events = %v, want run_started then user_message", eventTypeList(tp.events))
	}
	msg := tp.events[1]
	if msg.Source != types.UserMessageSourceInitialPrompt || msg.Text != config.Prompt || msg.RequestID != "" {
		t.Errorf("user_message = source %q text %q requestId %q, want the initial prompt", msg.Source, msg.Text, msg.RequestID)
	}
	if n := len(userMessages(tp.events)); n != 1 {
		t.Errorf("got %d user_message events, want 1", n)
	}
}

// TestLoop_UserMessagePerInjectedUserResponse pins one user_message per
// injected user_response, echoing each request ID, even though the
// inputs join the history as one merged message.
func TestLoop_UserMessagePerInjectedUserResponse(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn, scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	prov.onCall = func(i int) {
		if i == 0 {
			tr.FireControl(userResponse("First.", "r1"))
			tr.FireControl(userResponse("Second.", "r2"))
		}
	}

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := userMessages(tr.Events())
	if len(msgs) != 3 {
		t.Fatalf("user_message sources = %v, want initial_prompt and two user_response", userMessageSources(tr.Events()))
	}
	for i, want := range []struct{ text, requestID string }{{"First.", "r1"}, {"Second.", "r2"}} {
		got := msgs[i+1]
		if got.Source != types.UserMessageSourceUserResponse || got.Text != want.text || got.RequestID != want.requestID {
			t.Errorf("user_message %d = source %q text %q requestId %q, want user_response %q %q",
				i+1, got.Source, got.Text, got.RequestID, want.text, want.requestID)
		}
	}
}

func TestRunFollowUpLoop_UserMessageReportsFollowUpPrompt(t *testing.T) {
	loop, tr := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
	config := buildTestConfig()
	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { ran <- struct{}{} },
		})
	}()
	tr.FireControl(userResponse("And another thing.", "fu-1"))
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("follow-up run did not complete")
	}
	cancel()
	<-done

	msgs := userMessages(tr.Events())
	if len(msgs) != 2 {
		t.Fatalf("user_message sources = %v, want initial_prompt then followup_prompt", userMessageSources(tr.Events()))
	}
	got := msgs[1]
	if got.Source != types.UserMessageSourceFollowUpPrompt || got.Text != "And another thing." || got.RequestID != "fu-1" {
		t.Errorf("follow-up user_message = source %q text %q requestId %q", got.Source, got.Text, got.RequestID)
	}
	if loop.followUpInput != nil {
		t.Error("followUpInput left set after the follow-up run; a later run would be misreported")
	}
}

func TestLoop_UserMessageReportsVerifierFeedback(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: scriptEndTurn})
	loop.Verifier = &feedbackVerifier{feedback: "tests fail in parser_test.go", failures: 1}
	tp := &recordingTransport{}
	loop.Transport = tp

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := userMessages(tp.events)
	if len(msgs) != 2 || msgs[1].Source != types.UserMessageSourceVerifierFeedback || msgs[1].Text != "tests fail in parser_test.go" {
		t.Errorf("user_message events = %+v, want the initial prompt then the verifier feedback", msgs)
	}
}

// TestLoop_UserMessageReportsMergedNudgesSeparately pins that a stall
// nudge and a wind-down instruction merged into one history message are
// reported as two user_message events, in append order, each carrying
// only its own text.
func TestLoop_UserMessageReportsMergedNudgesSeparately(t *testing.T) {
	h, _ := newStallHarness(t,
		testToolTurn("tc_0"), testToolTurn("tc_1"), testToolTurn("tc_2"), distinctToolTurn(3), scriptEndTurn)

	if _, err := h.loop.Run(context.Background(), windDownConfig(5)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := userMessages(h.transport.Events())
	want := []types.HarnessEvent{
		{Source: types.UserMessageSourceInitialPrompt},
		{Source: types.UserMessageSourceStallNudge, Text: fmt.Sprintf(stallNudgeText, stallRepeats)},
		{Source: types.UserMessageSourceWindDown, Text: windDownText},
	}
	if len(msgs) != len(want) {
		t.Fatalf("user_message sources = %v, want %d events", userMessageSources(h.transport.Events()), len(want))
	}
	for i := 1; i < len(want); i++ {
		if msgs[i].Source != want[i].Source || msgs[i].Text != want[i].Text {
			t.Errorf("user_message %d = source %q text %q, want %q %q", i, msgs[i].Source, msgs[i].Text, want[i].Source, want[i].Text)
		}
	}
}

func TestLoop_UserMessageReportsOutputLimitRecovery(t *testing.T) {
	h := newOutputLimitHarness(
		[]types.StreamEvent{{Type: "text_delta", Text: "partial"}, stopWith("max_tokens")},
		scriptEndTurn,
	)

	if _, err := h.loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := userMessages(h.transport.Events())
	if len(msgs) != 2 || msgs[1].Source != types.UserMessageSourceOutputLimitRecovery || msgs[1].Text != outputLimitContinueText {
		t.Errorf("user_message events = %+v, want the initial prompt then the continue instruction", msgs)
	}
}

func eventTypeList(events []types.HarnessEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}
