package prompt

import (
	"strings"
	"testing"
)

func TestModePrompts_AllModesPresent(t *testing.T) {
	prompts := ModePrompts()

	expectedModes := []string{"execution", "planning", "review", "research", "toil"}
	for _, mode := range expectedModes {
		text, ok := prompts[mode]
		if !ok {
			t.Errorf("ModePrompts() missing mode %q", mode)
			continue
		}
		if text == "" {
			t.Errorf("ModePrompts()[%q] is empty", mode)
		}
	}

	if len(prompts) != len(expectedModes) {
		t.Errorf("ModePrompts() has %d entries, want %d", len(prompts), len(expectedModes))
	}
}

func TestModePrompts_ExpectedPhrases(t *testing.T) {
	prompts := ModePrompts()

	cases := []struct {
		mode    string
		phrases []string
	}{
		{"execution", []string{"coding agent", "read/write access", "run shell commands"}},
		{"planning", []string{"planning agent", "read-only access", "step-by-step implementation plan"}},
		{"review", []string{"code review agent", "Review the provided changes", "severity"}},
		{"research", []string{"research agent", "read-only access", "actionable recommendations"}},
		{"toil", []string{"monitoring agent", "trigger condition", "run shell commands"}},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			text := prompts[tc.mode]
			for _, phrase := range tc.phrases {
				if !strings.Contains(text, phrase) {
					t.Errorf("ModePrompts()[%q] does not contain %q", tc.mode, phrase)
				}
			}
		})
	}
}

func TestModePrompts_NoLeadingTrailingWhitespace(t *testing.T) {
	for mode, text := range ModePrompts() {
		if text != strings.TrimSpace(text) {
			t.Errorf("ModePrompts()[%q] has leading/trailing whitespace", mode)
		}
	}
}

func TestModePrompts_Idempotent(t *testing.T) {
	a := ModePrompts()
	b := ModePrompts()
	if len(a) != len(b) {
		t.Fatal("ModePrompts() returned different lengths on successive calls")
	}
	for k, v := range a {
		if b[k] != v {
			t.Errorf("ModePrompts()[%q] differs between calls", k)
		}
	}
}
