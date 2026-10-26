package quirks

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staleness is the window beyond which TestRuleStaleness flags a rule
// for re-verification.
const staleness = 180 * 24 * time.Hour

// TestResolve_EmptyRegistryAllocatesEveryField pins the invariant that
// Resolve always returns a ProviderQuirks with every map and slice field
// allocated as a non-nil empty value, even when no rule fires. Apply
// closures rely on this so they can read-modify-write without nil guards.
func TestResolve_EmptyRegistryAllocatesEveryField(t *testing.T) {
	must := require.New(t)
	q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")

	must.NotNil(q.FieldRenames)
	must.NotNil(q.OmitFields)
	must.NotNil(q.ValueOverrides)
	must.NotNil(q.EnumCoercions)
	must.NotNil(q.ReplayFields)

	must.Empty(q.FieldRenames)
	must.Empty(q.OmitFields)
	must.Empty(q.ValueOverrides)
	must.Empty(q.EnumCoercions)
	must.Empty(q.ReplayFields)

	must.NotNil(q.BehaviourFlags.OpenAI.ExtraBodyFields)
	must.Empty(q.BehaviourFlags.OpenAI.ExtraBodyFields)

	want := ProviderBehaviourFlags{
		OpenAI: OpenAIBehaviourFlags{
			ExtraBodyFields:       map[string]any{},
			ReasoningEffortLevels: []string{},
		},
		OpenAIResponses: OpenAIResponsesBehaviourFlags{
			ReasoningEffortLevels: []string{},
		},
		Gemini: GeminiBehaviourFlags{
			SchemaUnsupportedFeatures: []string{},
			ThinkingLevels:            []string{},
		},
		Anthropic: AnthropicBehaviourFlags{
			EffortLevels: []string{},
		},
	}
	must.Equal(want, q.BehaviourFlags)
}

// canonicalOpenAIFieldNames mirrors the canonical Chat Completions field
// surface enumerated by harness/internal/provider/openai.go's
// isCanonicalOpenAIField (unexported, so duplicated here to cross-check
// rules without crossing the package boundary). Add new canonical fields
// here too when the adapter learns one.
var canonicalOpenAIFieldNames = map[string]bool{
	"model":                 true,
	"messages":              true,
	"tools":                 true,
	"tool_choice":           true,
	"max_completion_tokens": true,
	"max_tokens":            true,
	"temperature":           true,
	"top_p":                 true,
	"presence_penalty":      true,
	"frequency_penalty":     true,
	"logprobs":              true,
	"top_logprobs":          true,
	"logit_bias":            true,
	"stream":                true,
	"parallel_tool_calls":   true,
}

// TestBuiltinRulesValidate asserts every rule baked into the registry
// passes a structural validity check: required metadata is populated
// (Description, LastVerified, Apply) so trace attributes and the CLI
// introspection surface have something to report, and every
// FieldRenames key is in the declared canonical field surface so a
// typo in a rule cannot silently rename a non-existent field.
func TestBuiltinRulesValidate(t *testing.T) {
	for _, rule := range BuiltinRules() {
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			must.NotEmpty(rule.Description, "Description is required")
			must.False(rule.LastVerified.IsZero(), "LastVerified is required")
			must.NotNil(rule.Apply, "Apply is required")

			// The canonical-field check applies only to openai-compatible rules
			// today. When a Gemini or Anthropic rule touches FieldRenames,
			// mirror canonicalOpenAIFieldNames for that provider and extend
			// this check rather than dropping it.
			if rule.ProviderType != "openai-compatible" {
				return
			}
			q := freshQuirks()
			rule.Apply(&q)
			for key := range q.FieldRenames {
				must.True(canonicalOpenAIFieldNames[key], "FieldRenames key %q is not in the canonical openai field set", key)
			}
		})
	}
}

// TestBuiltinRulesExtraBodyFieldsNoSecrets pins that ExtraBodyFields
// values must not carry secret:// references. The map is serialised
// directly into the request body; a rule that accidentally stored a
// secret reference there would bypass RunConfig.Redact() entirely. The
// same scan covers SchemaUnsupportedFeatures, whose entries surface in
// lint error messages.
func TestBuiltinRulesExtraBodyFieldsNoSecrets(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := freshQuirks()
			rule.Apply(&q)
			for k, v := range q.BehaviourFlags.OpenAI.ExtraBodyFields {
				s, ok := v.(string)
				if !ok {
					continue
				}
				must.NotContains(s, "secret://", "ExtraBodyFields[%q] holds a secret reference", k)
			}
			for j, s := range q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures {
				must.NotContains(s, "secret://", "SchemaUnsupportedFeatures[%d] holds a secret reference", j)
			}
		})
	}
}

// knownJSONSchemaKeywords is the allow-set that
// TestBuiltinRules_SchemaUnsupportedFeaturesAreKnownKeywords cross-checks
// every SchemaUnsupportedFeatures entry against, catching a typo (e.g.
// "patern" for "pattern") that would otherwise silently never fire. Update
// when adding a new keyword to any rule's SchemaUnsupportedFeatures list.
var knownJSONSchemaKeywords = map[string]struct{}{
	"oneOf":             {},
	"anyOf":             {},
	"allOf":             {},
	"not":               {},
	"pattern":           {},
	"patternProperties": {},
	"minProperties":     {},
	"maxProperties":     {},
	"format":            {},
	"minLength":         {},
	"maxLength":         {},
	"minimum":           {},
	"maximum":           {},
	"exclusiveMinimum":  {},
	"exclusiveMaximum":  {},
	"multipleOf":        {},
	"uniqueItems":       {},
	"minItems":          {},
	"maxItems":          {},
	"minContains":       {},
	"maxContains":       {},
	"contains":          {},
	"if":                {},
	"then":              {},
	"else":              {},
	"dependentSchemas":  {},
	"dependentRequired": {},
	"$ref":              {},
	"$defs":             {},
}

// TestBuiltinRules_SchemaUnsupportedFeaturesAreKnownKeywords pins that
// every string in any rule's SchemaUnsupportedFeatures list is a known JSON
// Schema keyword (per knownJSONSchemaKeywords).
func TestBuiltinRules_SchemaUnsupportedFeaturesAreKnownKeywords(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := freshQuirks()
			rule.Apply(&q)
			for j, kw := range q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures {
				must.Contains(knownJSONSchemaKeywords, kw, "SchemaUnsupportedFeatures[%d] is not a known JSON Schema keyword (typo? or add to knownJSONSchemaKeywords if deliberate)", j)
			}
		})
	}
}

