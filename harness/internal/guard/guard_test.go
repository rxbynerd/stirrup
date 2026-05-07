package guard

import "testing"

func TestValidPhase(t *testing.T) {
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
			if got := ValidPhase(tc.p); got != tc.want {
				t.Fatalf("ValidPhase(%q) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

func TestIsValidVerdict(t *testing.T) {
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
			if got := IsValidVerdict(tc.v); got != tc.want {
				t.Fatalf("IsValidVerdict(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}
