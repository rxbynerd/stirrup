package quirks

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// staleness is the window beyond which TestRuleStaleness flags a rule
// for re-verification. Kept in step with the design's §2.3 figure.
const staleness = 180 * 24 * time.Hour

// TestResolveEmptyRegistry pins the invariant that Resolve always
// returns a ProviderQuirks with every map and slice field allocated as
// a non-nil empty value, even when no rule fires. Apply closures rely
// on this so they can read-modify-write without nil guards.
func TestResolveEmptyRegistry(t *testing.T) {
	q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
	if q.FieldRenames == nil {
		t.Error("FieldRenames must be non-nil")
	}
	if q.OmitFields == nil {
		t.Error("OmitFields must be non-nil")
	}
	if q.ValueOverrides == nil {
		t.Error("ValueOverrides must be non-nil")
	}
	if q.EnumCoercions == nil {
		t.Error("EnumCoercions must be non-nil")
	}
	if q.ReplayFields == nil {
		t.Error("ReplayFields must be non-nil")
	}
	// And the maps are empty when no rule matched.
	if len(q.FieldRenames) != 0 || len(q.OmitFields) != 0 || len(q.ValueOverrides) != 0 || len(q.EnumCoercions) != 0 || len(q.ReplayFields) != 0 {
		t.Errorf("expected empty collections, got %+v", q)
	}
	// Behaviour flags must be at their zero values so adapters fall
	// through to today's behaviour — with the documented exception that
	// OpenAI.ExtraBodyFields is pre-initialised to a non-nil empty map
	// so rules can read-modify-write without nil guards.
	if q.BehaviourFlags.OpenAI.ExtraBodyFields == nil {
		t.Error("BehaviourFlags.OpenAI.ExtraBodyFields must be non-nil")
	}
	if len(q.BehaviourFlags.OpenAI.ExtraBodyFields) != 0 {
		t.Errorf("BehaviourFlags.OpenAI.ExtraBodyFields = %+v, want empty", q.BehaviourFlags.OpenAI.ExtraBodyFields)
	}
	want := ProviderBehaviourFlags{
		OpenAI: OpenAIBehaviourFlags{
			ExtraBodyFields: map[string]any{},
		},
		Gemini: GeminiBehaviourFlags{
			SchemaUnsupportedFeatures: []string{},
		},
	}
	if !reflect.DeepEqual(q.BehaviourFlags, want) {
		t.Errorf("BehaviourFlags = %+v, want %+v", q.BehaviourFlags, want)
	}
}

// canonicalOpenAIFieldNames mirrors the canonical Chat Completions
// field surface enumerated by harness/internal/provider/openai.go's
// isCanonicalOpenAIField. The duplication is intentional: a rule's
// FieldRenames key set must be a subset of this; the source-of-truth
// is the adapter, but the test cross-checks rules without crossing
// the package boundary (the adapter's set is unexported).
//
// When the adapter learns a new canonical field, add it here too.
// TestBuiltinRulesValidate catches a rule that renames a field
// neither side knows about, so the asymmetry stays observable.
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
	for i, rule := range BuiltinRules() {
		if rule.Description == "" {
			t.Errorf("BuiltinRules()[%d]: Description is required", i)
		}
		if rule.LastVerified.IsZero() {
			t.Errorf("BuiltinRules()[%d] (%q): LastVerified is required", i, rule.Description)
		}
		if rule.Apply == nil {
			t.Errorf("BuiltinRules()[%d] (%q): Apply is required", i, rule.Description)
			continue
		}
		// Canonical-field check: materialise the rule's effect on a
		// fresh ProviderQuirks and assert every FieldRenames key
		// (the source side of the rename) is in the canonical set.
		// Rules that don't touch FieldRenames are no-ops here.
		if rule.ProviderType != "openai-compatible" {
			// The canonical-field check applies only to openai-compatible
			// rules today. The Gemini base rule (added in Step 3) sets a
			// BehaviourFlags entry rather than touching FieldRenames, so
			// there is nothing to validate against a canonical wire-field
			// list. When a Gemini or Anthropic rule does touch
			// FieldRenames, mirror the canonicalOpenAIFieldNames table
			// for that provider and extend this check rather than
			// dropping it.
			continue
		}
		q := ProviderQuirks{
			FieldRenames:   map[string]string{},
			OmitFields:     []string{},
			ValueOverrides: map[string]Value{},
			EnumCoercions:  map[string]map[string]string{},
			ReplayFields:   []string{},
			BehaviourFlags: ProviderBehaviourFlags{OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}, Gemini: GeminiBehaviourFlags{SchemaUnsupportedFeatures: []string{}}},
		}
		rule.Apply(&q)
		for key := range q.FieldRenames {
			if !canonicalOpenAIFieldNames[key] {
				t.Errorf("BuiltinRules()[%d] (%q): FieldRenames key %q is not in the canonical openai field set", i, rule.Description, key)
			}
		}
	}
}