// TestRemoveFromOmit_RemovesOnlyNamedEntry covers the helper's three
// observable states. removeFromOmit has no production caller today (the
// current carve-out toggles OmitSamplingParams directly), so these cases
// keep it covered and stop it being removed as dead code.
func TestRemoveFromOmit_RemovesOnlyNamedEntry(t *testing.T) {
	t.Run("removes present entry", func(t *testing.T) {
		must := require.New(t)
		q := &ProviderQuirks{OmitFields: []string{"temperature", "top_p", "logprobs"}}
		removeFromOmit(q, "top_p")
		must.Equal([]string{"temperature", "logprobs"}, q.OmitFields)
	})
	t.Run("no-op when absent", func(t *testing.T) {
		must := require.New(t)
		q := &ProviderQuirks{OmitFields: []string{"temperature"}}
		removeFromOmit(q, "top_p")
		must.Equal([]string{"temperature"}, q.OmitFields)
	})
	t.Run("no-op when nil slice", func(t *testing.T) {
		must := require.New(t)
		q := &ProviderQuirks{}
		removeFromOmit(q, "top_p")
		must.Empty(q.OmitFields)
	})
}

// TestRuleCarveOuts pins the gpt-5-chat carve-out behaviour. The
// gpt-5* reasoning-class rule sets OmitSamplingParams=true; the
// gpt-5-chat* rule overrides it back to false because chat-class
// models accept sampling params. Specificity ordering is load-bearing:
// a reordered rule or weakened glob must fail this test rather than
// silently break the gpt-5-chat-latest wire shape.
func TestRuleCarveOuts(t *testing.T) {
	t.Run("gpt-5-chat-latest keeps sampling params", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-latest")
		must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams)
	})
	// The carve-out clears OmitSamplingParams but must not clear
	// StrictMode (set by the earlier-firing gpt-5* strict-mode rule).
	t.Run("gpt-5-chat-latest retains strict mode", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-latest")
		must.True(q.BehaviourFlags.OpenAI.StrictMode, "carve-out must not clear strict mode")
	})
	t.Run("gpt-5-nano omits sampling params", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-nano")
		must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "gpt-5* rule should fire")
	})
	t.Run("gpt-5-chat-mini also keeps sampling params", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-mini")
		must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams, "carve-out should cover the family")
	})
	// Bare o-series aliases (o1, o3, o4) ship alongside dash-suffixed
	// variants; the glob "o[1-9]*" must cover both forms. Each bare ID
	// is asserted individually so a regression names the specific alias.
	for _, bare := range []string{"o1", "o3", "o4"} {
		bare := bare
		t.Run("bare "+bare+" omits sampling params", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", bare)
			must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "o-series rule must cover the bare alias")
		})
	}
	// Two-digit series (e.g. o10-mini) match "o[1-9]*" too: [1-9] consumes
	// the leading "1" and "*" consumes "0-mini". Pinned so a future
	// tightening of the glob is a deliberate, test-visible edit.
	t.Run("o10-mini also omits sampling params (forward-compat)", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "o10-mini")
		must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "o[1-9]* should match two-digit aliases")
	})
}

// TestBuiltinRulesStrictMode checks the strict-mode rule wiring through
// DefaultRegistry rather than a synthetic registry, so a glob typo in
// builtin.go fails here. The positive cases are the models the built-in
// rules are documented to cover; the negative cases are pinned models that
// must not enable strict mode.
func TestBuiltinRulesStrictMode(t *testing.T) {
	positives := []string{
		"gpt-4o-mini",
		"gpt-4o-mini-2024-07-18",
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"gpt-5-nano",
		// carve-out clears OmitSamplingParams but must NOT clear StrictMode.
		"gpt-5-chat-latest",
	}
	for _, model := range positives {
		model := model
		t.Run(model+" enables strict mode", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", model)
			must.True(q.BehaviourFlags.OpenAI.StrictMode, "built-in rule did not fire")
		})
	}

	// Negative cases: bare gpt-4o supports strict mode per OpenAI's docs
	// but no rule covers it today; o1-mini matches the reasoning-class
	// rule but not any strict-mode rule.
	t.Run("gpt-4o does not enable strict mode", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		must.False(q.BehaviourFlags.OpenAI.StrictMode, "no built-in rule covers bare gpt-4o today")
	})
	t.Run("o1-mini omits sampling params but does not enable strict mode", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "o1-mini")
		must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams, "reasoning-class rule should fire")
		must.False(q.BehaviourFlags.OpenAI.StrictMode, "no rule enables strict mode on o-series")
	})
}

// TestNoMetacharsInKnownModelIDs reads the catalogue at
// testdata/model-ids.txt and asserts every entry is a literal model
// identifier — no glob metacharacters, which would break path.Match
// dispatch against the rules in builtin.go.
func TestNoMetacharsInKnownModelIDs(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	f, err := os.Open("testdata/model-ids.txt")
	must.NoError(err)
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		check.False(strings.ContainsAny(line, "*?[]"), "testdata/model-ids.txt:%d: model id %q contains a glob metachar", lineNo, line)
	}
	must.NoError(scanner.Err())
}

// TestKnownModelIDs_ResolutionSmoke is a forward-compatible smoke test,
// not a contract: entries in testdata/model-ids.txt that match no rule
// (e.g. gpt-4o, claude-3-*) are expected and produce a zero-value
// ProviderQuirks, so the resolution count is informational only
// (t.Logf, not t.Errorf).
func TestKnownModelIDs_ResolutionSmoke(t *testing.T) {
	f, err := os.Open("testdata/model-ids.txt")
	if err != nil {
		t.Skip("model-ids.txt unavailable")
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	provs := []string{"openai-compatible", "gemini", "anthropic"}
	resolved := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, prov := range provs {
			q := DefaultRegistry().Resolve(prov, line)
			if q.BehaviourFlags.OpenAI.OmitSamplingParams || q.BehaviourFlags.OpenAI.TokenField != TokenFieldMaxCompletionTokens || q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape != StreamArgsOff || q.ToolChoice.Supported {
				resolved++
			}
		}
	}
	t.Logf("rule resolution smoke: %d (provider,model) pairs produced a non-default ProviderQuirks", resolved)
}

