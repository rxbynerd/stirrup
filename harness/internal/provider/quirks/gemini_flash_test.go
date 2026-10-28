package quirks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGeminiFlashRules_Resolution pins the model gating of the 3.6+
// rules. The families whose API generation dropped role:"function" are
// exactly the ones that resolve to ToolResultRoleUser; everything older
// keeps the zero value, which is the historical role. Each rule carries the
// per-model thinkingLevel allow-list, and 3.7 onward is narrower than 3.6
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
		{"gemini-3.8-flash", ToolResultRoleUser, true, []string{"low", "medium", "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			must := require.New(t)
			g := DefaultRegistry().Resolve(Target{ProviderType: "gemini", Model: tc.model}).BehaviourFlags.Gemini
			must.Equal(tc.wantRole, g.ToolResultRole)
			must.Equal(tc.wantOmit, g.OmitSamplingParams)
			must.Equal(tc.wantLevels, g.ThinkingLevels)
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
			must := require.New(t)
			g := DefaultRegistry().Resolve(Target{ProviderType: pt, Model: "gemini-3.8-flash"}).BehaviourFlags.Gemini
			must.Equal(ToolResultRoleFunction, g.ToolResultRole)
			must.False(g.OmitSamplingParams)
			must.Empty(g.ThinkingLevels)
		})
	}
}

// TestGeminiToolResultRole_MarshalJSON_MatchesWireRoles locks the introspection
// strings to the literal wire roles: the CLI prints them, and an operator
// debugging a 400 compares them against the API's error message verbatim.
func TestGeminiToolResultRole_MarshalJSON_MatchesWireRoles(t *testing.T) {
	cases := []struct {
		name string
		val  GeminiToolResultRole
		want string
	}{
		{"function role", ToolResultRoleFunction, `"function"`},
		{"user role", ToolResultRoleUser, `"user"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := json.Marshal(tc.val)
			must.NoError(err)
			must.Equal(tc.want, string(got))

			var round GeminiToolResultRole
			must.NoError(json.Unmarshal(got, &round))
			must.Equal(tc.val, round)

			wire := tc.val.WireRole()
			must.Equal(tc.want, `"`+wire+`"`)
		})
	}
	t.Run("marshals unknown value as unknown(n)", func(t *testing.T) {
		must := require.New(t)
		got, err := json.Marshal(GeminiToolResultRole(99))
		must.NoError(err)
		must.Equal(`"unknown(99)"`, string(got))
	})
	t.Run("rejects role outside enum", func(t *testing.T) {
		must := require.New(t)
		var r GeminiToolResultRole
		must.Error(json.Unmarshal([]byte(`"model"`), &r))
	})
}