// TestBuiltinRulesExtraBodyFieldsNoSecrets pins design risk 4:
// ExtraBodyFields values must not carry secret:// references. The
// map is serialised directly into the request body; a rule that
// accidentally stored a secret reference there would bypass
// RunConfig.Redact() entirely. This test materialises every rule's
// effect and walks the resulting ExtraBodyFields for string values
// that contain the secret:// prefix.
//
// The current Z.ai rule stores a bool (tool_stream: true) so the
// check is trivially satisfied today; the test is structural
// insurance for future rules.
//
// SchemaUnsupportedFeatures gets the same secret-scan: a future
// first-party rule that accidentally embedded a dynamic
// secret-referenced string in its unsupported-features list would
// surface that string in the lint error path (which names the
// keyword in the error message), bypassing redaction at the log
// surface. Today's entries are safe literals ("pattern", "format")
// so the check is defence-in-depth.
func TestBuiltinRulesExtraBodyFieldsNoSecrets(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := ProviderQuirks{
			FieldRenames:   map[string]string{},
			OmitFields:     []string{},
			ValueOverrides: map[string]Value{},
			EnumCoercions:  map[string]map[string]string{},
			ReplayFields:   []string{},
			BehaviourFlags: ProviderBehaviourFlags{OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}, Gemini: GeminiBehaviourFlags{SchemaUnsupportedFeatures: []string{}}},
		}
		rule.Apply(&q)
		for k, v := range q.BehaviourFlags.OpenAI.ExtraBodyFields {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(s, "secret://") {
				t.Errorf("BuiltinRules()[%d] (%q): ExtraBodyFields[%q] = %q contains a secret:// reference", i, rule.Description, k, s)
			}
		}
		for j, s := range q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures {
			if strings.Contains(s, "secret://") {
				t.Errorf("BuiltinRules()[%d] (%q): SchemaUnsupportedFeatures[%d] = %q contains a secret:// reference", i, rule.Description, j, s)
			}
		}
	}
}

// knownJSONSchemaKeywords is the allow-set
// TestBuiltinRulesSchemaUnsupportedFeaturesKeywords cross-checks every
// SchemaUnsupportedFeatures entry against. Entries here are the JSON
// Schema keywords a rule might plausibly declare as Gemini-unsupported.
// A typo in builtin.go (e.g. "patern" for "pattern") would silently
// pass the existing lint at runtime — the linter does a literal key
// match so the unsupported keyword would never fire — and this test
// is the only structural defence against that.
//
// Update when adding a new JSON Schema keyword to any rule's
// SchemaUnsupportedFeatures list. The set is intentionally broad
// (covers the OpenAI structured-outputs strict-mode rejection set as
// well as the Gemini lint's plausible surface) so a deliberate
// addition only needs one entry rather than a churn of tests.
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

// TestBuiltinRulesSchemaUnsupportedFeaturesKeywords pins that every
// string in any rule's SchemaUnsupportedFeatures list is a known JSON
// Schema keyword (per knownJSONSchemaKeywords). A typo such as "onOf"
// for "oneOf" would trivially pass all other tests — the lint walker
// only matches keys present on the schema, so an unsupported entry
// that never matches anything is silently ineffective — but this test
// catches the typo as a build-time failure rather than a runtime gap.
func TestBuiltinRulesSchemaUnsupportedFeaturesKeywords(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := ProviderQuirks{
			FieldRenames:   map[string]string{},
			OmitFields:     []string{},
			ValueOverrides: map[string]Value{},
			EnumCoercions:  map[string]map[string]string{},
			ReplayFields:   []string{},
			BehaviourFlags: ProviderBehaviourFlags{OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}, Gemini: GeminiBehaviourFlags{SchemaUnsupportedFeatures: []string{}}},
		}
		rule.Apply(&q)
		for j, kw := range q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures {
			if _, ok := knownJSONSchemaKeywords[kw]; !ok {
				t.Errorf("BuiltinRules()[%d] (%q): SchemaUnsupportedFeatures[%d] = %q is not a known JSON Schema keyword (typo? or add to knownJSONSchemaKeywords if deliberate)", i, rule.Description, j, kw)
			}
		}
	}
}

// TestRemoveFromOmit_ReservedHelper covers the helper's three
// observable states: removing an entry that is present, a no-op
// when absent, and a no-op when the slice is nil. The helper is
// kept available for future OmitFields-driven carve-outs even
// though Step 2's only carve-out toggles OmitSamplingParams
// directly. The test exists primarily so the linter does not flag
// the helper as dead code; the "ReservedHelper" suffix on the test
// name signals this to a future reader who wonders why a tested
// helper has no production caller, so they don't delete the
// function thinking it is genuinely unused.
func TestRemoveFromOmit_ReservedHelper(t *testing.T) {
	t.Run("removes present entry", func(t *testing.T) {
		q := &ProviderQuirks{OmitFields: []string{"temperature", "top_p", "logprobs"}}
		removeFromOmit(q, "top_p")
		got := strings.Join(q.OmitFields, ",")
		want := "temperature,logprobs"
		if got != want {
			t.Errorf("OmitFields = %q, want %q", got, want)
		}
	})
	t.Run("no-op when absent", func(t *testing.T) {
		q := &ProviderQuirks{OmitFields: []string{"temperature"}}
		removeFromOmit(q, "top_p")
		if len(q.OmitFields) != 1 || q.OmitFields[0] != "temperature" {
			t.Errorf("OmitFields = %v, want [temperature]", q.OmitFields)
		}
	})
	t.Run("no-op when nil slice", func(t *testing.T) {
		q := &ProviderQuirks{}
		removeFromOmit(q, "top_p")
		if len(q.OmitFields) != 0 {
			t.Errorf("OmitFields = %v, want empty", q.OmitFields)
		}
	})
}