// TestGeminiRule_NoMatchOnOpenAICompatProvider pins the cross-provider
// isolation invariant: the Gemini base rule keys on
// ProviderType == "gemini" and must NOT fire when the resolution is
// for the openai-compatible provider, even when the model string looks
// like a Gemini model id (e.g. routed through a third-party gateway).
func TestGeminiRule_NoMatchOnOpenAICompatProvider(t *testing.T) {
	q := DefaultRegistry().Resolve("openai-compatible", "gemini-2.5-pro")

	t.Run("gemini stream args shape stays off", func(t *testing.T) {
		must := require.New(t)
		must.Equal(StreamArgsOff, q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape, "Gemini rule must not fire on a non-gemini provider")
	})
	t.Run("openai sampling params are not omitted", func(t *testing.T) {
		must := require.New(t)
		must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams, "no rule should have fired")
	})
	t.Run("openai token field keeps its default", func(t *testing.T) {
		must := require.New(t)
		must.Equal(TokenFieldMaxCompletionTokens, q.BehaviourFlags.OpenAI.TokenField)
	})
}

// TestQuirks_ToolChoiceCapabilityRules pins that each first-party provider's
// base rule advertises the tool-choice modes its API supports, that the
// capability lands on the top-level ProviderQuirks.ToolChoice field, and
// that a provider with no rule resolves the zero value (no support). The
// Anthropic-specific assertion guards the one asymmetry: Anthropic has no
// native "none", so None must stay false while the cross-provider modes
// are true.
func TestQuirks_ToolChoiceCapabilityRules(t *testing.T) {
	t.Run("anthropic advertises auto/required/named but not none", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
		must.True(q.ToolChoice.Supported)
		must.True(q.ToolChoice.Auto)
		must.True(q.ToolChoice.Required)
		must.True(q.ToolChoice.NamedTool)
		must.False(q.ToolChoice.None, "anthropic has no native none mode")
		must.True(q.ToolChoice.RejectsTrailingAssistant, "a trailing assistant message is a prefill")
	})

	t.Run("openai-compatible advertises every mode", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		want := ToolChoiceCapability{Supported: true, Auto: true, Required: true, None: true, NamedTool: true}
		must.Equal(want, q.ToolChoice)
	})

	t.Run("gemini advertises every mode and rejects a trailing model turn", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		want := ToolChoiceCapability{Supported: true, Auto: true, Required: true, None: true, NamedTool: true, RejectsTrailingAssistant: true}
		must.Equal(want, q.ToolChoice)
	})

	t.Run("unknown provider resolves zero (no support)", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("mystery-provider", "some-model")
		must.Zero(q.ToolChoice, "no native support")
	})
}

// TestToolChoiceRules_AnyModeImpliesSupported pins the structural
// relationship the adapters depend on: any first-party rule that turns on
// a per-mode tool-choice bool must also set Supported. An adapter checks
// Supported as the master gate, so a rule that set Required without
// Supported would silently disable the feature it intended to enable.
func TestToolChoiceRules_AnyModeImpliesSupported(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := freshQuirks()
			rule.Apply(&q)
			tc := q.ToolChoice
			anyMode := tc.Auto || tc.Required || tc.None || tc.NamedTool
			if anyMode {
				must.True(tc.Supported, "a tool-choice mode is set but Supported is false")
			}
		})
	}
}

// TestQuirks_StructuredToolResultCapabilityRules pins that each first-party
// provider's base rule advertises the structured tool-result wire shape its
// API actually accepts, that the capability lands on the top-level
// ProviderQuirks.StructuredToolResults field, and that a provider with no
// rule resolves the zero value (text-only). OpenAI is the load-bearing
// negative: its tool messages are plain strings on the wire, so it must NOT
// gain the capability — the no-regression guarantee for the OpenAI adapters.
func TestQuirks_StructuredToolResultCapabilityRules(t *testing.T) {
	t.Run("anthropic advertises content-block array", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
		want := StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
		must.Equal(want, q.StructuredToolResults)
	})

	t.Run("gemini advertises object response", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		want := StructuredToolResultCapability{Supported: true, ObjectResponse: true}
		must.Equal(want, q.StructuredToolResults)
	})

	t.Run("openai-compatible stays text-only", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		must.Zero(q.StructuredToolResults, "text-only")
	})

	t.Run("bedrock stays text-only", func(t *testing.T) {
		must := require.New(t)
		// Pin bedrock as text-only so a future rule enabling Supported
		// cannot land unnoticed.
		q := DefaultRegistry().Resolve("bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0")
		must.Zero(q.StructuredToolResults, "text-only")
	})

	t.Run("unknown provider resolves zero (text-only)", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("mystery-provider", "some-model")
		must.Zero(q.StructuredToolResults, "text-only")
	})
}

// TestQuirks_AnthropicOmitSamplingParamsCapabilityRules pins which Claude model
// families return an HTTP 400 on a non-default temperature and therefore
// have sampling params omitted. Opus 4.6, Sonnet 4.6, and Haiku 4.5 still
// accept one, so they are the load-bearing negative cases guarding against
// an over-broad glob.
func TestQuirks_AnthropicOmitSamplingParamsCapabilityRules(t *testing.T) {
	omits := []string{
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-sonnet-5",
		"claude-sonnet-5-5",
		"claude-opus-5",
		"claude-opus-5-5",
		"claude-fable-5",
		"claude-fable-5-1",
		"claude-haiku-5-5",
		"claude-mythos-5",
	}
	for _, model := range omits {
		t.Run(model+" omits sampling params", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.True(q.BehaviourFlags.Anthropic.OmitSamplingParams, "rule not firing")
		})
	}

	accepts := []string{
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-haiku-4-5-20251001",
		// claude-mythos-preview's sampling-param behaviour is unconfirmed
		// against a live capture, so no rule matches it; pinned so widening
		// "claude-mythos-5*" to "claude-mythos*" is a deliberate edit.
		"claude-mythos-preview",
	}
	for _, model := range accepts {
		t.Run(model+" keeps sampling params", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.False(q.BehaviourFlags.Anthropic.OmitSamplingParams, "rule misfire")
		})
	}
}

// TestAnthropicEffortLevels_PerModelAllowList pins the per-model
// output_config.effort allow-lists. An empty list is load-bearing: Haiku 4.5
// and Sonnet 4.5 return HTTP 400 on the effort key itself, so those models
// must resolve to "send nothing" rather than to a guessed list.
func TestAnthropicEffortLevels_PerModelAllowList(t *testing.T) {
	full := []string{"low", "medium", "high", "xhigh", "max"}
	cases := []struct {
		model string
		want  []string
	}{
		{"claude-haiku-4-5-20251001", []string{}},
		{"claude-sonnet-4-5-20250929", []string{}},
		{"claude-opus-4-5-20251101", []string{"low", "medium", "high"}},
		{"claude-opus-4-6", []string{"low", "medium", "high", "max"}},
		{"claude-sonnet-4-6", []string{"low", "medium", "high", "max"}},
		{"claude-opus-4-7", full},
		{"claude-opus-4-8", full},
		{"claude-sonnet-5", full},
		{"claude-sonnet-5-5", full},
		{"claude-opus-5", full},
		{"claude-opus-5-5", full},
		{"claude-fable-5", full},
		{"claude-fable-5-1", full},
		{"claude-haiku-5-5", full},
		// Unprobed: keeps the send-nothing default.
		{"claude-mythos-5", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			must := require.New(t)
			got := DefaultRegistry().Resolve("anthropic", tc.model).BehaviourFlags.Anthropic.EffortLevels
			must.Equal(tc.want, got)
		})
	}
}

