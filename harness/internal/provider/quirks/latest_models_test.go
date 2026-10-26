package quirks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGPT6_ChatCompletionsRules pins the gpt-6 family on the
// openai-compatible surface. First-party ids get the full treatment,
// including the tools-require-Responses guard; gateway-prefixed ids only
// get the reasoning-class omissions, because a gateway may serve tools
// through its own Responses translation.
func TestGPT6_ChatCompletionsRules(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			o := DefaultRegistry().Resolve("openai-compatible", model).BehaviourFlags.OpenAI
			must.True(o.OmitSamplingParams, "reasoning-class flags not applied: %+v", o)
			must.Equal(TokenFieldMaxCompletionTokens, o.TokenField)
			must.True(o.StrictMode)
			must.True(o.ToolsRequireResponses)
			must.Equal(levels, o.ReasoningEffortLevels)
		})
	}
	for _, model := range []string{"openai/gpt-6-luna", "openai/gpt-6.1-sol"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			o := DefaultRegistry().Resolve("openai-compatible", model).BehaviourFlags.OpenAI
			must.True(o.OmitSamplingParams)
			must.False(o.ToolsRequireResponses, "first-party-only flags leaked onto gateway id: %+v", o)
			must.Empty(o.ReasoningEffortLevels, "first-party-only flags leaked onto gateway id: %+v", o)
		})
	}
}

// TestGPT6_ResponsesRules pins the gpt-6 family on the Responses surface
// and that earlier gpt-5 models keep forwarding temperature there.
func TestGPT6_ResponsesRules(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			r := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
			must.True(r.OmitSamplingParams)
			must.Equal(levels, r.ReasoningEffortLevels)
		})
	}
	for _, model := range []string{"gpt-5", "gpt-5-mini", "o3"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			r := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
			must.False(r.OmitSamplingParams, "picked up GPT-5.4+ Responses flags: %+v", r)
			must.Empty(r.ReasoningEffortLevels, "picked up GPT-5.4+ Responses flags: %+v", r)
		})
	}
}

// TestGPT5_ResponsesEffortRules pins the documented (not probed) GPT-5.4,
// 5.5 and 5.6 Responses allow-lists and sampling suppression: none lists
// minimal, only 5.6 lists max, and gpt-5.4 keeps temperature because its
// default effort is none.
func TestGPT5_ResponsesEffortRules(t *testing.T) {
	upToXHigh := []string{"low", "medium", "high", "xhigh"}
	upToMax := []string{"low", "medium", "high", "xhigh", "max"}
	cases := []struct {
		name       string
		model      string
		levels     []string
		omitSample bool
	}{
		{"gpt-5.4 allows up to xhigh and keeps temperature", "gpt-5.4", upToXHigh, false},
		{"gpt-5.4-nano inherits the gpt-5.4 rules", "gpt-5.4-nano", upToXHigh, false}, // inferred: only gpt-5.4 is documented
		{"gpt-5.5 allows up to xhigh and omits sampling", "gpt-5.5", upToXHigh, true},
		{"dated gpt-5.5 snapshot matches gpt-5.5", "gpt-5.5-2026-04-23", upToXHigh, true},
		{"gpt-5.6 sol allows max and omits sampling", "gpt-5.6-sol", upToMax, true},
		{"gpt-5.6 terra allows max and omits sampling", "gpt-5.6-terra", upToMax, true},
		{"gpt-5.6 luna allows max and omits sampling", "gpt-5.6-luna", upToMax, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			r := DefaultRegistry().Resolve("openai-responses", tc.model).BehaviourFlags.OpenAIResponses
			must.Equal(tc.levels, r.ReasoningEffortLevels)
			must.Equal(tc.omitSample, r.OmitSamplingParams)
		})
	}
}

// TestResponses_StrictModeRules pins explicit strict tools on the Responses
// surface for the gpt-5, gpt-6 and o-series families only; the older chat
// models keep the key omitted.
func TestResponses_StrictModeRules(t *testing.T) {
	cases := map[string]bool{
		"gpt-5":           true,
		"gpt-5-nano":      true,
		"gpt-5.4":         true,
		"gpt-5.6-luna":    true,
		"gpt-6-astra":     true,
		"gpt-6.1-sol":     true,
		"o1":              true,
		"o3-mini":         true,
		"o4-mini":         true,
		"gpt-4.1":         false,
		"gpt-4o":          false,
		"gpt-4o-mini":     false,
		"openai/gpt-5.6":  false,
		"omni-moderation": false,
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-responses", model)
			must.Equal(want, q.BehaviourFlags.OpenAI.StrictMode)
		})
	}
}

// TestResponses_IncludeEncryptedReasoningRules pins the encrypted-reasoning
// include to the reasoning families on the Responses surface. gpt-5-chat is
// a non-reasoning model inside the gpt-5* glob, so its carve-out clears the
// flag while keeping strict tools.
func TestResponses_IncludeEncryptedReasoningRules(t *testing.T) {
	cases := map[string]bool{
		"gpt-5":             true,
		"gpt-5-mini":        true,
		"gpt-5.5":           true,
		"gpt-5.6-sol":       true,
		"gpt-6-luna":        true,
		"o3":                true,
		"o4-mini":           true,
		"gpt-5-chat-latest": false,
		"gpt-4.1":           false,
		"gpt-4o":            false,
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-responses", model)
			must.Equal(want, q.BehaviourFlags.OpenAIResponses.IncludeEncryptedReasoning)
		})
	}
	t.Run("gpt-5-chat-latest keeps strict tools", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-responses", "gpt-5-chat-latest")
		must.True(q.BehaviourFlags.OpenAI.StrictMode)
	})
}

// TestResponses_ReplayOutputItemsRules pins verbatim output replay to the
// same reasoning families as the encrypted-reasoning include, so a model
// never replays reasoning items it was not asked to encrypt, and
// non-reasoning models keep the reconstructed input shape.
func TestResponses_ReplayOutputItemsRules(t *testing.T) {
	cases := map[string]bool{
		"gpt-5":             true,
		"gpt-5.4-mini":      true, // inferred: only gpt-5.4 is documented
		"gpt-5.6-sol":       true,
		"gpt-6.1-sol":       true,
		"o1":                true,
		"o3":                true,
		"gpt-5-chat-latest": false,
		"gpt-4.1":           false,
		"gpt-4.1-mini":      false,
		"gpt-4o":            false,
		"gpt-4o-mini":       false,
		"custom-deployment": false,
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			flags := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
			must.Equal(want, flags.ReplayOutputItems)
			must.Equal(flags.IncludeEncryptedReasoning, flags.ReplayOutputItems,
				"ReplayOutputItems and IncludeEncryptedReasoning must be set together")
		})
	}
}

// TestDeepSeekFlash_ReplayAndThinkingRules pins the V4.1 Flash id, which the
// deepseek-v4* glob does not match: without its own rule, reasoning_content
// is not replayed and every tool loop fails on its second turn.
func TestDeepSeekFlash_ReplayAndThinkingRules(t *testing.T) {
	levels := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", model)
			must.Equal([]string{"reasoning_content"}, q.ReplayFields)
			o := q.BehaviourFlags.OpenAI
			must.True(o.OmitSamplingParams)
			must.Equal(TokenFieldMaxTokens, o.TokenField)
			must.Equal(levels, o.ReasoningEffortLevels)
		})
	}
	t.Run("gateway deepseek-flash replays nothing and carries the thinking efforts", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek/deepseek-flash")
		must.Empty(q.ReplayFields)
		must.Equal(levels, q.BehaviourFlags.OpenAI.ReasoningEffortLevels)
	})
}