// TestRuleCarveOuts pins the gpt-5-chat carve-out behaviour. The
// gpt-5* reasoning-class rule sets OmitSamplingParams=true; the
// gpt-5-chat* rule overrides it back to false because chat-class
// models accept sampling params. Specificity ordering (D10) is
// load-bearing: if a future edit reorders the rules or weakens the
// glob, this test fails loudly rather than silently breaking the
// gpt-5-chat-latest wire shape.
func TestRuleCarveOuts(t *testing.T) {
	t.Run("gpt-5-chat-latest keeps sampling params", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-latest")
		if q.BehaviourFlags.OpenAI.OmitSamplingParams {
			t.Errorf("gpt-5-chat-latest: OmitSamplingParams = true after carve-out; expected false")
		}
	})
	// The carve-out clears OmitSamplingParams but deliberately leaves
	// StrictMode set (the gpt-5* strict-mode rule fired earlier in the
	// resolution order). An accidental edit that adds
	// `q.BehaviourFlags.OpenAI.StrictMode = false` to the carve-out's
	// Apply would silently regress strict-mode coverage for
	// gpt-5-chat-latest; this assertion pins the composition.
	t.Run("gpt-5-chat-latest retains strict mode", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-latest")
		if !q.BehaviourFlags.OpenAI.StrictMode {
			t.Errorf("gpt-5-chat-latest: StrictMode = false after carve-out; expected true (carve-out must not clear strict mode)")
		}
	})
	t.Run("gpt-5-nano omits sampling params", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-nano")
		if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
			t.Errorf("gpt-5-nano: OmitSamplingParams = false; expected true (gpt-5* rule should fire)")
		}
	})
	t.Run("gpt-5-chat-mini also keeps sampling params", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-5-chat-mini")
		if q.BehaviourFlags.OpenAI.OmitSamplingParams {
			t.Errorf("gpt-5-chat-mini: OmitSamplingParams = true; expected false (carve-out should cover the family)")
		}
	})
	// Bare o-series aliases (o1, o3, o4) are shipped by OpenAI as
	// production model IDs alongside their dash-suffixed variants. The
	// glob "o[1-9]*" must cover both forms; a previous "o[1-9]-*"
	// dash-required form silently bypassed the rule for the bare form
	// and produced HTTP 400 responses. Each bare ID is asserted
	// individually so a regression names the specific alias.
	for _, bare := range []string{"o1", "o3", "o4"} {
		bare := bare
		t.Run("bare "+bare+" omits sampling params", func(t *testing.T) {
			q := DefaultRegistry().Resolve("openai-compatible", bare)
			if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
				t.Errorf("%s: OmitSamplingParams = false; expected true (o-series rule must cover the bare alias)", bare)
			}
		})
	}
	// Two-digit series (e.g. o10-mini) match the "o[1-9]*" glob because
	// [1-9] consumes the leading "1" and the trailing "*" consumes
	// "0-mini". This is the safer default for forward-compatibility:
	// any future o10+ alias that ships will inherit the reasoning-class
	// behaviour rather than silently regressing to greedy decoding +
	// HTTP 400. Pinned so a future tightening of the glob is a
	// deliberate edit that breaks this test rather than a silent
	// behaviour change.
	t.Run("o10-mini also omits sampling params (forward-compat)", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "o10-mini")
		if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
			t.Errorf("o10-mini: OmitSamplingParams = false; expected true (o[1-9]* should match two-digit aliases)")
		}
	})
}

// TestBuiltinRulesStrictMode exercises the strict-mode rule wiring
// through DefaultRegistry rather than a synthetic test registry. The
// OpenAI strict-mode integration tests in the provider package use
// strictRegistryFor(...) to pin the flag without depending on the
// production globs, so a glob typo in builtin.go (e.g.
// `gpt-4o-mini*` → `gpt-4o-mini-*`, which would drop the bare
// `gpt-4o-mini` from coverage) would not fail any provider-level test.
// This test closes that gap by asserting the resolved
// BehaviourFlags.OpenAI.StrictMode through DefaultRegistry for each
// model the built-in rules are documented to cover, plus pinned
// negative cases that share the same provider type but are not meant
// to enable strict mode.
//
// The positive set is derived from builtin.go's three strict-mode
// rules: `gpt-4o-mini*`, `gpt-4.1*`, and `gpt-5*` (which composes
// through the `gpt-5-chat*` carve-out without clearing StrictMode).
// The negative set guards against false positives: a future rule
// landing strict mode on a bare gpt-4o or on an o-series reasoning
// model would either be a deliberate widening (update the test) or
// a bug (the test catches it).
func TestBuiltinRulesStrictMode(t *testing.T) {
	positives := []string{
		"gpt-4o-mini",
		"gpt-4o-mini-2024-07-18",
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"gpt-5-nano",
		// gpt-5-chat-latest: carve-out clears OmitSamplingParams but
		// must NOT clear StrictMode. Redundant with the dedicated
		// sub-test in TestRuleCarveOuts; pinning both adds defence in
		// depth because the failure surfaces under each test name.
		"gpt-5-chat-latest",
	}
	for _, model := range positives {
		model := model
		t.Run(model+" enables strict mode", func(t *testing.T) {
			q := DefaultRegistry().Resolve("openai-compatible", model)
			if !q.BehaviourFlags.OpenAI.StrictMode {
				t.Errorf("%s: StrictMode = false (built-in rule did not fire)", model)
			}
		})
	}

	// Negative cases. Bare `gpt-4o` is documented by OpenAI as
	// supporting strict mode but no rule covers it today (the rule
	// glob `gpt-4o-mini*` is deliberately narrower; see
	// quirks/builtin.go for the rationale). Pin the current state so
	// a future opt-in is a deliberate edit. `o1-mini` matches the
	// reasoning-class rule but not any strict-mode rule.
	t.Run("gpt-4o does not enable strict mode", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		if q.BehaviourFlags.OpenAI.StrictMode {
			t.Errorf("gpt-4o: StrictMode = true; expected false (no built-in rule covers bare gpt-4o today)")
		}
	})
	t.Run("o1-mini omits sampling params but does not enable strict mode", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "o1-mini")
		if !q.BehaviourFlags.OpenAI.OmitSamplingParams {
			t.Errorf("o1-mini: OmitSamplingParams = false (reasoning-class rule should fire)")
		}
		if q.BehaviourFlags.OpenAI.StrictMode {
			t.Errorf("o1-mini: StrictMode = true; expected false (no rule enables strict mode on o-series)")
		}
	})
}