// TestAnthropicPromptCaching_ResolvesOnForAnthropicOnly pins that every
// Anthropic model resolves prompt caching on, and that a registry with no
// rule leaves it off so a rule-less request keeps the uncached wire shape.
func TestAnthropicPromptCaching_ResolvesOnForAnthropicOnly(t *testing.T) {
	models := []string{
		"claude-haiku-4-5-20251001",
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-6",
		"claude-opus-4-5",
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-sonnet-5",
		"claude-sonnet-5-5",
		"claude-opus-5-5",
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-haiku-5-5",
	}
	for _, model := range models {
		t.Run(model+" enables prompt caching", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.True(q.BehaviourFlags.Anthropic.PromptCaching)
		})
	}
	t.Run("empty registry leaves prompt caching off", func(t *testing.T) {
		must := require.New(t)
		q := NewRegistry(nil).Resolve("anthropic", "claude-sonnet-5-5")
		must.False(q.BehaviourFlags.Anthropic.PromptCaching)
	})
	t.Run("openai-compatible does not get prompt caching", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "claude-sonnet-5-5")
		must.False(q.BehaviourFlags.Anthropic.PromptCaching, "rule scoped to the anthropic provider")
	})
}

// TestAnthropicForcedToolChoiceRemoved pins the models that return HTTP
// 400 on tool_choice "any" and "tool". They must advertise auto only so the
// escalation policy picks its prompt fallback instead of a forced choice,
// while the earlier 5.x models keep the full base capability.
func TestAnthropic_ForcedToolChoiceRemoved(t *testing.T) {
	autoOnly := ToolChoiceCapability{Supported: true, Auto: true}
	for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1", "claude-mythos-5-1"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.Equal(autoOnly, q.ToolChoice)
			must.True(q.ParallelToolCalls.Disable, "disable_parallel_tool_use still works with auto")
		})
	}
	for _, model := range []string{"claude-sonnet-5", "claude-opus-5", "claude-fable-5", "claude-opus-4-8", "claude-haiku-5-5"} {
		t.Run(model+" keeps forced choice", func(t *testing.T) {
			must := require.New(t)
			tc := DefaultRegistry().Resolve("anthropic", model).ToolChoice
			must.True(tc.Required && tc.NamedTool, "ToolChoice = %+v, want Required and NamedTool", tc)
		})
	}
}

// TestAnthropicOmitSamplingParamsComposesWithExistingCapabilities pins that
// the claude-*-glob OmitSamplingParams rules, which resolve after the
// pre-existing "anthropic / *" capability rules, do not clobber
// ToolChoice, StructuredToolResults, or ParallelToolCalls.
func TestAnthropic_OmitSamplingParamsComposesWithCapabilities(t *testing.T) {
	baseline := DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")

	for _, model := range []string{
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-sonnet-5",
		"claude-opus-5",
		"claude-fable-5",
		"claude-mythos-5",
	} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.True(q.BehaviourFlags.Anthropic.OmitSamplingParams, "rule not firing")
			must.Equal(baseline.ToolChoice, q.ToolChoice, "differs from the claude-sonnet-4-5 baseline")
			must.Equal(baseline.StructuredToolResults, q.StructuredToolResults, "differs from the claude-sonnet-4-5 baseline")
			must.Equal(baseline.ParallelToolCalls, q.ParallelToolCalls, "differs from the claude-sonnet-4-5 baseline")
			must.Equal(baseline.ToolExamples, q.ToolExamples, "differs from the claude-sonnet-4-5 baseline")
		})
	}
}

// TestStructuredToolResultRulesSetSupportedWhenAnyShape pins the structural
// relationship the adapters depend on: any first-party rule that turns on a
// structured wire-shape bool must also set Supported. An adapter checks
// Supported as the master gate, so a rule that set ObjectResponse without
// Supported would silently keep the provider text-only.
func TestStructuredToolResultRules_SetSupportedWhenAnyShape(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := freshQuirks()
			rule.Apply(&q)
			sr := q.StructuredToolResults
			if sr.ObjectResponse || sr.ContentBlockArray {
				must.True(sr.Supported, "a structured-result shape is set but Supported is false")
			}
		})
	}
}

// TestParallelToolCalls_CapabilityRules pins which providers advertise a
// native parallel-tool-call control. Gemini and Bedrock are the load-bearing
// negatives: builtin.go names them as deliberately absent, so a future rule
// flipping Supported=true cannot land unnoticed.
func TestParallelToolCalls_CapabilityRules(t *testing.T) {
	supported := map[string]string{
		"anthropic":         "claude-sonnet-4-5",
		"openai-compatible": "gpt-4o",
		"openai-responses":  "gpt-4o",
	}
	for provider, model := range supported {
		t.Run(provider+" advertises disable", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve(provider, model)
			must.Equal(ParallelToolCallsCapability{Supported: true, Disable: true}, q.ParallelToolCalls)
		})
	}

	unsupported := map[string]string{
		"gemini":         "gemini-2.5-pro",
		"bedrock":        "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"mystery-vendor": "some-model",
	}
	for provider, model := range unsupported {
		t.Run(provider+" stays unsupported", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve(provider, model)
			must.Zero(q.ParallelToolCalls)
		})
	}
}

// TestToolExamples_CapabilityRules pins which providers accept worked tool
// examples and where: Anthropic on its native input_examples field, the
// OpenAI adapters in the JSON-Schema `examples` keyword. Gemini is the
// load-bearing negative: its Schema dialect rejects `examples`, so the
// example must reach the model via the description text instead — never
// folded into the schema.
func TestToolExamples_CapabilityRules(t *testing.T) {
	supported := map[string]struct {
		model string
		want  ToolExamplesCapability
	}{
		"anthropic":         {"claude-sonnet-4-5", ToolExamplesCapability{Supported: true, Native: true}},
		"openai-compatible": {"gpt-4o", ToolExamplesCapability{Supported: true}},
		"openai-responses":  {"gpt-4o", ToolExamplesCapability{Supported: true}},
	}
	for provider, tc := range supported {
		t.Run(provider+" accepts examples", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve(provider, tc.model)
			must.Equal(tc.want, q.ToolExamples)
		})
	}

	unsupported := map[string]string{
		"gemini":         "gemini-2.5-pro",
		"bedrock":        "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"mystery-vendor": "some-model",
	}
	for provider, model := range unsupported {
		t.Run(provider+" stays unsupported", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve(provider, model)
			must.Zero(q.ToolExamples)
		})
	}

	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5"} {
		t.Run("anthropic "+model+" sends input_examples natively", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("anthropic", model)
			must.Equal(ToolExamplesCapability{Supported: true, Native: true}, q.ToolExamples)
		})
	}
}

