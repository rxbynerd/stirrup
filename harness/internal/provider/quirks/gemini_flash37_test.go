package quirks

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestGeminiFlashRules_Resolution pins the model gating of the 3.6/3.7
// rules. The families whose API generation dropped role:"function" are
// exactly the ones that resolve to ToolResultRoleUser; everything older
// keeps the zero value, which is the historical role. Both rules carry the
// per-model thinkingLevel allow-list, and 3.7 is the narrower of the two
// because it rejects "minimal".
func TestGeminiFlashRules_Resolution(t *testing.T) {
	cases := []struct {
		model      string
		wantRole   GeminiToolResultRole
		wantOmit   bool
		wantLevels []string
	}{
		{"gemini-2.5-pro", ToolResultRoleFunction, false, []string{}},
		{"gemini-3.1-pro-preview", ToolResultRoleFunction, false, []string{}},
		{"gemini-3.5-flash", ToolResultRoleFunction, false, []string{}},
		{"gemini-3.6-flash", ToolResultRoleUser, true, []string{"minimal", "low", "medium", "high"}},
		{"gemini-3.6-pro", ToolResultRoleUser, true, []string{"minimal", "low", "medium", "high"}},
		{"gemini-3.7-flash", ToolResultRoleUser, true, []string{"low", "medium", "high"}},
		{"gemini-3.7-flash-preview-08-01", ToolResultRoleUser, true, []string{"low", "medium", "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			q := DefaultRegistry().Resolve("gemini", tc.model)
			g := q.BehaviourFlags.Gemini
			if g.ToolResultRole != tc.wantRole {
				t.Errorf("ToolResultRole = %v, want %v", g.ToolResultRole, tc.wantRole)
			}
			if g.OmitSamplingParams != tc.wantOmit {
				t.Errorf("OmitSamplingParams = %v, want %v", g.OmitSamplingParams, tc.wantOmit)
			}
			if !reflect.DeepEqual(g.ThinkingLevels, tc.wantLevels) {
				t.Errorf("ThinkingLevels = %v, want %v", g.ThinkingLevels, tc.wantLevels)
			}
		})
	}
}

// TestGeminiFlashRules_DoNotLeakAcrossProviders guards the same
// cross-provider isolation the Gemini 3 replay rule has: a model id that
// merely looks like a Gemini one must not pick up Gemini wire behaviour
// when it is routed through an OpenAI-compatible gateway, where the
// concepts do not exist.
func TestGeminiFlashRules_DoNotLeakAcrossProviders(t *testing.T) {
	for _, pt := range []string{"openai-compatible", "anthropic", "openai-responses"} {
		t.Run(pt, func(t *testing.T) {
			g := DefaultRegistry().Resolve(pt, "gemini-3.7-flash").BehaviourFlags.Gemini
			if g.ToolResultRole != ToolResultRoleFunction || g.OmitSamplingParams || len(g.ThinkingLevels) != 0 {
				t.Errorf("Gemini flags leaked into %s resolution: %+v", pt, g)
			}
		})
	}
}

// TestGeminiToolResultRoleMarshalJSON locks the introspection strings to
// the literal wire roles: the CLI prints them, and an operator debugging
// a 400 compares them against the API's error message verbatim.
func TestGeminiToolResultRoleMarshalJSON(t *testing.T) {
	cases := []struct {
		val  GeminiToolResultRole
		want string
	}{
		{ToolResultRoleFunction, `"function"`},
		{ToolResultRoleUser, `"user"`},
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
			var round GeminiToolResultRole
			if err := json.Unmarshal(got, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round != tc.val {
				t.Errorf("round-trip: got %v, want %v", round, tc.val)
			}
			if wire := tc.val.WireRole(); `"`+wire+`"` != tc.want {
				t.Errorf("WireRole() = %q, inconsistent with marshalled %s", wire, tc.want)
			}
		})
	}
	t.Run("unknown-marshal", func(t *testing.T) {
		got, err := json.Marshal(GeminiToolResultRole(99))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != `"unknown(99)"` {
			t.Errorf("Marshal(99) = %s, want %q", got, `"unknown(99)"`)
		}
	})
	t.Run("unknown-unmarshal", func(t *testing.T) {
		var r GeminiToolResultRole
		if err := json.Unmarshal([]byte(`"model"`), &r); err == nil {
			t.Error("Unmarshal of a role outside the enum must return an error")
		}
	})
}