// TestNoMetacharsInKnownModelIDs reads the catalogue at
// testdata/model-ids.txt and asserts every entry is a literal model
// identifier — no glob metacharacters. The catalogue exists to spot-
// check that the rules in builtin.go don't accidentally glob-match
// real model names with `[`, `?`, or `*` in them; an entry with a
// metachar in it is either a typo or a model name that breaks the
// path.Match dispatch. Either case should fail the test loudly.
func TestNoMetacharsInKnownModelIDs(t *testing.T) {
	f, err := os.Open("testdata/model-ids.txt")
	if err != nil {
		t.Fatalf("open model-ids.txt: %v", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.ContainsAny(line, "*?[]") {
			t.Errorf("testdata/model-ids.txt:%d: model id %q contains a glob metachar", lineNo, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan model-ids.txt: %v", err)
	}
}

// rulesMatchAtLeastOne sanity-checks that the catalogue and the rule
// set agree: every model id in testdata/model-ids.txt that matches a
// builtin rule resolves to a non-zero ProviderQuirks state. This is
// a forward-compatible smoke test for the catalogue, not a contract
// — entries that match no rule (e.g. gpt-4o, claude-3-*) are
// expected and produce a zero-value ProviderQuirks.
//
// Not promoted to a load-bearing assertion because the catalogue's
// purpose is the metachar guard above; the resolution count is
// informational only and surfaces via t.Logf rather than t.Errorf.
func TestKnownModelIDsResolutionSmoke(t *testing.T) {
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
// for the openai-compatible provider, even when the model string
// happens to look like a Gemini model id (e.g. "gemini-2.5-pro"
// routed through a third-party gateway that exposes Vertex models
// behind an OpenAI-compatible facade).
//
// Today the isolation is structurally guaranteed because RuleMatches
// compares ProviderType exactly and the Gemini base rule's writes
// land on BehaviourFlags.Gemini, never BehaviourFlags.OpenAI. The
// test is defence-in-depth: a future code path that loosens
// provider-type matching (e.g. a wildcard ProviderType, or a rule
// that writes across sub-structs) would fail this assertion rather
// than silently leak Gemini behaviour into an openai-compatible
// request.
func TestGeminiRule_NoMatchOnOpenAICompatProvider(t *testing.T) {
	q := DefaultRegistry().Resolve("openai-compatible", "gemini-2.5-pro")
	// The Gemini sub-struct must remain at its zero value: the base
	// rule did not fire because the provider type does not match.
	// Asserting against the zero value (rather than just inspecting
	// whether the rule's writes landed) catches a future
	// regression where a Gemini rule writes to the Gemini sub-struct
	// on an openai-compatible resolution by accident.
	if q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape != StreamArgsOff {
		t.Errorf("openai-compatible/gemini-2.5-pro: BehaviourFlags.Gemini.StreamFunctionCallArgsShape = %v, want StreamArgsOff (zero value); Gemini rule must not fire on a non-gemini provider", q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape)
	}
	// And the openai-compatible defaults are unaffected.
	if q.BehaviourFlags.OpenAI.OmitSamplingParams {
		t.Errorf("openai-compatible/gemini-2.5-pro: OpenAI.OmitSamplingParams = true; no rule should have fired")
	}
	if q.BehaviourFlags.OpenAI.TokenField != TokenFieldMaxCompletionTokens {
		t.Errorf("openai-compatible/gemini-2.5-pro: OpenAI.TokenField = %v, want TokenFieldMaxCompletionTokens (zero default)", q.BehaviourFlags.OpenAI.TokenField)
	}
}

// TestToolChoiceCapabilityRules pins that each first-party provider's
// base rule advertises the tool-choice modes its API supports, that the
// capability lands on the top-level ProviderQuirks.ToolChoice field, and
// that a provider with no rule resolves the zero value (no support). The
// Anthropic-specific assertion guards the one asymmetry: Anthropic has no
// native "none", so None must stay false while the cross-provider modes
// are true.
func TestToolChoiceCapabilityRules(t *testing.T) {
	t.Run("anthropic advertises auto/required/named but not none", func(t *testing.T) {
		q := DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
		if !q.ToolChoice.Supported {
			t.Fatalf("anthropic: ToolChoice.Supported = false, want true")
		}
		if !q.ToolChoice.Auto || !q.ToolChoice.Required || !q.ToolChoice.NamedTool {
			t.Errorf("anthropic: ToolChoice = %+v, want Auto/Required/NamedTool all true", q.ToolChoice)
		}
		if q.ToolChoice.None {
			t.Errorf("anthropic: ToolChoice.None = true, want false (no native none mode)")
		}
	})

	t.Run("openai-compatible advertises every mode", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		want := ToolChoiceCapability{Supported: true, Auto: true, Required: true, None: true, NamedTool: true}
		if q.ToolChoice != want {
			t.Errorf("openai-compatible: ToolChoice = %+v, want %+v", q.ToolChoice, want)
		}
	})

	t.Run("gemini advertises every mode", func(t *testing.T) {
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		want := ToolChoiceCapability{Supported: true, Auto: true, Required: true, None: true, NamedTool: true}
		if q.ToolChoice != want {
			t.Errorf("gemini: ToolChoice = %+v, want %+v", q.ToolChoice, want)
		}
	})

	t.Run("unknown provider resolves zero (no support)", func(t *testing.T) {
		q := DefaultRegistry().Resolve("mystery-provider", "some-model")
		if q.ToolChoice != (ToolChoiceCapability{}) {
			t.Errorf("unknown provider: ToolChoice = %+v, want zero value (no native support)", q.ToolChoice)
		}
	})
}

// TestToolChoiceRulesSetSupportedWhenAnyMode pins the structural
// relationship the adapters depend on: any first-party rule that turns on
// a per-mode tool-choice bool must also set Supported. An adapter checks
// Supported as the master gate, so a rule that set Required without
// Supported would silently disable the feature it intended to enable.
func TestToolChoiceRulesSetSupportedWhenAnyMode(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := freshQuirks()
		rule.Apply(&q)
		tc := q.ToolChoice
		anyMode := tc.Auto || tc.Required || tc.None || tc.NamedTool
		if anyMode && !tc.Supported {
			t.Errorf("BuiltinRules()[%d] (%q): a tool-choice mode is set but Supported is false", i, rule.Description)
		}
	}
}

// TestStructuredToolResultCapabilityRules pins that each first-party
// provider's base rule advertises the structured tool-result wire shape its
// API actually accepts, that the capability lands on the top-level
// ProviderQuirks.StructuredToolResults field, and that a provider with no
// rule resolves the zero value (text-only). OpenAI is the load-bearing
// negative: its tool messages are plain strings on the wire, so it must NOT
// gain the capability — the no-regression guarantee for the OpenAI adapters.
func TestStructuredToolResultCapabilityRules(t *testing.T) {
	t.Run("anthropic advertises content-block array", func(t *testing.T) {
		q := DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
		want := StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
		if q.StructuredToolResults != want {
			t.Errorf("anthropic: StructuredToolResults = %+v, want %+v", q.StructuredToolResults, want)
		}
	})

	t.Run("gemini advertises object response", func(t *testing.T) {
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		want := StructuredToolResultCapability{Supported: true, ObjectResponse: true}
		if q.StructuredToolResults != want {
			t.Errorf("gemini: StructuredToolResults = %+v, want %+v", q.StructuredToolResults, want)
		}
	})

	t.Run("openai-compatible stays text-only", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
		if q.StructuredToolResults != (StructuredToolResultCapability{}) {
			t.Errorf("openai-compatible: StructuredToolResults = %+v, want zero value (text-only)", q.StructuredToolResults)
		}
	})

	t.Run("bedrock stays text-only", func(t *testing.T) {
		// builtin.go names bedrock as the load-bearing negative: a provider
		// with no rule stays text-only by construction. Pin it so a future
		// bedrock rule flipping Supported=true cannot land unnoticed.
		q := DefaultRegistry().Resolve("bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0")
		if q.StructuredToolResults != (StructuredToolResultCapability{}) {
			t.Errorf("bedrock: StructuredToolResults = %+v, want zero value (text-only)", q.StructuredToolResults)
		}
	})

	t.Run("unknown provider resolves zero (text-only)", func(t *testing.T) {
		q := DefaultRegistry().Resolve("mystery-provider", "some-model")
		if q.StructuredToolResults != (StructuredToolResultCapability{}) {
			t.Errorf("unknown provider: StructuredToolResults = %+v, want zero value (text-only)", q.StructuredToolResults)
		}
	})
}

// TestStructuredToolResultRulesSetSupportedWhenAnyShape pins the structural
// relationship the adapters depend on: any first-party rule that turns on a
// structured wire-shape bool must also set Supported. An adapter checks
// Supported as the master gate, so a rule that set ObjectResponse without
// Supported would silently keep the provider text-only.
func TestStructuredToolResultRulesSetSupportedWhenAnyShape(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := freshQuirks()
		rule.Apply(&q)
		sr := q.StructuredToolResults
		anyShape := sr.ObjectResponse || sr.ContentBlockArray
		if anyShape && !sr.Supported {
			t.Errorf("BuiltinRules()[%d] (%q): a structured-result shape is set but Supported is false", i, rule.Description)
		}
	}
}