// TestOpenAIResponses_BehaviourFlags pins the Responses-specific wire
// divergences resolved by the builtin "openai-responses / *" rule. The adapter
// reads these flags from the resolved ProviderQuirks instead of hard-coding
// them, so each one is asserted explicitly and a dropped pin fails here.
func TestOpenAIResponses_BehaviourFlags(t *testing.T) {
	must := require.New(t)
	q := DefaultRegistry().Resolve("openai-responses", "gpt-4o")
	rf := q.BehaviourFlags.OpenAIResponses
	must.Equal(TokenFieldMaxOutputTokens, rf.TokenField)
	must.Equal(StoreFalse, rf.StoreMode)
	must.Equal(TypedInputItems, rf.InputItemShape)

	for _, model := range []string{"gpt-4o", "gpt-5.4", "gpt-6-astra", "some-new-model"} {
		t.Run("openai-responses "+model+" enables prompt cache key", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-responses", model)
			must.True(q.BehaviourFlags.OpenAIResponses.PromptCacheKey)
		})
	}
	for _, providerType := range []string{"openai-compatible", "anthropic"} {
		t.Run(providerType+" leaves prompt cache key off", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve(providerType, "gpt-4o")
			must.False(q.BehaviourFlags.OpenAIResponses.PromptCacheKey, "rule is scoped to openai-responses")
		})
	}

	// With no matching rule the flags resolve to their zero values, with
	// ReasoningEffortLevels allocated as an empty slice.
	empty := NewRegistry(nil).Resolve("openai-responses", "gpt-4o")
	wantEmpty := OpenAIResponsesBehaviourFlags{ReasoningEffortLevels: []string{}}
	must.Equal(wantEmpty, empty.BehaviourFlags.OpenAIResponses)
}

// TestParallelToolCallsRules_SetSupportedWhenDisable pins the structural
// relationship the adapters depend on: any rule that turns on the Disable bool
// must also set Supported. An adapter checks Supported as the master gate, so
// a rule that set Disable without Supported would silently no-op.
func TestParallelToolCallsRules_SetSupportedWhenDisable(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
			q := freshQuirks()
			rule.Apply(&q)
			if q.ParallelToolCalls.Disable {
				must.True(q.ParallelToolCalls.Supported, "Disable is set but Supported is false")
			}
		})
	}
}

// freshQuirks returns a ProviderQuirks with the same map/slice
// pre-initialisation Resolve performs, for rule-materialisation tests
// that call Apply directly.
func freshQuirks() ProviderQuirks {
	return ProviderQuirks{
		FieldRenames:   map[string]string{},
		OmitFields:     []string{},
		ValueOverrides: map[string]Value{},
		EnumCoercions:  map[string]map[string]string{},
		ReplayFields:   []string{},
		BehaviourFlags: ProviderBehaviourFlags{
			OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}},
			Gemini: GeminiBehaviourFlags{SchemaUnsupportedFeatures: []string{}},
		},
	}
}

// TestReplayFieldsRules_DeepSeekReasoner pins that the deepseek-reasoner rule
// populates ReplayFields with the documented path. The isolation cases assert
// that the rule does not fire for other DeepSeek-family models and does not
// fire when the same model name is routed through a non-openai-compatible
// provider.
func TestReplayFieldsRules_DeepSeekReasoner(t *testing.T) {
	t.Run("fires on deepseek-reasoner", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-reasoner")
		must.Equal([]string{"reasoning_content"}, q.ReplayFields)
		// The reasoner rule carries the same behaviour-flag set as the v4
		// rule: sampling params suppressed, legacy max_tokens key, no
		// strict mode (Beta-only, unreliable).
		oai := q.BehaviourFlags.OpenAI
		must.True(oai.OmitSamplingParams)
		must.Equal(TokenFieldMaxTokens, oai.TokenField)
		must.False(oai.StrictMode)
	})
	t.Run("fires on deepseek-reasoner-lite (suffix variant)", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-reasoner-lite")
		must.Equal([]string{"reasoning_content"}, q.ReplayFields)
	})
	t.Run("does not fire on unrelated openai-compatible models", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-chat")
		must.Empty(q.ReplayFields)
	})
	t.Run("does not fire when routed through a different provider", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("gemini", "deepseek-reasoner")
		must.NotContains(q.ReplayFields, "reasoning_content", "DeepSeek rule leaked into gemini resolution")
	})
}

// TestReplayFieldsRules_DeepSeekV4 pins that the deepseek-v4 rule fires on the
// v4 family ids and not on earlier DeepSeek models.
func TestReplayFieldsRules_DeepSeekV4(t *testing.T) {
	t.Run("fires on deepseek-v4", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v4")
		must.Equal([]string{"reasoning_content"}, q.ReplayFields)
	})
	t.Run("fires on deepseek-v4-chat", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v4-chat")
		must.Equal([]string{"reasoning_content"}, q.ReplayFields)
	})
	t.Run("does not fire on deepseek-v3 (no rule)", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v3")
		must.Empty(q.ReplayFields)
	})
}

// TestDeepSeekV4Resolution pins the full resolved quirk set for the
// first-party v4 ids: thinking-mode behaviour flags, no strict mode
// (Beta-only, unreliable), and the base openai-compatible capabilities
// left unperturbed.
func TestDeepSeekV4_Resolution(t *testing.T) {
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", model)
			must.Equal([]string{"reasoning_content"}, q.ReplayFields)
			oai := q.BehaviourFlags.OpenAI
			must.True(oai.OmitSamplingParams, "thinking mode ignores sampling params; logprobs hard-errors")
			must.Equal(TokenFieldMaxTokens, oai.TokenField, "DeepSeek docs use the legacy key")
			must.False(oai.StrictMode, "DeepSeek strict tools are Beta-only and unreliable")
			// The openai-compatible base "*" rules still compose: full
			// tool_choice and parallel/examples capabilities, text-only
			// structured results.
			wantTC := ToolChoiceCapability{Supported: true, Auto: true, Required: true, None: true, NamedTool: true}
			must.Equal(wantTC, q.ToolChoice, "base rule must compose")
			must.Zero(q.StructuredToolResults, "structured results must stay text-only")
		})
	}
}

