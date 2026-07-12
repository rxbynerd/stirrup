package prompt

import (
	"strings"
	"testing"
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

// executionBasePrompt pins the execution mode's fall-through text to the
// exact prompt shipped before #492 introduced templating. If this test
// fails, either the base text was edited (update the constant alongside
// a deliberate prompt change) or tier content leaked into the default
// tier (a fall-through regression).
const executionBasePrompt = `You are a coding agent with full read/write access to the workspace.
Read relevant files before making changes. Apply edits, run tests, and iterate until all tests pass and the task is complete.
If the task is ambiguous, make the minimal reasonable interpretation rather than asking.
You can read files, write files, search the codebase, and run shell commands.`

func renderMode(t *testing.T, mode, model string) string {
	t.Helper()
	got, err := RenderModePrompt(mode, PromptContext{Mode: mode, Model: model})
	if err != nil {
		t.Fatalf("RenderModePrompt(%q, model %q) error: %v", mode, model, err)
	}
	return got
}

// Every embedded mode prompt must parse and render cleanly for every
// tier: no leftover template syntax, and the default-tier text is always
// a prefix of the output because tier blocks are additive (issue #492's
// fall-through requirement, held structurally).
func TestRenderModePrompt_Matrix(t *testing.T) {
	for mode := range ModePrompts() {
		base := renderMode(t, mode, "")
		if base == "" {
			t.Fatalf("mode %q: empty base render", mode)
		}
		for _, model := range renderMatrixModels {
			name := mode + "/" + model
			if model == "" {
				name = mode + "/(empty)"
			}
			t.Run(name, func(t *testing.T) {
				got := renderMode(t, mode, model)
				if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
					t.Errorf("rendered prompt contains unrendered template syntax:\n%s", got)
				}
				if !strings.HasPrefix(got, base) {
					t.Errorf("rendered prompt for model %q does not start with the default-tier text", model)
				}
			})
		}
	}
}

// A model outside both tier tables renders exactly the default-tier
// text, and for the execution mode that text is byte-identical to the
// pre-templating prompt.
func TestRenderModePrompt_UnknownModelFallsThrough(t *testing.T) {
	for mode := range ModePrompts() {
		base := renderMode(t, mode, "")
		for _, model := range []string{"some-brand-new-model", "claude-haiku-4-5-20251001"} {
			if got := renderMode(t, mode, model); got != base {
				t.Errorf("mode %q, model %q: rendered prompt differs from the default-tier text\ngot:\n%s\nwant:\n%s", mode, model, got, base)
			}
		}
	}
	if got := renderMode(t, "execution", "some-brand-new-model"); got != executionBasePrompt {
		t.Errorf("execution fall-through drifted from the pinned pre-#492 prompt\ngot:\n%s", got)
	}
}

// Each tier's block must actually render for its models and stay out of
// the other tier's output.
func TestRenderModePrompt_TierBlocksRender(t *testing.T) {
	for mode := range ModePrompts() {
		base := renderMode(t, mode, "")
		frontier := renderMode(t, mode, "claude-fable-5")
		openWeight := renderMode(t, mode, "gemma-4")

		if frontier == base {
			t.Errorf("mode %q: frontier render is identical to base — frontier block missing", mode)
		}
		if openWeight == base {
			t.Errorf("mode %q: open-weight render is identical to base — open-weight block missing", mode)
		}
		if frontier == openWeight {
			t.Errorf("mode %q: frontier and open-weight renders are identical", mode)
		}
	}

	// Spot-check one distinctive phrase per tier on the execution mode.
	if got := renderMode(t, "execution", "claude-fable-5"); !strings.Contains(got, "When you have enough information to act, act.") {
		t.Errorf("execution frontier block missing its guidance:\n%s", got)
	}
	if got := renderMode(t, "execution", "glm-5.2"); !strings.Contains(got, "Never describe an edit or command in prose") {
		t.Errorf("execution open-weight block missing its guidance:\n%s", got)
	}
}

func TestRenderModePrompt_UnknownMode(t *testing.T) {
	_, err := RenderModePrompt("no-such-mode", PromptContext{Mode: "no-such-mode"})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(err.Error(), "no-such-mode") {
		t.Errorf("expected error to name the mode, got: %v", err)
	}
}

// DefaultPromptBuilder and the composed ModeTemplateFragment must agree
// with RenderModePrompt so the "default" and "composed" builder types
// produce the same preamble.
func TestModeTemplateFragment_MatchesRenderModePrompt(t *testing.T) {
	pc := PromptContext{Mode: "execution", Model: "claude-fable-5"}
	want, err := RenderModePrompt("execution", pc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ModeTemplateFragment().Render(t.Context(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("fragment output differs from RenderModePrompt")
	}
}
