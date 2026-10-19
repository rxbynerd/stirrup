package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func trialData() TemplateData {
	return TemplateData{Model: "claude-fable-5", Mode: "execution"}
}

func TestNewTemplatePromptBuilder_RendersModelConditionals(t *testing.T) {
	b, err := NewTemplatePromptBuilder(
		`You are a tuned agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}{{if .ModelIs "gemma*"}} Follow the loop.{{end}}`,
		trialData(),
	)
	require.NoError(t, err)

	t.Run("frontier model renders its tier branch only", func(t *testing.T) {
		got, err := b.Build(t.Context(), PromptContext{Mode: "execution", Model: "claude-fable-5"})
		require.NoError(t, err)
		require.Contains(t, got, "You are a tuned agent. Act when ready.")
		require.NotContains(t, got, "Follow the loop.")
	})

	t.Run("gemma model renders the ModelIs branch", func(t *testing.T) {
		got, err := b.Build(t.Context(), PromptContext{Mode: "execution", Model: "gemma-4"})
		require.NoError(t, err)
		require.Contains(t, got, "Follow the loop.")
	})
}

// The operator template replaces the mode prompt but keeps the structural
// fragments, matching NewOverridePromptBuilder's contract.
func TestNewTemplatePromptBuilder_KeepsStructuralFragments(t *testing.T) {
	b, err := NewTemplatePromptBuilder("Tuned preamble.", trialData())
	require.NoError(t, err)
	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"go.mod"}
	ws.toplevel = "/workspace"
	got, err := b.Build(t.Context(), PromptContext{
		Mode:           "execution",
		Model:          "claude-fable-5",
		Workspace:      "/workspace",
		Executor:       ws,
		MaxTurns:       7,
		DynamicContext: map[string]string{"ticket": "fix the bug"},
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "Tuned preamble."), "preamble missing")
	execBase := ModePrompts()["execution"]
	require.NotContains(t, got, execBase, "mode prompt should be replaced, not appended")

	fragments := []struct {
		name string
		want string
	}{
		{name: "working directory", want: "Working directory:"},
		{name: "turn budget", want: "Turn budget: 7"},
		{name: "workspace files", want: "Workspace files:\n  go.mod"},
		{name: "git status", want: "Git status: clean"},
		{name: "untrusted context block", want: "<untrusted_context"},
	}
	for _, tc := range fragments {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, got, tc.want)
		})
	}
}

func TestNewTemplatePromptBuilder_ParseError(t *testing.T) {
	_, err := NewTemplatePromptBuilder("broken {{if .Tier}} unclosed", trialData())
	require.Error(t, err)
}

// Mustache-style placeholders from an uncompiled external prompt (e.g.
// Langfuse "{{var}}") must fail at construction, not at run start.
func TestNewTemplatePromptBuilder_RejectsMustachePlaceholder(t *testing.T) {
	_, err := NewTemplatePromptBuilder("You are an {{criticlevel}} movie critic", trialData())
	require.Error(t, err)
}

// Execution errors (a field TemplateData does not have) surface from the
// trial render at construction.
func TestNewTemplatePromptBuilder_TrialRenderRejectsUnknownField(t *testing.T) {
	_, err := NewTemplatePromptBuilder("hello {{.NoSuchField}}", trialData())
	require.ErrorContains(t, err, "claude-fable-5")
}