// TestDeepSeekV4GatewayPrefixResolution pins the sibling rule for
// OpenRouter-style prefixed ids. path.Match's `*` does not cross `/`,
// which cuts both ways: the bare deepseek-v4* glob cannot match the
// prefixed id (hence the sibling rule), and the openai-compatible base
// "*" rules cannot either — so the gateway ids resolve the DeepSeek
// flags but zero-value capabilities. Both directions are pinned so a
// future glob change is a deliberate edit.
func TestDeepSeekV4_GatewayPrefixResolution(t *testing.T) {
	for _, model := range []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q, applied := DefaultRegistry().ResolveWithRules("openai-compatible", model)
			must.Equal([]string{"reasoning_content"}, q.ReplayFields)
			oai := q.BehaviourFlags.OpenAI
			must.True(oai.OmitSamplingParams)
			must.Equal(TokenFieldMaxTokens, oai.TokenField)
			must.False(oai.StrictMode)
			// The DeepSeek-specific wire behaviour (replay, sampling,
			// token key) comes solely from the gateway-prefix rule: the
			// bare "*" reasoning/sampling globs do not cross the slash.
			// Capabilities, however, now arrive from the "*/*" sibling
			// rules added so vendor-prefixed ids advertise the same tool
			// surface as bare ids — so more than one rule contributes.
			descs := make([]string, 0, len(applied))
			for _, r := range applied {
				descs = append(descs, r.Description)
			}
			must.Contains(strings.Join(descs, "\n"), "gateway prefix", "applied rules %v should include the gateway-prefix rule", descs)
			// The "*/*" siblings restore the tool surface for the slash id.
			must.True(q.ToolChoice.Supported, "the */* sibling should cover vendor-prefixed ids")
			must.True(q.ParallelToolCalls.Supported, "the */* sibling should cover vendor-prefixed ids")
		})
	}

	t.Run("bare ids do not fire the gateway rule", func(t *testing.T) {
		must := require.New(t)
		_, applied := DefaultRegistry().ResolveWithRules("openai-compatible", "deepseek-v4-flash")
		for _, r := range applied {
			must.NotContains(r.Description, "gateway prefix")
		}
	})

	for _, model := range []string{"deepseek/deepseek-chat", "deepseek/deepseek-v3", "openrouter/deepseek-v4-flash"} {
		t.Run(model+" does not fire", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", model)
			must.Empty(q.ReplayFields)
		})
	}
}

// TestVendorPrefixed_CapabilityRules pins the "*/*" sibling rules that
// advertise the openai-compatible tool surface for one level of vendor
// prefix (qwen/..., acme/...), the form LM Studio and OpenRouter serve.
// path.Match's "*" cannot cross "/", so without these siblings a
// locally-hosted model would be silently denied native tool_choice and
// parallel_tool_calls.
func TestVendorPrefixed_CapabilityRules(t *testing.T) {
	for _, model := range []string{"qwen/qwen3.6-27b", "acme/foo-model"} {
		t.Run(model+" gains the full tool surface", func(t *testing.T) {
			must := require.New(t)
			q := DefaultRegistry().Resolve("openai-compatible", model)
			must.True(q.ToolChoice.Supported, "ToolChoice.Supported should be true")
			must.True(q.ParallelToolCalls.Supported, "ParallelToolCalls.Supported should be true")
			must.True(q.ToolExamples.Supported, "ToolExamples.Supported should be true")
		})
	}

	t.Run("bare ids are unchanged", func(t *testing.T) {
		must := require.New(t)
		// The bare id continues to resolve via the "*" base rule; the
		// "*/*" sibling must not regress it.
		q := DefaultRegistry().Resolve("openai-compatible", "qwen3.6-35b-a3b-mlx")
		must.True(q.ToolChoice.Supported, "bare id lost ToolChoice support")
		must.True(q.ParallelToolCalls.Supported, "bare id lost ParallelToolCalls support")
	})

	t.Run("only one prefix level matches", func(t *testing.T) {
		must := require.New(t)
		// "*/*" matches exactly one slash; deeper nesting is intentionally
		// out of scope, so a/b/c stays at the zero (unsupported) value.
		q := DefaultRegistry().Resolve("openai-compatible", "a/b/c")
		must.False(q.ToolChoice.Supported, "a/b/c must not match */*")
	})
}

// TestReplayFieldsRules_Gemini3 pins the gemini-3* ReplayFields rule: it fires
// on gemini-3* model ids and registers the sibling-of-functionCall
// thoughtSignature path, and it does not fire on 2.x or on openai-compatible
// resolutions that use a Gemini-like model name.
func TestReplayFieldsRules_Gemini3(t *testing.T) {
	t.Run("fires on gemini-3.1-pro-preview", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("gemini", "gemini-3.1-pro-preview")
		must.Contains(q.ReplayFields, "candidates[].content.parts[].thoughtSignature")
	})
	t.Run("does not fire on gemini-2.5-pro", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		must.Empty(q.ReplayFields)
	})
	t.Run("does not fire when routed through openai-compatible", func(t *testing.T) {
		must := require.New(t)
		q := DefaultRegistry().Resolve("openai-compatible", "gemini-3.1-pro-preview")
		must.Empty(q.ReplayFields)
	})
}

// TestRuleStaleness logs, rather than fails, any rule whose LastVerified is
// more than 180 days before now. Staleness is a signal to re-verify the rule
// against the upstream wire shape, not a CI failure.
func TestRuleStaleness(t *testing.T) {
	cutoff := time.Now().Add(-staleness)
	for _, rule := range BuiltinRules() {
		if rule.LastVerified.Before(cutoff) {
			t.Logf("STALE: rule %q last verified %s (>180d ago); re-verify against the upstream wire shape", rule.Description, rule.LastVerified.Format("2006-01-02"))
		}
	}
}

