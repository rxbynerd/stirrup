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
	must := require.New(t)
	b, err := NewTemplatePromptBuilder(
		`You are a tuned agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}{{if .ModelIs "gemma*"}} Follow the loop.{{end}}`,
		trialData(),
	)
	must.NoError(err)

	t.Run("frontier model renders its tier branch only", func(t *testing.T) {
		must := require.New(t)
		got, err := b.Build(t.Context(), PromptContext{Mode: "execution", Model: "claude-fable-5"})
		must.NoError(err)
		must.Contains(got, "You are a tuned agent. Act when ready.")
		must.NotContains(got, "Follow the loop.")
	})

	t.Run("gemma model renders the ModelIs branch", func(t *testing.T) {
		must := require.New(t)
		got, err := b.Build(t.Context(), PromptContext{Mode: "execution", Model: "gemma-4"})
		must.NoError(err)
		must.Contains(got, "Follow the loop.")
	})
}

// The operator template replaces the mode prompt but keeps the structural
// fragments, matching NewOverridePromptBuilder's contract.
func TestNewTemplatePromptBuilder_KeepsStructuralFragments(t *testing.T) {
	must := require.New(t)
	b, err := NewTemplatePromptBuilder("Tuned preamble.", trialData())
	must.NoError(err)
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
	must.NoError(err)
	must.True(strings.HasPrefix(got, "Tuned preamble."), "preamble missing")
	execBase := ModePrompts()["execution"]
	must.NotContains(got, execBase, "mode prompt should be replaced, not appended")

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
			must := require.New(t)
			must.Contains(got, tc.want)
		})
	}
}

func TestNewTemplatePromptBuilder_ParseError(t *testing.T) {
	must := require.New(t)
	_, err := NewTemplatePromptBuilder("broken {{if .Tier}} unclosed", trialData())
	must.Error(err)
}

// Mustache-style placeholders from an uncompiled external prompt (e.g.
// Langfuse "{{var}}") must fail at construction, not at run start.
func TestNewTemplatePromptBuilder_RejectsMustachePlaceholder(t *testing.T) {
	must := require.New(t)
	_, err := NewTemplatePromptBuilder("You are an {{criticlevel}} movie critic", trialData())
	must.Error(err)
}

// Execution errors (a field TemplateData does not have) surface from the
// trial render at construction.
func TestNewTemplatePromptBuilder_TrialRenderRejectsUnknownField(t *testing.T) {
	must := require.New(t)
	_, err := NewTemplatePromptBuilder("hello {{.NoSuchField}}", trialData())
	must.ErrorContains(err, "claude-fable-5")
}
