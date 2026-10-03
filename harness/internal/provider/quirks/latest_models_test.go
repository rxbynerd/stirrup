package quirks

import (
	"reflect"
	"testing"
)

// TestGPT6ChatCompletionsRules pins the gpt-6 family on the
// openai-compatible surface. First-party ids get the full treatment,
// including the tools-require-Responses guard; gateway-prefixed ids only
// get the reasoning-class omissions, because a gateway may serve tools
// through its own Responses translation.
func TestGPT6ChatCompletionsRules(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			o := DefaultRegistry().Resolve("openai-compatible", model).BehaviourFlags.OpenAI
			if !o.OmitSamplingParams || o.TokenField != TokenFieldMaxCompletionTokens {
				t.Errorf("reasoning-class flags not applied: %+v", o)
			}
			if !o.StrictMode {
				t.Error("StrictMode = false, want true")
			}
			if !o.ToolsRequireResponses {
				t.Error("ToolsRequireResponses = false, want true")
			}
			if !reflect.DeepEqual(o.ReasoningEffortLevels, levels) {
				t.Errorf("ReasoningEffortLevels = %v, want %v", o.ReasoningEffortLevels, levels)
			}
		})
	}
	for _, model := range []string{"openai/gpt-6-luna", "openai/gpt-6.1-sol"} {
		t.Run(model, func(t *testing.T) {
			o := DefaultRegistry().Resolve("openai-compatible", model).BehaviourFlags.OpenAI
			if !o.OmitSamplingParams {
				t.Error("OmitSamplingParams = false, want true")
			}
			if o.ToolsRequireResponses || len(o.ReasoningEffortLevels) != 0 {
				t.Errorf("first-party-only flags leaked onto gateway id: %+v", o)
			}
		})
	}
}

// TestGPT6ResponsesRules pins the gpt-6 family on the Responses surface
// and that earlier gpt-5 models keep forwarding temperature there.
func TestGPT6ResponsesRules(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			r := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
			if !r.OmitSamplingParams {
				t.Error("OmitSamplingParams = false, want true")
			}
			if !reflect.DeepEqual(r.ReasoningEffortLevels, levels) {
				t.Errorf("ReasoningEffortLevels = %v, want %v", r.ReasoningEffortLevels, levels)
			}
		})
	}
	for _, model := range []string{"gpt-5", "gpt-5-mini", "o3"} {
		r := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
		if r.OmitSamplingParams || len(r.ReasoningEffortLevels) != 0 {
			t.Errorf("%s picked up GPT-5.4+ Responses flags: %+v", model, r)
		}
	}
}

// TestGPT5ResponsesEffortRules pins the documented (not probed) GPT-5.4,
// 5.5 and 5.6 Responses allow-lists and sampling suppression: none lists
// minimal, only 5.6 lists max, and gpt-5.4 keeps temperature because its
// default effort is none.
func TestGPT5ResponsesEffortRules(t *testing.T) {
	upToXHigh := []string{"low", "medium", "high", "xhigh"}
	upToMax := []string{"low", "medium", "high", "xhigh", "max"}
	cases := []struct {
		model      string
		levels     []string
		omitSample bool
	}{
		{"gpt-5.4", upToXHigh, false},
		{"gpt-5.4-nano", upToXHigh, false}, // inferred: only gpt-5.4 is documented
		{"gpt-5.5", upToXHigh, true},
		{"gpt-5.5-2026-04-23", upToXHigh, true},
		{"gpt-5.6-sol", upToMax, true},
		{"gpt-5.6-terra", upToMax, true},
		{"gpt-5.6-luna", upToMax, true},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			r := DefaultRegistry().Resolve("openai-responses", tc.model).BehaviourFlags.OpenAIResponses
			if !reflect.DeepEqual(r.ReasoningEffortLevels, tc.levels) {
				t.Errorf("ReasoningEffortLevels = %v, want %v", r.ReasoningEffortLevels, tc.levels)
			}
			if r.OmitSamplingParams != tc.omitSample {
				t.Errorf("OmitSamplingParams = %v, want %v", r.OmitSamplingParams, tc.omitSample)
			}
		})
	}
}

