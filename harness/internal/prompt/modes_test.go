package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModePrompts_AllModesPresent(t *testing.T) {
	prompts := ModePrompts()

	expectedModes := []string{"execution", "planning", "review", "research", "toil"}
	for _, mode := range expectedModes {
		t.Run(mode, func(t *testing.T) {
			require.Contains(t, prompts, mode)
			require.NotEmpty(t, prompts[mode])
		})
	}

	require.Len(t, prompts, len(expectedModes))
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
		{"toil", []string{"monitoring agent", "trigger condition", "fetch URLs"}},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			text := prompts[tc.mode]
			for _, phrase := range tc.phrases {
				require.Contains(t, text, phrase)
			}
		})
	}
}

func TestModePrompts_NoLeadingTrailingWhitespace(t *testing.T) {
	for mode, text := range ModePrompts() {
		t.Run(mode, func(t *testing.T) {
			require.Equal(t, strings.TrimSpace(text), text)
		})
	}
}

func TestModePrompts_Idempotent(t *testing.T) {
	a := ModePrompts()
	b := ModePrompts()
	require.Len(t, b, len(a), "ModePrompts() returned different lengths on successive calls")
	for k, v := range a {
		t.Run(k, func(t *testing.T) {
			require.Equal(t, v, b[k])
		})
	}
}