// TestParallelToolCallsCapabilityRules pins which providers advertise a
// native parallel-tool-call control (#222). Gemini and Bedrock are the
// load-bearing negatives: builtin.go names them as deliberately absent, so a
// future rule flipping Supported=true cannot land unnoticed.
func TestParallelToolCallsCapabilityRules(t *testing.T) {
	supported := map[string]string{
		"anthropic":         "claude-sonnet-4-5",
		"openai-compatible": "gpt-4o",
		"openai-responses":  "gpt-4o",
	}
	for provider, model := range supported {
		t.Run(provider+" advertises disable", func(t *testing.T) {
			q := DefaultRegistry().Resolve(provider, model)
			want := ParallelToolCallsCapability{Supported: true, Disable: true}
			if q.ParallelToolCalls != want {
				t.Errorf("%s: ParallelToolCalls = %+v, want %+v", provider, q.ParallelToolCalls, want)
			}
		})
	}

	unsupported := map[string]string{
		"gemini":         "gemini-2.5-pro",
		"bedrock":        "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"mystery-vendor": "some-model",
	}
	for provider, model := range unsupported {
		t.Run(provider+" stays unsupported", func(t *testing.T) {
			q := DefaultRegistry().Resolve(provider, model)
			if q.ParallelToolCalls != (ParallelToolCallsCapability{}) {
				t.Errorf("%s: ParallelToolCalls = %+v, want zero value (unsupported)", provider, q.ParallelToolCalls)
			}
		})
	}
}

