package core

import (
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
