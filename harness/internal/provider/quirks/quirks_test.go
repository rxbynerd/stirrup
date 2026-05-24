package quirks

import (
	"encoding/json"
	"reflect"
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
	}
	if !reflect.DeepEqual(q.BehaviourFlags, want) {
		t.Errorf("BehaviourFlags = %+v, want %+v", q.BehaviourFlags, want)
	}
}

// TestBuiltinRulesValidate asserts every rule baked into the registry
// passes a structural validity check: required metadata is populated
// (Description, LastVerified, Apply) so trace attributes and the CLI
// introspection surface have something to report.
//
// Step 1 ships an empty rule set so this loop runs zero iterations;
// the test still validates that BuiltinRules returns a usable value
// (the for-range over the slice is the assertion).
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
		}
	}
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
