package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Representative model IDs, one per tier plus the boundary cases.
var renderMatrixModels = []string{
	"claude-fable-5-20260115", // frontier
	"gpt-5.6",                 // frontier
	"gemma-4-27b",             // open-weight
	"glm-5.2",                 // open-weight
	"org/gemma-4",             // open-weight, gateway-prefixed
	"some-brand-new-model",    // unknown → default tier
	"",                        // unset → default tier
}

// executionBasePrompt pins the execution mode's default-tier fall-through
// text. A failure means either the base text was edited (update the
// constant alongside a deliberate prompt change) or tier content leaked
// into the default tier.
const executionBasePrompt = `You are a coding agent with full read/write access to the workspace.
Read relevant files before making changes. Apply edits, run tests, and iterate until all tests pass and the task is complete.
If the task is ambiguous, make the minimal reasonable interpretation rather than asking.
You can read files, write files, search the codebase, and run shell commands.`

func renderMode(t *testing.T, mode, model string) string {
	t.Helper()
	got, err := RenderModePrompt(mode, PromptContext{Mode: mode, Model: model})
	require.NoError(t, err, "RenderModePrompt(%q, model %q)", mode, model)
	return got
}

// Every embedded mode prompt must parse and render cleanly for every
// tier: no leftover template syntax, and the default-tier text is always
// a prefix of the output because tier blocks are additive.
func TestRenderModePrompt_Matrix(t *testing.T) {
	for mode := range ModePrompts() {
		base := renderMode(t, mode, "")
		require.NotEmpty(t, base, "mode %q: empty base render", mode)
		for _, model := range renderMatrixModels {
			name := mode + "/" + model
			if model == "" {
				name = mode + "/(empty)"
			}
			t.Run(name, func(t *testing.T) {
				got := renderMode(t, mode, model)
				require.NotContains(t, got, "{{", "rendered prompt contains unrendered template syntax")
				require.NotContains(t, got, "}}", "rendered prompt contains unrendered template syntax")
				require.True(t, strings.HasPrefix(got, base),
					"rendered prompt for model %q does not start with the default-tier text", model)
			})
		}
	}
}

// A model outside both tier tables renders exactly the default-tier
// text, and for the execution mode that text is byte-identical to the
// pinned execution prompt.
func TestRenderModePrompt_UnknownModelFallsThrough(t *testing.T) {
	for mode := range ModePrompts() {
		base := renderMode(t, mode, "")
		for _, model := range []string{"some-brand-new-model", "claude-haiku-4-5-20251001"} {
			t.Run(mode+"/"+model, func(t *testing.T) {
				require.Equal(t, base, renderMode(t, mode, model))
			})
		}
	}

	got := renderMode(t, "execution", "some-brand-new-model")
	require.Equal(t, executionBasePrompt, got, "execution fall-through drifted from the pinned prompt")
}

// Each tier's block must actually render for its models and stay out of
// the other tier's output.
func TestRenderModePrompt_TierBlocksRender(t *testing.T) {
	for mode := range ModePrompts() {
		t.Run(mode, func(t *testing.T) {
			base := renderMode(t, mode, "")
			frontier := renderMode(t, mode, "claude-fable-5")
			openWeight := renderMode(t, mode, "gemma-4")

			require.NotEqual(t, base, frontier, "frontier block missing")
			require.NotEqual(t, base, openWeight, "open-weight block missing")
			require.NotEqual(t, frontier, openWeight, "frontier and open-weight renders are identical")
		})
	}

	// Spot-check one distinctive phrase per tier on the execution mode.
	frontierExecution := renderMode(t, "execution", "claude-fable-5")
	require.Contains(t, frontierExecution, "When you have enough information to act, act.", "execution frontier block")

	openWeightExecution := renderMode(t, "execution", "glm-5.2")
	require.Contains(t, openWeightExecution, "Never describe an edit or command in prose", "execution open-weight block")
}

func TestRenderModePrompt_UnknownMode_ErrorNamesMode(t *testing.T) {
	_, err := RenderModePrompt("no-such-mode", PromptContext{Mode: "no-such-mode"})
	require.ErrorContains(t, err, "no-such-mode")
}

// DefaultPromptBuilder and the composed ModeTemplateFragment must agree
// with RenderModePrompt so the "default" and "composed" builder types
// produce the same preamble.
func TestModeTemplateFragment_MatchesRenderModePrompt(t *testing.T) {
	pc := PromptContext{Mode: "execution", Model: "claude-fable-5"}
	want, err := RenderModePrompt("execution", pc)
	require.NoError(t, err)

	got, err := ModeTemplateFragment().Render(t.Context(), pc)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