// TestRegistry_Composition pins the specificity-then-declaration-order
// composition rule: the longer ModelMatch glob wins when two rules touch
// the same field. The wildcard rule sets a baseline; the more specific
// glob overrides it for matching models only.
func TestRegistry_Composition(t *testing.T) {
	wildcard := Rule{
		ProviderType: "openai-compatible",
		ModelMatch:   "*",
		Description:  "wildcard: token field = max_tokens",
		LastVerified: Date("2026-05-24"),
		Apply: func(q *ProviderQuirks) {
			q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
		},
	}
	specific := Rule{
		ProviderType: "openai-compatible",
		ModelMatch:   "gpt-5*",
		Description:  "gpt-5*: token field = max_completion_tokens",
		LastVerified: Date("2026-05-24"),
		Apply: func(q *ProviderQuirks) {
			q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxCompletionTokens
		},
	}
	reg := NewRegistry([]Rule{wildcard, specific})

	t.Run("specific glob overrides the wildcard when both match", func(t *testing.T) {
		must := require.New(t)
		got := reg.Resolve("openai-compatible", "gpt-5-nano")
		must.Equal(TokenFieldMaxCompletionTokens, got.BehaviourFlags.OpenAI.TokenField)
	})

	t.Run("wildcard applies when only it matches", func(t *testing.T) {
		must := require.New(t)
		got := reg.Resolve("openai-compatible", "gpt-4o")
		must.Equal(TokenFieldMaxTokens, got.BehaviourFlags.OpenAI.TokenField)
	})

	t.Run("no rule applies to another provider type", func(t *testing.T) {
		must := require.New(t)
		got := reg.Resolve("anthropic", "gpt-5-nano")
		must.Equal(TokenFieldMaxCompletionTokens, got.BehaviourFlags.OpenAI.TokenField, "TokenField keeps its zero default")
	})
}

// TestValue_Constructors checks that each New*Value constructor sets its own
// field to the given value and leaves every other field unset.
func TestValue_Constructors(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		must := require.New(t)
		v := NewStringValue("hello")
		must.NotNil(v.String)
		must.Equal("hello", *v.String)
		must.Nil(v.Int)
		must.Nil(v.Float)
		must.Nil(v.Bool)
		must.False(v.Null)
	})
	t.Run("int", func(t *testing.T) {
		must := require.New(t)
		v := NewIntValue(42)
		must.NotNil(v.Int)
		must.Equal(42, *v.Int)
		must.Nil(v.String)
		must.Nil(v.Float)
		must.Nil(v.Bool)
		must.False(v.Null)
	})
	t.Run("float", func(t *testing.T) {
		must := require.New(t)
		v := NewFloatValue(3.14)
		must.NotNil(v.Float)
		must.InDelta(3.14, *v.Float, 1e-9)
		must.Nil(v.String)
		must.Nil(v.Int)
		must.Nil(v.Bool)
		must.False(v.Null)
	})
	t.Run("bool", func(t *testing.T) {
		must := require.New(t)
		v := NewBoolValue(true)
		must.NotNil(v.Bool)
		must.True(*v.Bool)
		must.Nil(v.String)
		must.Nil(v.Int)
		must.Nil(v.Float)
		must.False(v.Null)
	})
	t.Run("null", func(t *testing.T) {
		must := require.New(t)
		v := NewNullValue()
		must.True(v.Null)
		must.Nil(v.String)
		must.Nil(v.Int)
		must.Nil(v.Float)
		must.Nil(v.Bool)
	})
}

// TestDefaultRegistryConcurrentAccess stresses the sync.Once-guarded
// DefaultRegistry singleton: 16 goroutines x 100 resolutions each.
// Combined with `go test -race`, this asserts the singleton is
// constructed exactly once and that Resolve does not mutate any
// shared state.
func TestDefaultRegistryConcurrentAccess(t *testing.T) {
	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
				// Touch the maps so a future regression that returned a
				// shared map would race under -race.
				q.FieldRenames["k"] = "v"
				q.OmitFields = append(q.OmitFields, "x")
			}
		}()
	}
	wg.Wait()
}

// TestRule_Matches pins the exported predicate the CLI shares with Resolve.
// It covers exact match, empty-ModelMatch wildcard, glob match and non-match,
// ProviderType mismatch, and a malformed glob, which returns false rather than
// panicking.
func TestRule_Matches(t *testing.T) {
	cases := []struct {
		name         string
		rule         Rule
		providerType string
		model        string
		want         bool
	}{
		{
			name:         "exact match with empty ModelMatch wildcard",
			rule:         Rule{ProviderType: "openai-compatible", ModelMatch: ""},
			providerType: "openai-compatible",
			model:        "gpt-4o",
			want:         true,
		},
		{
			name:         "glob match",
			rule:         Rule{ProviderType: "openai-compatible", ModelMatch: "gpt-5*"},
			providerType: "openai-compatible",
			model:        "gpt-5-nano",
			want:         true,
		},
		{
			name:         "glob non-match",
			rule:         Rule{ProviderType: "openai-compatible", ModelMatch: "gpt-5*"},
			providerType: "openai-compatible",
			model:        "gpt-4o",
			want:         false,
		},
		{
			name:         "ProviderType mismatch",
			rule:         Rule{ProviderType: "anthropic", ModelMatch: "*"},
			providerType: "openai-compatible",
			model:        "gpt-5-nano",
			want:         false,
		},
		{
			name:         "malformed glob returns false (no panic)",
			rule:         Rule{ProviderType: "openai-compatible", ModelMatch: "gpt-[5"},
			providerType: "openai-compatible",
			model:        "gpt-5-nano",
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := RuleMatches(tc.rule, tc.providerType, tc.model)
			must.Equal(tc.want, got, "RuleMatches(%+v, %q, %q)", tc.rule, tc.providerType, tc.model)
		})
	}
}

// TestOpenAITokenField_MarshalJSON pins the wire form of each named
// constant and round-trips it through UnmarshalJSON, so CLI output can be
// parsed back into the same struct.
func TestOpenAITokenField_MarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		val  OpenAITokenField
		want string
	}{
		{"max completion tokens", TokenFieldMaxCompletionTokens, `"max_completion_tokens"`},
		{"max tokens", TokenFieldMaxTokens, `"max_tokens"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round OpenAITokenField
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)
		})
	}
	t.Run("unknown value marshals as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		// An out-of-range value must render as "unknown(N)" rather than
		// panicking so a forward-compatible reader still gets parseable
		// JSON from a newer harness binary.
		got, err := json.Marshal(OpenAITokenField(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("unknown string is rejected on unmarshal", func(t *testing.T) {
		must := require.New(t)
		// Unknown strings reject; silent acceptance would defeat the
		// point of the named constants.
		var f OpenAITokenField
		must.Error(json.Unmarshal([]byte(`"bogus"`), &f))
	})
}

// TestGeminiStreamArgsShape_MarshalJSON applies the OpenAI token-field
// checks to the Gemini stream-args enum.
func TestGeminiStreamArgsShape_MarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		val  GeminiStreamArgsShape
		want string
	}{
		{"off", StreamArgsOff, `"off"`},
		{"v2 snapshot", StreamArgsV2Snapshot, `"v2_snapshot"`},
		{"v3 deltas", StreamArgsV3Deltas, `"v3_deltas"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round GeminiStreamArgsShape
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)
		})
	}
	t.Run("unknown value marshals as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		got, err := json.Marshal(GeminiStreamArgsShape(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("unknown string is rejected on unmarshal", func(t *testing.T) {
		must := require.New(t)
		var s GeminiStreamArgsShape
		must.Error(json.Unmarshal([]byte(`"bogus"`), &s))
	})
}

