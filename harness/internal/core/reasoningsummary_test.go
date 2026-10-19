package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestStreamsThinking_FollowsReasoningSummary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary string
		want    bool
	}{
		{"empty summary streams thinking", "", true},
		{"auto summary streams thinking", types.ReasoningSummaryAuto, true},
		{"off summary suppresses thinking", types.ReasoningSummaryOff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := streamsThinking(&types.RunConfig{ReasoningSummary: tc.summary})
			require.Equal(t, tc.want, got)
		})
	}
}

// TestLoop_ReasoningSummary_GatesThinkingDelta pins that the run's
// reasoningSummary decides whether the loop forwards reasoning: "off"
// suppresses both streamed fragments and whole-block fallbacks, and the
// default forwards both.
func TestLoop_ReasoningSummary_GatesThinkingDelta(t *testing.T) {
	turn := []types.StreamEvent{
		{Type: "thinking_delta", Text: "streamed"},
		{Type: "thinking", Text: "streamed", ThoughtSignature: "sig-A"},
		{Type: "thinking", Text: "whole", ThoughtSignature: "sig-B"},
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}
	for _, tc := range []struct {
		name    string
		summary string
		want    string
	}{
		{"default forwards streamed and whole-block reasoning", "", "[streamed whole]"},
		{"off suppresses all reasoning", types.ReasoningSummaryOff, "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop := buildTestLoop(nil)
			loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{turn}}
			tp := &recordingTransport{}
			loop.Transport = tp
			cfg := buildTestConfig()
			cfg.ReasoningSummary = tc.summary

			_, err := loop.Run(context.Background(), cfg)
			require.NoError(t, err)
			require.Equal(t, tc.want, fmt.Sprint(thinkingDeltaTexts(tp.events)))
		})
	}
}
