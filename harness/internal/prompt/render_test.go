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

// Every embedded mode prompt must parse as a template and render for
// every tier. The base prompt text must always be the prefix of the
// rendered output: tier blocks are additive, appended after the base, so
// a model matching neither tier table gets exactly the base prompt
// (issue #492's fall-through requirement).
func TestRenderModePrompt_Matrix(t *testing.T) {
	for mode, base := range ModePrompts() {
		for _, model := range renderMatrixModels {
			name := mode + "/" + model
			if model == "" {
				name = mode + "/(empty)"
			}
			t.Run(name, func(t *testing.T) {
				got, err := RenderModePrompt(mode, PromptContext{Mode: mode, Model: model})
				if err != nil {
					t.Fatalf("RenderModePrompt(%q) error: %v", mode, err)
				}
				if !strings.HasPrefix(got, base) {
					t.Errorf("rendered prompt for model %q does not start with the base prompt text", model)
				}
			})
		}
	}
}

// A model outside both tier tables renders the base prompt byte-identical
// to the raw embedded text — the regression test for fall-through.
func TestRenderModePrompt_UnknownModelRendersBaseExactly(t *testing.T) {
	for mode, base := range ModePrompts() {
		for _, model := range []string{"some-brand-new-model", ""} {
			got, err := RenderModePrompt(mode, PromptContext{Mode: mode, Model: model})
			if err != nil {
				t.Fatalf("RenderModePrompt(%q) error: %v", mode, err)
			}
			if got != base {
				t.Errorf("mode %q, model %q: rendered prompt differs from base text\ngot:\n%s\nwant:\n%s", mode, model, got, base)
			}
		}
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
