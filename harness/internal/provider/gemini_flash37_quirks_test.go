package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// geminiToolRoundTripParams returns StreamParams carrying a completed
// tool exchange (user turn → assistant functionCall → tool result), the
// shape that distinguishes a 3.6+ resolution from an earlier one: the
// tool result's contents[].role is the only field the ToolResultRole
// quirk touches.
func geminiToolRoundTripParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read the file"}}},
			{Role: "assistant", Content: []types.ContentBlock{{
				Type:             "tool_use",
				ID:               "gemini-0-0",
				Name:             "read_file",
				Input:            json.RawMessage(`{"path":"main.go"}`),
				ThoughtSignature: "sig-abc",
			}}},
			{Role: "user", Content: []types.ContentBlock{{
				Type:      "tool_result",
				ToolUseID: "gemini-0-0",
				Content:   "package main",
			}}},
		},
		Tools: []types.ToolDefinition{
			{
				Name:        "read_file",
				Description: "read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
	}
}

// geminiContentRoles extracts the contents[].role sequence from a
// marshalled request body.
func geminiContentRoles(t *testing.T, body []byte) []string {
	t.Helper()
	must := require.New(t)
	var req struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
	}
	must.NoError(json.Unmarshal(body, &req), "unmarshal body: %s", body)
	roles := make([]string, 0, len(req.Contents))
	for _, c := range req.Contents {
		roles = append(roles, c.Role)
	}
	return roles
}

// TestGeminiQuirks_ToolResultRole_ByFamily pins the model-gated role
// switch: the 3.6/3.7 families place a functionResponse on role:"user"
// while every earlier family keeps the historical role.
//
// The boundary comes from the AI Studio surface, which rejects
// role:"function" from 3.6 onward. Vertex, which this adapter calls,
// accepts either role for every family, so a regression in the flag
// fails no live Vertex run; this test is the guard.
func TestGeminiQuirks_ToolResultRole_ByFamily(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"gemini-2.5-pro", "function"},
		{"gemini-3.1-pro-preview", "function"},
		{"gemini-3.5-flash", "function"},
		{"gemini-3.6-flash", "user"},
		{"gemini-3.7-flash", "user"},
		{"gemini-3.7-pro", "user"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			must := require.New(t)
			params := geminiToolRoundTripParams(tc.model)
			q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: tc.model})
			body, _, err := BuildGenerateContentRequest(params, nil, q)
			must.NoError(err)
			roles := geminiContentRoles(t, body)
			must.Len(roles, 3, "contents roles %v: want user text, model turn, tool result", roles)
			must.Equal(tc.want, roles[2], "tool-result role (body: %s)", body)
		})
	}
}

// TestGeminiQuirks_OmitSamplingParams_ByFamily pins that the families
// which deprecate the sampling knobs send no temperature, while earlier
// families still do. The API ignores a deprecated temperature rather than
// rejecting it, so no live request fails if one is sent; only this body
// check catches a regression.
func TestGeminiQuirks_OmitSamplingParams_ByFamily(t *testing.T) {
	cases := []struct {
		model           string
		wantTemperature bool
	}{
		{"gemini-2.5-pro", true},
		{"gemini-3.1-pro-preview", true},
		{"gemini-3.6-flash", false},
		{"gemini-3.7-flash", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			must := require.New(t)
			params := geminiQuirksCanonicalParams(tc.model)
			q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: tc.model})
			body, _, err := BuildGenerateContentRequest(params, nil, q)
			must.NoError(err)
			if tc.wantTemperature {
				must.Contains(string(body), `"temperature"`, "body: %s", body)
			} else {
				must.NotContains(string(body), `"temperature"`, "body: %s", body)
			}
			// maxOutputTokens must survive the suppression: only the
			// sampling knobs are deprecated, not the whole config.
			must.Contains(string(body), `"maxOutputTokens":4096`,
				"maxOutputTokens dropped alongside temperature (body: %s)", body)
		})
	}
}