// TestToolExamplesCapabilityRules pins which providers accept the JSON-Schema
// `examples` keyword in a tool's parameters (#222). Gemini is the load-bearing
// negative: its Schema dialect rejects `examples`, so the example must reach
// the model via the description text instead — never folded into the schema.
func TestToolExamplesCapabilityRules(t *testing.T) {
	supported := map[string]string{
		"anthropic":         "claude-sonnet-4-5",
		"openai-compatible": "gpt-4o",
		"openai-responses":  "gpt-4o",
	}
	for provider, model := range supported {
		t.Run(provider+" accepts schema examples", func(t *testing.T) {
			q := DefaultRegistry().Resolve(provider, model)
			want := ToolExamplesCapability{Supported: true}
			if q.ToolExamples != want {
				t.Errorf("%s: ToolExamples = %+v, want %+v", provider, q.ToolExamples, want)
			}
		})
	}

	unsupported := map[string]string{
		"gemini":         "gemini-2.5-pro",
		"bedrock":        "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"mystery-vendor": "some-model",
	}
	for provider, model := range unsupported {
		t.Run(provider+" stays unsupported", func(t *testing.T) {
			q := DefaultRegistry().Resolve(provider, model)
			if q.ToolExamples != (ToolExamplesCapability{}) {
				t.Errorf("%s: ToolExamples = %+v, want zero value (unsupported)", provider, q.ToolExamples)
			}
		})
	}
}

// TestOpenAIResponsesBehaviourFlags pins the Responses-specific wire
// divergences the builtin "openai-responses / *" rule resolves (#332). The
// resolved ProviderQuirks is the single source of truth for the Responses
// send path (the Codec invariant), so the adapter reads these flags rather
// than hard-coding the divergences. Each value is the zero value of its enum
// — the rule pins them explicitly so a future model-scoped rule has somewhere
// to override and a dropped pin is caught here.
func TestOpenAIResponsesBehaviourFlags(t *testing.T) {
	q := DefaultRegistry().Resolve("openai-responses", "gpt-4o")
	rf := q.BehaviourFlags.OpenAIResponses
	if rf.TokenField != TokenFieldMaxOutputTokens {
		t.Errorf("TokenField = %v, want TokenFieldMaxOutputTokens", rf.TokenField)
	}
	if rf.StoreMode != StoreFalse {
		t.Errorf("StoreMode = %v, want StoreFalse", rf.StoreMode)
	}
	if rf.InputItemShape != TypedInputItems {
		t.Errorf("InputItemShape = %v, want TypedInputItems", rf.InputItemShape)
	}

	// A provider with no rule resolves the same zero-value flags, so the
	// adapter falls through to today's byte-identical behaviour even when
	// the registry is empty for the (provider, model) pair.
	empty := NewRegistry(nil).Resolve("openai-responses", "gpt-4o")
	if empty.BehaviourFlags.OpenAIResponses != (OpenAIResponsesBehaviourFlags{}) {
		t.Errorf("empty registry: OpenAIResponses = %+v, want zero value", empty.BehaviourFlags.OpenAIResponses)
	}
}

// TestParallelToolCallsRulesSetSupportedWhenDisable pins the structural
// relationship the adapters depend on: any rule that turns on the Disable bool
// must also set Supported. An adapter checks Supported as the master gate, so
// a rule that set Disable without Supported would silently no-op.
func TestParallelToolCallsRulesSetSupportedWhenDisable(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := freshQuirks()
		rule.Apply(&q)
		if q.ParallelToolCalls.Disable && !q.ParallelToolCalls.Supported {
			t.Errorf("BuiltinRules()[%d] (%q): Disable is set but Supported is false", i, rule.Description)
		}
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

// TestReplayFieldsRules_DeepSeekReasoner pins the DeepSeek-reasoner
// rule fires and populates ReplayFields with the documented path.
// The defensive isolation cases assert the rule does NOT fire for
// other DeepSeek-family models (e.g. deepseek-v3) and does NOT fire
// when the same model name is routed through a non-openai-compatible
// provider.
func TestReplayFieldsRules_DeepSeekReasoner(t *testing.T) {
	t.Run("fires on deepseek-reasoner", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-reasoner")
		if len(q.ReplayFields) != 1 || q.ReplayFields[0] != "reasoning_content" {
			t.Errorf("ReplayFields = %v, want [reasoning_content]", q.ReplayFields)
		}
	})
	t.Run("fires on deepseek-reasoner-lite (suffix variant)", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-reasoner-lite")
		if len(q.ReplayFields) != 1 || q.ReplayFields[0] != "reasoning_content" {
			t.Errorf("ReplayFields = %v, want [reasoning_content]", q.ReplayFields)
		}
	})
	t.Run("does not fire on unrelated openai-compatible models", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-chat")
		if len(q.ReplayFields) != 0 {
			t.Errorf("deepseek-chat must not fire deepseek-reasoner rule; got ReplayFields=%v", q.ReplayFields)
		}
	})
	t.Run("does not fire when routed through a different provider", func(t *testing.T) {
		q := DefaultRegistry().Resolve("gemini", "deepseek-reasoner")
		for _, p := range q.ReplayFields {
			if p == "reasoning_content" {
				t.Errorf("DeepSeek rule leaked into Gemini provider resolution: %v", q.ReplayFields)
			}
		}
	})
}

// TestReplayFieldsRules_DeepSeekV4 mirrors DeepSeekReasoner for the
// v4 family rule. Same shape, different glob.
func TestReplayFieldsRules_DeepSeekV4(t *testing.T) {
	t.Run("fires on deepseek-v4", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v4")
		if len(q.ReplayFields) != 1 || q.ReplayFields[0] != "reasoning_content" {
			t.Errorf("ReplayFields = %v, want [reasoning_content]", q.ReplayFields)
		}
	})
	t.Run("fires on deepseek-v4-chat", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v4-chat")
		if len(q.ReplayFields) != 1 || q.ReplayFields[0] != "reasoning_content" {
			t.Errorf("ReplayFields = %v, want [reasoning_content]", q.ReplayFields)
		}
	})
	t.Run("does not fire on deepseek-v3 (no rule)", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "deepseek-v3")
		if len(q.ReplayFields) != 0 {
			t.Errorf("deepseek-v3 must not fire deepseek-v4 rule; got ReplayFields=%v", q.ReplayFields)
		}
	})
}

