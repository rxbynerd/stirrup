package prompt

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTierFor_ClassifiesModelIDs(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{"fable 5 is frontier", "claude-fable-5", TierFrontier},
		{"fable 5 dated snapshot is frontier", "claude-fable-5-20260115", TierFrontier},
		{"mythos 5 is frontier", "claude-mythos-5", TierFrontier},
		{"sonnet 5 is frontier", "claude-sonnet-5", TierFrontier},
		{"opus 4.8 is frontier", "claude-opus-4-8", TierFrontier},
		{"opus 5 is frontier", "claude-opus-5", TierFrontier},
		{"opus 5.5 is frontier", "claude-opus-5-5", TierFrontier},
		{"sonnet 5.5 is frontier", "claude-sonnet-5-5", TierFrontier},
		{"fable 5.1 is frontier", "claude-fable-5-1", TierFrontier},
		{"gpt 5.5 is frontier", "gpt-5.5", TierFrontier},
		{"gpt 5.6 turbo is frontier", "gpt-5.6-turbo", TierFrontier},
		{"gpt 6 astra is frontier", "gpt-6-astra", TierFrontier},
		{"gpt 6.1 sol is frontier", "gpt-6.1-sol", TierFrontier},
		{"gateway-prefixed gpt 6 luna is frontier", "openai/gpt-6-luna", TierFrontier},
		{"gateway-prefixed fable 5 is frontier", "openrouter/claude-fable-5", TierFrontier},
		{"gemma 4 27b is open weight", "gemma-4-27b", TierOpenWeight},
		{"gemma 3 is open weight", "gemma3", TierOpenWeight},
		{"glm 5.2 is open weight", "glm-5.2", TierOpenWeight},
		{"deepseek v4 is open weight", "deepseek-v4", TierOpenWeight},
		{"qwen 3 coder is open weight", "qwen3-coder", TierOpenWeight},
		{"gateway-prefixed gemma 4 is open weight", "org/gemma-4", TierOpenWeight},
		{"gateway-prefixed deepseek v4 is open weight", "deepseek/deepseek-v4", TierOpenWeight},
		{"deepseek flash is open weight", "deepseek-flash", TierOpenWeight},
		{"glm 5.3 is open weight", "glm-5.3", TierOpenWeight},
		{"sonnet 4.6 is default", "claude-sonnet-4-6", TierDefault},
		{"haiku 4.5 dated snapshot is default", "claude-haiku-4-5-20251001", TierDefault},
		{"haiku 5.5 is default", "claude-haiku-5-5", TierDefault},
		{"gpt 4o is default", "gpt-4o", TierDefault},
		{"unknown model is default", "some-brand-new-model", TierDefault},
		{"empty model is default", "", TierDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TierFor(tt.model)
			require.Equal(t, tt.want, got, "TierFor(%q)", tt.model)
		})
	}
}

func TestTemplateData_ModelIs_MatchesGlobs(t *testing.T) {
	d := TemplateData{Model: "claude-fable-5-20260115"}
	prefixed := TemplateData{Model: "openrouter/claude-fable-5"}
	empty := TemplateData{Model: ""}

	t.Run("matches a glob", func(t *testing.T) {
		require.True(t, d.ModelIs("claude-fable-5*"))
	})
	t.Run("matches when a later glob matches", func(t *testing.T) {
		require.True(t, d.ModelIs("gpt-5.6*", "claude-fable-5*"))
	})
	t.Run("rejects a non-matching glob", func(t *testing.T) {
		require.False(t, d.ModelIs("gpt-5.6*"))
	})
	// path.Match does not cross "/": a bare glob must not match a
	// gateway-prefixed ID.
	t.Run("bare glob does not match a gateway-prefixed id", func(t *testing.T) {
		require.False(t, prefixed.ModelIs("claude-fable-5*"))
	})
	t.Run("*/ glob matches a gateway-prefixed id", func(t *testing.T) {
		require.True(t, prefixed.ModelIs("*/claude-fable-5*"))
	})
	t.Run("malformed glob does not match", func(t *testing.T) {
		require.False(t, d.ModelIs("[unclosed"))
	})
	t.Run("empty model matches no glob", func(t *testing.T) {
		require.False(t, empty.ModelIs("*"))
	})
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
	// assert, not require: every malformed glob and every overlapping
	// sample is reported in a single run.
	for _, tables := range [][]string{frontierModelGlobs, openWeightModelGlobs} {
		for _, g := range tables {
			_, err := path.Match(g, "probe")
			require.NoError(t, err, "glob %q does not compile", g)
		}
	}
	for _, m := range samples {
		inFrontier := modelMatchesAny(m, frontierModelGlobs)
		inOpenWeight := modelMatchesAny(m, openWeightModelGlobs)
		assert.False(t, inFrontier && inOpenWeight, "model %q matches both tier tables", m)
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
		// assert, not require: every glob missing a sibling is reported.
		for g := range bare {
			assert.True(t, prefixed[g], "glob %q has no */ variant", g)
		}
	}
}
