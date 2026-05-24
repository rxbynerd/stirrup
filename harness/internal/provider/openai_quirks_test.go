package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// fixtureRoot is the relative path from this _test.go file to the
// per-model fixture directories. Tests run in their package's source
// directory so the path is relative to harness/internal/provider/.
const fixtureRoot = "testdata/quirks/openai-compatible"

// quirksCanonicalParams returns a representative StreamParams whose
// shape exercises the fields driven by the quirks layer (model,
// max_tokens, temperature, messages). The same shape is reused across
// every contract test so a fixture for a different model differs only
// in the parts the rules actually change.
func quirksCanonicalParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		},
	}
}

// TestOpenAIQuirks_O1MiniOmitsSamplingParams pins the o-series
// reasoning-class wire shape: max_completion_tokens emitted at the
// canonical key, sampling params (temperature) omitted even when the
// caller supplies a non-nil value. The fixture is the source of truth
// for the expected shape; AssertWireEqual normalises both sides.
func TestOpenAIQuirks_O1MiniOmitsSamplingParams(t *testing.T) {
	params := quirksCanonicalParams("o1-mini")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
		t.Fatalf("o1-mini: expected OmitSamplingParams=true, got false (rule not firing)")
	}
	body, err := json.Marshal(buildOpenAIRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "o1-mini", "request.json"), body)
}

// TestOpenAIQuirks_GPT5NanoOmitsSamplingParams mirrors the o1-mini
// case for the gpt-5 family. Same canonical-form body; pinned by the
// gpt-5* rule.
func TestOpenAIQuirks_GPT5NanoOmitsSamplingParams(t *testing.T) {
	params := quirksCanonicalParams("gpt-5-nano")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
		t.Fatalf("gpt-5-nano: expected OmitSamplingParams=true, got false (rule not firing)")
	}
	body, err := json.Marshal(buildOpenAIRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-5-nano", "request.json"), body)
}

// TestOpenAIQuirks_GPT5ChatLatestKeepsSamplingParams pins the carve-out
// in design D10: gpt-5-chat-latest matches both gpt-5* and
// gpt-5-chat*; the longer glob runs last and clears
// OmitSamplingParams, so temperature is on the wire. This test is the
// load-bearing assertion that the composition order works in
// practice, not just in the unit test in quirks_test.go.
func TestOpenAIQuirks_GPT5ChatLatestKeepsSamplingParams(t *testing.T) {
	params := quirksCanonicalParams("gpt-5-chat-latest")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	if q.BehaviourFlags.OpenAI.OmitSamplingParams {
		t.Fatalf("gpt-5-chat-latest: expected OmitSamplingParams=false after carve-out, got true (specificity order broken)")
	}
	body, err := json.Marshal(buildOpenAIRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-5-chat-latest", "request.json"), body)
}

// TestOpenAIQuirks_GPT4oNoQuirksApply pins the negative case: gpt-4o
// matches none of the openai-compatible rules. The wire body is
// therefore the canonical openaiRequest projection: max_completion_tokens,
// temperature transmitted, no extra body fields.
func TestOpenAIQuirks_GPT4oNoQuirksApply(t *testing.T) {
	params := quirksCanonicalParams("gpt-4o")
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	if q.BehaviourFlags.OpenAI.OmitSamplingParams {
		t.Fatalf("gpt-4o: expected OmitSamplingParams=false, got true (rule misfire)")
	}
	if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxCompletionTokens {
		t.Fatalf("gpt-4o: expected TokenField=max_completion_tokens, got %v", q.BehaviourFlags.OpenAI.TokenField)
	}
	body, err := json.Marshal(buildOpenAIRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(fixtureRoot, "gpt-4o", "request.json"), body)
}

// TestNoRegressionMaxCompletionTokensDefault pins design risk 1: the
// default openai-compatible body MUST emit max_completion_tokens, NOT
// the legacy max_tokens, regardless of whether a quirk rule fired.
// The current main branch hard-codes max_completion_tokens; this test
// guards against a regression where a future rule accidentally
// restores the legacy field for non-Z.ai providers.
//
// The assertion is exhaustive: every (model) combination from the
// shared canonical-params table plus a no-rule-match model produces
// the modern key and never the legacy key in the same body.
func TestNoRegressionMaxCompletionTokensDefault(t *testing.T) {
	models := []string{
		"gpt-4o",            // no rule
		"gpt-3.5-turbo",     // no rule
		"o1-mini",           // reasoning-class rule applies
		"gpt-5-nano",        // gpt-5* rule applies
		"gpt-5-chat-latest", // gpt-5-chat* carve-out applies
		"claude-3-opus",     // wrong-provider sanity check
		"random-model-xyz",  // forward-compatible no-rule sanity
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			params := quirksCanonicalParams(model)
			q := quirks.DefaultRegistry().Resolve("openai-compatible", model)
			body, err := json.Marshal(buildOpenAIRequest(params, true, q))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(body)
			if !strings.Contains(s, `"max_completion_tokens"`) {
				t.Errorf("model %q: body missing 'max_completion_tokens': %s", model, s)
			}
			if strings.Contains(s, `"max_tokens"`) {
				t.Errorf("model %q: body contains legacy 'max_tokens' (regression): %s", model, s)
			}
		})
	}
}