// TestReplayFieldsRules_Gemini3 pins the gemini-3* ReplayFields rule:
// fires on gemini-3* model ids, captures the sibling-of-functionCall
// thoughtSignature path, and does not fire on 2.x or on
// openai-compatible resolutions that happen to use a Gemini-like model
// name.
func TestReplayFieldsRules_Gemini3(t *testing.T) {
	t.Run("fires on gemini-3.1-pro-preview", func(t *testing.T) {
		q := DefaultRegistry().Resolve("gemini", "gemini-3.1-pro-preview")
		wantPath := "candidates[].content.parts[].thoughtSignature"
		found := false
		for _, p := range q.ReplayFields {
			if p == wantPath {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Gemini 3 rule did not register %q; got ReplayFields=%v", wantPath, q.ReplayFields)
		}
	})
	t.Run("does not fire on gemini-2.5-pro", func(t *testing.T) {
		q := DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
		if len(q.ReplayFields) != 0 {
			t.Errorf("gemini-2.5-pro must not fire gemini-3* rule; got ReplayFields=%v", q.ReplayFields)
		}
	})
	t.Run("does not fire when routed through openai-compatible", func(t *testing.T) {
		q := DefaultRegistry().Resolve("openai-compatible", "gemini-3.1-pro-preview")
		if len(q.ReplayFields) != 0 {
			t.Errorf("Gemini 3 rule leaked into openai-compatible resolution: %v", q.ReplayFields)
		}
	})
}

// TestRuleStaleness logs (does not fail) any rule whose LastVerified
// is more than 180 days behind today. Per design §2.3 staleness is a
// signal, not an error — re-verification is the response, not breaking
// CI. The harness ships with the test in place from Step 1 so Step 2's
// first rule additions land against a live warning surface.
func TestRuleStaleness(t *testing.T) {
	cutoff := time.Now().Add(-staleness)
	for _, rule := range BuiltinRules() {
		if rule.LastVerified.Before(cutoff) {
			t.Logf("STALE: rule %q last verified %s (>180d ago); re-verify against the upstream wire shape", rule.Description, rule.LastVerified.Format("2006-01-02"))
		}
	}
}

// TestRegistryComposition pins the specificity-then-declaration-order
// composition rule from design D10: the longer ModelMatch glob wins
// when two rules touch the same field. The wildcard rule sets a
// baseline; the more specific glob overrides it for matching models
// only, leaving non-matching models with the baseline alone.
func TestRegistryComposition(t *testing.T) {
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

	// gpt-5-nano: both rules match; the more specific one runs second
	// and wins.
	got := reg.Resolve("openai-compatible", "gpt-5-nano")
	if got.BehaviourFlags.OpenAI.TokenField != TokenFieldMaxCompletionTokens {
		t.Errorf("gpt-5-nano: TokenField = %v, want %v (specific override)", got.BehaviourFlags.OpenAI.TokenField, TokenFieldMaxCompletionTokens)
	}

	// gpt-4o: only the wildcard matches.
	got = reg.Resolve("openai-compatible", "gpt-4o")
	if got.BehaviourFlags.OpenAI.TokenField != TokenFieldMaxTokens {
		t.Errorf("gpt-4o: TokenField = %v, want %v (wildcard only)", got.BehaviourFlags.OpenAI.TokenField, TokenFieldMaxTokens)
	}

	// Different provider type: neither rule matches; zero-value remains.
	got = reg.Resolve("anthropic", "gpt-5-nano")
	if got.BehaviourFlags.OpenAI.TokenField != TokenFieldMaxCompletionTokens {
		t.Errorf("anthropic/gpt-5-nano: TokenField = %v, want zero default", got.BehaviourFlags.OpenAI.TokenField)
	}
}

// TestValueConstructors round-trips each New*Value constructor and
// asserts exactly one field is set on the resulting Value.
func TestValueConstructors(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		v := NewStringValue("hello")
		if v.String == nil || *v.String != "hello" {
			t.Errorf("String not set or wrong value: %+v", v)
		}
		if v.Int != nil || v.Float != nil || v.Bool != nil || v.Null {
			t.Errorf("other fields must be unset: %+v", v)
		}
	})
	t.Run("int", func(t *testing.T) {
		v := NewIntValue(42)
		if v.Int == nil || *v.Int != 42 {
			t.Errorf("Int not set or wrong value: %+v", v)
		}
		if v.String != nil || v.Float != nil || v.Bool != nil || v.Null {
			t.Errorf("other fields must be unset: %+v", v)
		}
	})
	t.Run("float", func(t *testing.T) {
		v := NewFloatValue(3.14)
		if v.Float == nil || *v.Float != 3.14 {
			t.Errorf("Float not set or wrong value: %+v", v)
		}
		if v.String != nil || v.Int != nil || v.Bool != nil || v.Null {
			t.Errorf("other fields must be unset: %+v", v)
		}
	})
	t.Run("bool", func(t *testing.T) {
		v := NewBoolValue(true)
		if v.Bool == nil || *v.Bool != true {
			t.Errorf("Bool not set or wrong value: %+v", v)
		}
		if v.String != nil || v.Int != nil || v.Float != nil || v.Null {
			t.Errorf("other fields must be unset: %+v", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		v := NewNullValue()
		if !v.Null {
			t.Errorf("Null must be true: %+v", v)
		}
		if v.String != nil || v.Int != nil || v.Float != nil || v.Bool != nil {
			t.Errorf("other fields must be unset: %+v", v)
		}
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

// TestRuleMatches pins the exported predicate the CLI shares with
// Resolve. Covers exact-match, empty-ModelMatch wildcard, glob
// success, glob non-match, ProviderType mismatch, and malformed glob
// (returns false rather than panicking).
func TestRuleMatches(t *testing.T) {
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
			got := RuleMatches(tc.rule, tc.providerType, tc.model)
			if got != tc.want {
				t.Errorf("RuleMatches(%+v, %q, %q) = %v, want %v", tc.rule, tc.providerType, tc.model, got, tc.want)
			}
		})
	}
}

// TestOpenAITokenFieldMarshalJSON pins the human-readable wire form
// of each named constant and round-trips through UnmarshalJSON so the
// CLI output can be parsed back by a consumer that decodes into the
// same struct.
func TestOpenAITokenFieldMarshalJSON(t *testing.T) {
	cases := []struct {
		val  OpenAITokenField
		want string
	}{
		{TokenFieldMaxCompletionTokens, `"max_completion_tokens"`},
		{TokenFieldMaxTokens, `"max_tokens"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%v) = %s, want %s", tc.val, got, tc.want)
			}
			var round OpenAITokenField
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		// An out-of-range value must render as "unknown(N)" rather than
		// panicking so a forward-compatible reader still gets parseable
		// JSON from a newer harness binary.
		got, err := json.Marshal(OpenAITokenField(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		// Unknown strings reject; silent acceptance would defeat the
		// point of the named constants.
		var f OpenAITokenField
		if err := json.Unmarshal([]byte(`"bogus"`), &f); err == nil {
			t.Error("Unmarshal of unknown string must return an error")
		}
	})
}

// TestGeminiStreamArgsShapeMarshalJSON mirrors
// TestOpenAITokenFieldMarshalJSON for the Gemini enum.
func TestGeminiStreamArgsShapeMarshalJSON(t *testing.T) {
	cases := []struct {
		val  GeminiStreamArgsShape
		want string
	}{
		{StreamArgsOff, `"off"`},
		{StreamArgsV2Snapshot, `"v2_snapshot"`},
		{StreamArgsV3Deltas, `"v3_deltas"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%v) = %s, want %s", tc.val, got, tc.want)
			}
			var round GeminiStreamArgsShape
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		got, err := json.Marshal(GeminiStreamArgsShape(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		var s GeminiStreamArgsShape
		if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
			t.Error("Unmarshal of unknown string must return an error")
		}
	})
}

