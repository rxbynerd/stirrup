package guard

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidPhase_AcceptsOnlyKnownPhases(t *testing.T) {
	tests := []struct {
		name string
		p    Phase
		want bool
	}{
		{"pre_turn", PhasePreTurn, true},
		{"pre_tool", PhasePreTool, true},
		{"post_turn", PhasePostTurn, true},
		{"empty", Phase(""), false},
		{"unknown", Phase("post_tool"), false},
		{"casing matters", Phase("Pre_Turn"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ValidPhase(tc.p), "ValidPhase(%q)", tc.p)
		})
	}
}

func TestIsValidVerdict_AcceptsOnlyKnownVerdicts(t *testing.T) {
	tests := []struct {
		name string
		v    Verdict
		want bool
	}{
		{"allow", VerdictAllow, true},
		{"spotlight", VerdictAllowSpot, true},
		{"deny", VerdictDeny, true},
		{"empty", Verdict(""), false},
		{"unknown", Verdict("warn"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsValidVerdict(tc.v), "IsValidVerdict(%q)", tc.v)
		})
	}
}