// TestResponsesStrictModeRules pins explicit strict tools on the Responses
// surface for the gpt-5, gpt-6 and o-series families only; the older chat
// models keep the key omitted.
func TestResponsesStrictModeRules(t *testing.T) {
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
			q := DefaultRegistry().Resolve("openai-responses", model)
			if got := q.BehaviourFlags.OpenAI.StrictMode; got != want {
				t.Errorf("StrictMode = %v, want %v", got, want)
			}
		})
	}
}

// TestResponsesIncludeEncryptedReasoningRules pins the encrypted-reasoning
// include to the reasoning families on the Responses surface. gpt-5-chat is
// a non-reasoning model inside the gpt-5* glob, so its carve-out clears the
// flag while keeping strict tools.
func TestResponsesIncludeEncryptedReasoningRules(t *testing.T) {
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
			q := DefaultRegistry().Resolve("openai-responses", model)
			if got := q.BehaviourFlags.OpenAIResponses.IncludeEncryptedReasoning; got != want {
				t.Errorf("IncludeEncryptedReasoning = %v, want %v", got, want)
			}
		})
	}
	if !DefaultRegistry().Resolve("openai-responses", "gpt-5-chat-latest").BehaviourFlags.OpenAI.StrictMode {
		t.Error("gpt-5-chat carve-out must not clear StrictMode")
	}
}

// TestResponsesReplayOutputItemsRules pins verbatim output replay to the
// same reasoning families as the encrypted-reasoning include, so a model
// never replays reasoning items it was not asked to encrypt, and
// non-reasoning models keep the reconstructed input shape.
func TestResponsesReplayOutputItemsRules(t *testing.T) {
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
			flags := DefaultRegistry().Resolve("openai-responses", model).BehaviourFlags.OpenAIResponses
			if flags.ReplayOutputItems != want {
				t.Errorf("ReplayOutputItems = %v, want %v", flags.ReplayOutputItems, want)
			}
			if flags.ReplayOutputItems != flags.IncludeEncryptedReasoning {
				t.Errorf("ReplayOutputItems = %v but IncludeEncryptedReasoning = %v; the two must be set together",
					flags.ReplayOutputItems, flags.IncludeEncryptedReasoning)
			}
		})
	}
}

// TestDeepSeekFlashRules pins the V4.1 Flash id, which the deepseek-v4*
// glob does not match: without its own rule, reasoning_content is not
// replayed and every tool loop fails on its second turn.
func TestDeepSeekFlashRules(t *testing.T) {
	levels := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"} {
		t.Run(model, func(t *testing.T) {
			q := DefaultRegistry().Resolve("openai-compatible", model)
			if !reflect.DeepEqual(q.ReplayFields, []string{"reasoning_content"}) {
				t.Errorf("ReplayFields = %v, want [reasoning_content]", q.ReplayFields)
			}
			o := q.BehaviourFlags.OpenAI
			if !o.OmitSamplingParams || o.TokenField != TokenFieldMaxTokens {
				t.Errorf("thinking-class flags not applied: %+v", o)
			}
			if !reflect.DeepEqual(o.ReasoningEffortLevels, levels) {
				t.Errorf("ReasoningEffortLevels = %v, want %v", o.ReasoningEffortLevels, levels)
			}
		})
	}
	q := DefaultRegistry().Resolve("openai-compatible", "deepseek/deepseek-flash")
	if !reflect.DeepEqual(q.ReplayFields, []string{"reasoning_content"}) || len(q.BehaviourFlags.OpenAI.ReasoningEffortLevels) != 0 {
		t.Errorf("gateway deepseek-flash: ReplayFields = %v, ReasoningEffortLevels = %v", q.ReplayFields, q.BehaviourFlags.OpenAI.ReasoningEffortLevels)
	}
}