// TestOpenAIResponsesTokenFieldMarshalJSON mirrors
// TestOpenAITokenFieldMarshalJSON for the Responses token-field enum,
// locking the wire-key string against a future typo the rest of the
// suite would not catch.
func TestOpenAIResponsesTokenFieldMarshalJSON(t *testing.T) {
	cases := []struct {
		val  OpenAIResponsesTokenField
		want string
	}{
		{TokenFieldMaxOutputTokens, `"max_output_tokens"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%v) = %s, want %s", tc.val, got, tc.want)
			}
			var round OpenAIResponsesTokenField
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		got, err := json.Marshal(OpenAIResponsesTokenField(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		var f OpenAIResponsesTokenField
		if err := json.Unmarshal([]byte(`"bogus"`), &f); err == nil {
			t.Error("Unmarshal of unknown string must return an error")
		}
	})
}

// TestOpenAIResponsesStoreModeMarshalJSON mirrors the pattern for the
// Responses store-mode enum.
func TestOpenAIResponsesStoreModeMarshalJSON(t *testing.T) {
	cases := []struct {
		val  OpenAIResponsesStoreMode
		want string
	}{
		{StoreFalse, `"store_false"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%v) = %s, want %s", tc.val, got, tc.want)
			}
			var round OpenAIResponsesStoreMode
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		got, err := json.Marshal(OpenAIResponsesStoreMode(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		var s OpenAIResponsesStoreMode
		if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
			t.Error("Unmarshal of unknown string must return an error")
		}
	})
}

// TestOpenAIResponsesInputShapeMarshalJSON mirrors the pattern for the
// Responses input-item shape enum.
func TestOpenAIResponsesInputShapeMarshalJSON(t *testing.T) {
	cases := []struct {
		val  OpenAIResponsesInputShape
		want string
	}{
		{TypedInputItems, `"typed_input_items"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%v) = %s, want %s", tc.val, got, tc.want)
			}
			var round OpenAIResponsesInputShape
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		got, err := json.Marshal(OpenAIResponsesInputShape(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		var s OpenAIResponsesInputShape
		if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
			t.Error("Unmarshal of unknown string must return an error")
		}
	})
}

// TestValueJSONTags pins the camelCase JSON keys + omitempty on the
// Value struct. Operators read CLI output as JSON; the shape needs to
// stay stable across the empty-rule-set baseline and the first Step 2
// rule that populates ValueOverrides.
func TestValueJSONTags(t *testing.T) {
	cases := []struct {
		name string
		val  Value
		want string
	}{
		{"string", NewStringValue("foo"), `{"string":"foo"}`},
		{"int", NewIntValue(42), `{"int":42}`},
		{"float", NewFloatValue(3.14), `{"float":3.14}`},
		{"bool-true", NewBoolValue(true), `{"bool":true}`},
		{"bool-false", NewBoolValue(false), `{"bool":false}`},
		{"null", NewNullValue(), `{"null":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%+v) = %s, want %s", tc.val, got, tc.want)
			}
		})
	}
}
