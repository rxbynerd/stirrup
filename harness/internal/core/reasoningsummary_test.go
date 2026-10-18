package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestStreamsThinking(t *testing.T) {
	for _, tc := range []struct {
		summary string
		want    bool
	}{
		{"", true},
		{types.ReasoningSummaryAuto, true},
		{types.ReasoningSummaryOff, false},
	} {
		if got := streamsThinking(&types.RunConfig{ReasoningSummary: tc.summary}); got != tc.want {
			t.Errorf("streamsThinking(reasoningSummary=%q) = %v, want %v", tc.summary, got, tc.want)
		}
	}
}

// TestLoop_ReasoningSummaryGatesThinkingDelta pins that the run's
// reasoningSummary decides whether the loop forwards reasoning: "off"
// suppresses both streamed fragments and whole-block fallbacks, and the
// default forwards both.
func TestLoop_ReasoningSummaryGatesThinkingDelta(t *testing.T) {
	turn := []types.StreamEvent{
		{Type: "thinking_delta", Text: "streamed"},
		{Type: "thinking", Text: "streamed", ThoughtSignature: "sig-A"},
		{Type: "thinking", Text: "whole", ThoughtSignature: "sig-B"},
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}
	for _, tc := range []struct {
		summary string
		want    string
	}{
		{"", "[streamed whole]"},
		{types.ReasoningSummaryOff, "[]"},
	} {
		loop := buildTestLoop(nil)
		loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{turn}}
		tp := &recordingTransport{}
		loop.Transport = tp
		cfg := buildTestConfig()
		cfg.ReasoningSummary = tc.summary

		if _, err := loop.Run(context.Background(), cfg); err != nil {
			t.Fatalf("reasoningSummary=%q: Run: %v", tc.summary, err)
		}
		if got := thinkingDeltaTexts(tp.events); fmt.Sprint(got) != tc.want {
			t.Errorf("reasoningSummary=%q: thinking_delta texts = %q, want %s", tc.summary, got, tc.want)
		}
	}
}
