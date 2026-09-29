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
	r := DefaultRegistry().Resolve("openai-responses", "gpt-5.4-nano").BehaviourFlags.OpenAIResponses
	if r.OmitSamplingParams || len(r.ReasoningEffortLevels) != 0 {
		t.Errorf("gpt-5.4-nano picked up gpt-6 Responses flags: %+v", r)
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