// TestGeminiQuirks_ThinkingLevel_Wire pins the generationConfig
// .thinkingConfig.thinkingLevel projection, including the uppercase
// coercion — the REST enum is THINKING_LEVEL_*, and while the API
// tolerates lowercase, emitting the documented casing keeps the wire
// body readable against the reference.
func TestGeminiQuirks_ThinkingLevel_Wire(t *testing.T) {
	const model = "gemini-3.7-flash"
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: model})

	t.Run("explicit level is uppercased with summaries", func(t *testing.T) {
		must := require.New(t)
		params := geminiQuirksCanonicalParams(model)
		params.ReasoningEffort = "high"
		body, _, err := BuildGenerateContentRequest(params, nil, q)
		must.NoError(err)
		must.Contains(string(body), `"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}`,
			"body: %s", body)
	})

	// Empty effort sends no level so the model keeps its own default; an
	// emitted THINKING_LEVEL_UNSPECIFIED would be a behaviour change
	// disguised as a default.
	t.Run("empty level sends summaries only", func(t *testing.T) {
		must := require.New(t)
		params := geminiQuirksCanonicalParams(model)
		params.ReasoningEffort = ""
		body, _, err := BuildGenerateContentRequest(params, nil, q)
		must.NoError(err)
		must.Contains(string(body), `"thinkingConfig":{"includeThoughts":true}`, "body: %s", body)
	})

	t.Run("empty level and summary off omit thinking config", func(t *testing.T) {
		must := require.New(t)
		params := geminiQuirksCanonicalParams(model)
		params.ReasoningEffort = ""
		params.ReasoningSummary = types.ReasoningSummaryOff
		body, _, err := BuildGenerateContentRequest(params, nil, q)
		must.NoError(err)
		must.NotContains(string(body), "thinkingConfig", "body: %s", body)
	})
}

// TestGeminiQuirks_ThinkingLevel_RejectedBeforeWire pins the
// fail-before-send guard: "minimal" is a documented 400 on 3.7 Flash,
// so the adapter must refuse to build the body rather than spend a
// round trip discovering it. 3.6 Flash accepts the same level, and a
// model with no probed allow-list passes any thinkingLevel enum member
// through; "xhigh" and "max" have no Gemini spelling on any model.
func TestGeminiQuirks_ThinkingLevel_RejectedBeforeWire(t *testing.T) {
	cases := []struct {
		model     string
		level     string
		wantError bool
	}{
		{"gemini-3.7-flash", "minimal", true},
		{"gemini-3.7-flash", "low", false},
		{"gemini-3.7-flash", "high", false},
		{"gemini-3.6-flash", "minimal", false},
		{"gemini-3.1-pro-preview", "minimal", false},
		{"gemini-unknown-future", "minimal", false},
		{"gemini-3.8-flash", "minimal", true},
		{"gemini-3.8-flash", "medium", false},
		{"gemini-unknown-future", "xhigh", true},
		{"gemini-3.1-pro-preview", "max", true},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			must := require.New(t)
			params := geminiQuirksCanonicalParams(tc.model)
			q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: tc.model})
			params.ReasoningEffort = tc.level
			_, _, err := BuildGenerateContentRequest(params, nil, q)
			if !tc.wantError {
				must.NoError(err)
				return
			}
			must.Error(err)
			must.ErrorContains(err, tc.level, "error message must name the level")
			must.ErrorContains(err, tc.model, "error message must name the model")
		})
	}
}

// TestGeminiQuirks_Gemini37Flash_WireFixture pins the full 3.7 Flash
// request body, tool round trip included, so any future rule that
// changes the shape has to restate it here.
func TestGeminiQuirks_Gemini37Flash_WireFixture(t *testing.T) {
	must := require.New(t)
	params := geminiToolRoundTripParams("gemini-3.7-flash")
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: params.Model})
	params.ReasoningEffort = "low"
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err)
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.7-flash", "request.json"), body)
}

// TestGeminiQuirks_Gemini36Flash_WireFixture is the 3.6 counterpart.
// The two fixtures differ only in the model-specific thinking level, so
// a diff between them isolates what 3.7 actually changed.
func TestGeminiQuirks_Gemini36Flash_WireFixture(t *testing.T) {
	must := require.New(t)
	params := geminiToolRoundTripParams("gemini-3.6-flash")
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: params.Model})
	params.ReasoningEffort = "minimal"
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err)
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.6-flash", "request.json"), body)
}

// TestGeminiQuirks_ThoughtSignatureSurvivesRoleSwitch pins that moving
// the tool result onto role:"user" did not disturb the functionCall
// part's thoughtSignature, which Gemini 3 requires on every replayed
// call — dropping it is a 400 one turn later.
func TestGeminiQuirks_ThoughtSignatureSurvivesRoleSwitch(t *testing.T) {
	must := require.New(t)
	params := geminiToolRoundTripParams("gemini-3.7-flash")
	q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: params.Model})
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err)
	must.Contains(string(body), `"thoughtSignature":"sig-abc"`,
		"thoughtSignature missing from replayed functionCall (body: %s)", body)
}