// TestOpenAIResponsesTokenField_MarshalJSON applies the OpenAI token-field
// checks to the Responses token-field enum, locking the wire-key string
// against a typo that the rest of the suite would not catch.
func TestOpenAIResponsesTokenField_MarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		val  OpenAIResponsesTokenField
		want string
	}{
		{"max output tokens", TokenFieldMaxOutputTokens, `"max_output_tokens"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round OpenAIResponsesTokenField
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)
		})
	}
	t.Run("unknown value marshals as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		got, err := json.Marshal(OpenAIResponsesTokenField(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("unknown string is rejected on unmarshal", func(t *testing.T) {
		must := require.New(t)
		var f OpenAIResponsesTokenField
		must.Error(json.Unmarshal([]byte(`"bogus"`), &f))
	})
}

// TestOpenAIResponsesStoreMode_MarshalJSON applies the OpenAI token-field
// checks to the Responses store-mode enum.
func TestOpenAIResponsesStoreMode_MarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		val  OpenAIResponsesStoreMode
		want string
	}{
		{"store false", StoreFalse, `"store_false"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round OpenAIResponsesStoreMode
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)
		})
	}
	t.Run("unknown value marshals as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		got, err := json.Marshal(OpenAIResponsesStoreMode(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("unknown string is rejected on unmarshal", func(t *testing.T) {
		must := require.New(t)
		var s OpenAIResponsesStoreMode
		must.Error(json.Unmarshal([]byte(`"bogus"`), &s))
	})
}

// TestOpenAIResponsesInputShape_MarshalJSON applies the OpenAI token-field
// checks to the Responses input-item shape enum.
func TestOpenAIResponsesInputShape_MarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		val  OpenAIResponsesInputShape
		want string
	}{
		{"typed input items", TypedInputItems, `"typed_input_items"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round OpenAIResponsesInputShape
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)
		})
	}
	t.Run("unknown value marshals as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		got, err := json.Marshal(OpenAIResponsesInputShape(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("unknown string is rejected on unmarshal", func(t *testing.T) {
		must := require.New(t)
		var s OpenAIResponsesInputShape
		must.Error(json.Unmarshal([]byte(`"bogus"`), &s))
	})
}

// TestValue_JSONTags pins the camelCase key each Value constructor
// emits. Operators read CLI output as JSON, so these keys must stay stable.
func TestValue_JSONTags(t *testing.T) {
	cases := []struct {
		name string
		val  Value
		want string
	}{
		{"string", NewStringValue("foo"), `{"string":"foo"}`},
		{"int", NewIntValue(42), `{"int":42}`},
		{"float", NewFloatValue(3.14), `{"float":3.14}`},
		{"bool true", NewBoolValue(true), `{"bool":true}`},
		{"bool false", NewBoolValue(false), `{"bool":false}`},
		{"null", NewNullValue(), `{"null":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))
		})
	}
}

// TestGatewayPrefixResolution_OpenAIReasoningClass covers the vendor-prefixed
// ids gateways serve for OpenAI reasoning models. path.Match's `*` does not
// cross `/`, so the bare */gpt-5* and */o[1-9]* globs miss these ids; the
// prefixed sibling rules must also omit sampling params.
func TestGatewayPrefixResolution_OpenAIReasoningClass(t *testing.T) {
	r := DefaultRegistry()

	reasoningModels := []string{
		"openai/gpt-5.6-terra",
		"openai/gpt-5.6-sol",
		"openai/gpt-5.6-luna",
		"openai/gpt-5-nano",
		"openai/o3-mini",
		"azure/o1",
	}
	for _, model := range reasoningModels {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := r.Resolve("openai-compatible", model)
			must.True(q.BehaviourFlags.OpenAI.OmitSamplingParams,
				"a gateway-prefixed reasoning model must omit sampling params")

			// The bare form must keep resolving identically; the sibling
			// rules add coverage rather than moving it.
			bare := model[strings.Index(model, "/")+1:]
			bq := r.Resolve("openai-compatible", bare)
			must.True(bq.BehaviourFlags.OpenAI.OmitSamplingParams,
				"bare form %q must omit sampling params", bare)
		})
	}

	// The chat-class carve-out must survive one level of prefix too,
	// and it must run after the broader */gpt-5* rule.
	chatModels := []string{"gpt-5-chat-latest", "openai/gpt-5-chat-latest"}
	for _, model := range chatModels {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := r.Resolve("openai-compatible", model)
			must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams,
				"chat-class models accept sampling params")
		})
	}

	// Non-reasoning ids must not be caught by the new globs.
	nonReasoningModels := []string{"openai/gpt-4o", "anthropic/claude-sonnet-5"}
	for _, model := range nonReasoningModels {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := r.Resolve("openai-compatible", model)
			must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams)
		})
	}
}

// TestContextWindow_OnlyRegisteredModelsHaveWindow pins which models carry a
// registered context window; every other model resolves to zero and keeps
// the loop default.
func TestContextWindow_OnlyRegisteredModelsHaveWindow(t *testing.T) {
	reg := DefaultRegistry()
	cases := []struct {
		name            string
		provider, model string
		want            int
	}{
		{"openai-compatible gpt-4o", "openai-compatible", "gpt-4o", 128_000},
		{"openai-compatible dated gpt-4o-mini", "openai-compatible", "gpt-4o-mini-2024-07-18", 128_000},
		{"gateway-prefixed gpt-4o", "openai-compatible", "openai/gpt-4o", 128_000},
		{"openai-responses gpt-4o", "openai-responses", "gpt-4o", 128_000},
		{"unregistered openai-compatible gpt-4.1", "openai-compatible", "gpt-4.1", 0},
		{"unregistered openai-compatible gpt-5", "openai-compatible", "gpt-5", 0},
		{"unregistered openai-responses gpt-6", "openai-responses", "gpt-6", 0},
		{"unregistered anthropic claude-sonnet-4-6", "anthropic", "claude-sonnet-4-6", 0},
		{"unregistered gemini 2.5 pro", "gemini", "gemini-2.5-pro", 0},
		{"unregistered bedrock claude-sonnet-4-6", "bedrock", "anthropic.claude-sonnet-4-6-v1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := reg.Resolve(tc.provider, tc.model).ContextWindow
			must.Equal(tc.want, got)
		})
	}
}
