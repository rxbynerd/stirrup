package prompt

import (
	"path"
	"strings"
	"testing"
)

func TestTierFor(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-fable-5", TierFrontier},
		{"claude-fable-5-20260115", TierFrontier},
		{"claude-mythos-5", TierFrontier},
		{"claude-sonnet-5", TierFrontier},
		{"claude-opus-4-8", TierFrontier},
		{"gpt-5.5", TierFrontier},
		{"gpt-5.6-turbo", TierFrontier},
		{"openrouter/claude-fable-5", TierFrontier},
		{"gemma-4-27b", TierOpenWeight},
		{"gemma3", TierOpenWeight},
		{"glm-5.2", TierOpenWeight},
		{"deepseek-v4", TierOpenWeight},
		{"qwen3-coder", TierOpenWeight},
		{"org/gemma-4", TierOpenWeight},
		{"deepseek/deepseek-v4", TierOpenWeight},
		{"claude-sonnet-4-6", TierDefault},
		{"claude-haiku-4-5-20251001", TierDefault},
		{"gpt-4o", TierDefault},
		{"some-brand-new-model", TierDefault},
		{"", TierDefault},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := TierFor(tt.model); got != tt.want {
				t.Errorf("TierFor(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestTemplateData_ModelIs(t *testing.T) {
	d := TemplateData{Model: "claude-fable-5-20260115"}
	if !d.ModelIs("claude-fable-5*") {
		t.Error("expected match for claude-fable-5*")
	}
	if !d.ModelIs("gpt-5.6*", "claude-fable-5*") {
		t.Error("expected multi-glob OR to match on the second glob")
	}
	if d.ModelIs("gpt-5.6*") {
		t.Error("expected no match for gpt-5.6*")
	}
	// path.Match does not cross "/": a bare glob must not match a
	// gateway-prefixed ID.
	prefixed := TemplateData{Model: "openrouter/claude-fable-5"}
	if prefixed.ModelIs("claude-fable-5*") {
		t.Error("bare glob must not cross the / boundary")
	}
	if !prefixed.ModelIs("*/claude-fable-5*") {
		t.Error("expected */ variant to match the prefixed ID")
	}
	// A malformed glob is a non-match, not an error.
	if d.ModelIs("[unclosed") {
		t.Error("malformed glob must not match")
	}
	// Empty model never matches.
	empty := TemplateData{Model: ""}
	if empty.ModelIs("*") {
		t.Error("empty model must not match any glob")
	}
}

// The tier tables must stay disjoint: a model matching both would get
// whichever tier TierFor checks first, hiding a classification bug.
func TestTierGlobs_DisjointAndWellFormed(t *testing.T) {
	samples := []string{
		"claude-fable-5", "claude-mythos-5", "claude-sonnet-5",
		"claude-opus-4-8", "gpt-5.5", "gpt-5.6",
		"gemma-4", "glm-5.2", "deepseek-v4", "qwen3",
		"openrouter/claude-fable-5", "org/gemma-4",
	}
	for _, tables := range [][]string{frontierModelGlobs, openWeightModelGlobs} {
		for _, g := range tables {
			if _, err := path.Match(g, "probe"); err != nil {
				t.Errorf("glob %q does not compile: %v", g, err)
			}
		}
	}
	for _, m := range samples {
		inFrontier := modelMatchesAny(m, frontierModelGlobs)
		inOpenWeight := modelMatchesAny(m, openWeightModelGlobs)
		if inFrontier && inOpenWeight {
			t.Errorf("model %q matches both tier tables", m)
		}
	}
}

// Every tier table entry has a "*/"-prefixed sibling so gateway-routed
// model IDs classify the same as their bare forms.
func TestTierGlobs_PrefixedVariantsPresent(t *testing.T) {
	for _, table := range [][]string{frontierModelGlobs, openWeightModelGlobs} {
		bare := make(map[string]bool)
		prefixed := make(map[string]bool)
		for _, g := range table {
			if rest, ok := strings.CutPrefix(g, "*/"); ok {
				prefixed[rest] = true
			} else {
				bare[g] = true
			}
		}
		for g := range bare {
			if !prefixed[g] {
				t.Errorf("glob %q has no */ variant", g)
			}
		}
	}
}
