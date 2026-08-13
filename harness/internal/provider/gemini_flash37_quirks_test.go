package provider

import (
	"encoding/json"
	"strings"
	"testing"

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
	var req struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal body: %v\nbody: %s", err, body)
	}
	roles := make([]string, 0, len(req.Contents))
	for _, c := range req.Contents {
		roles = append(roles, c.Role)
	}
	return roles
}

// TestGeminiQuirks_ToolResultRole_ByFamily pins the model-gated role
// switch. Gemini 3.6 removed "function" from the accepted role set, so
// the 3.6/3.7 families must place a functionResponse on role:"user"
// while every earlier family keeps the historical role. A regression
// here is not cosmetic: the wrong role is an HTTP 400 that kills the
// agentic loop on its second turn.
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
			params := geminiToolRoundTripParams(tc.model)
			q := quirks.DefaultRegistry().Resolve("gemini", tc.model)
			body, _, err := BuildGenerateContentRequest(params, nil, "", q)
			if err != nil {
				t.Fatalf("BuildGenerateContentRequest: %v", err)
			}
			roles := geminiContentRoles(t, body)
			if len(roles) != 3 {
				t.Fatalf("contents roles = %v, want 3 entries (user text, model turn, tool result)", roles)
			}
			if roles[2] != tc.want {
				t.Errorf("tool-result role = %q, want %q (body: %s)", roles[2], tc.want, body)
			}
		})
	}
}

// TestGeminiQuirks_OmitSamplingParams_ByFamily pins that the families
// which deprecate the sampling knobs send no temperature, while earlier
// families still do. The API ignores rather than rejects a deprecated
// temperature, so the only observable consequence is what the trace
// records — which is exactly why it needs a test rather than a live
// 400 to catch a regression.
func TestGeminiQuirks_OmitSamplingParams_ByFamily(t *testing.T) {
	cases := []struct {
		model string
		want  bool // want temperature on the wire
	}{
		{"gemini-2.5-pro", true},
		{"gemini-3.1-pro-preview", true},
		{"gemini-3.6-flash", false},
		{"gemini-3.7-flash", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			params := geminiQuirksCanonicalParams(tc.model)
			q := quirks.DefaultRegistry().Resolve("gemini", tc.model)
			body, _, err := BuildGenerateContentRequest(params, nil, "", q)
			if err != nil {
				t.Fatalf("BuildGenerateContentRequest: %v", err)
			}
			got := strings.Contains(string(body), `"temperature"`)
			if got != tc.want {
				t.Errorf("temperature present = %v, want %v (body: %s)", got, tc.want, body)
			}
			// maxOutputTokens must survive the suppression: only the
			// sampling knobs are deprecated, not the whole config.
			if !strings.Contains(string(body), `"maxOutputTokens":4096`) {
				t.Errorf("maxOutputTokens dropped alongside temperature: %s", body)
			}
		})
	}
}

// TestGeminiQuirks_ThinkingLevel_Wire pins the generationConfig
// .thinkingConfig.thinkingLevel projection, including the uppercase
// coercion — the REST enum is THINKING_LEVEL_*, and while the API
// tolerates lowercase, emitting the documented casing keeps the wire
// body readable against the reference.
func TestGeminiQuirks_ThinkingLevel_Wire(t *testing.T) {
	params := geminiQuirksCanonicalParams("gemini-3.7-flash")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)

	body, _, err := BuildGenerateContentRequest(params, nil, "high", q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	if !strings.Contains(string(body), `"thinkingConfig":{"thinkingLevel":"HIGH"}`) {
		t.Errorf("thinkingConfig absent or malformed: %s", body)
	}

	// Empty level says nothing on the wire so the model keeps its own
	// default; an emitted THINKING_LEVEL_UNSPECIFIED would be a
	// behaviour change disguised as a default.
	bare, _, err := BuildGenerateContentRequest(params, nil, "", q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest (bare): %v", err)
	}
	if strings.Contains(string(bare), "thinkingConfig") {
		t.Errorf("empty thinking level emitted thinkingConfig anyway: %s", bare)
	}
}

// TestGeminiQuirks_ThinkingLevel_RejectedBeforeWire pins the
// fail-before-send guard: "minimal" is a documented 400 on 3.7 Flash,
// so the adapter must refuse to build the body rather than spend a
// round trip discovering it. 3.6 Flash accepts the same level, and a
// model with no probed allow-list passes anything through.
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
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			params := geminiQuirksCanonicalParams(tc.model)
			q := quirks.DefaultRegistry().Resolve("gemini", tc.model)
			_, _, err := BuildGenerateContentRequest(params, nil, tc.level, q)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected rejection of level %q on %s, got none", tc.level, tc.model)
				}
				if !strings.Contains(err.Error(), tc.level) || !strings.Contains(err.Error(), tc.model) {
					t.Errorf("error message must name the level and model: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection of level %q on %s: %v", tc.level, tc.model, err)
			}
		})
	}
}

// TestGeminiQuirks_Gemini37Flash_WireFixture pins the full 3.7 Flash
// request body, tool round trip included, so any future rule that
// changes the shape has to restate it here.
func TestGeminiQuirks_Gemini37Flash_WireFixture(t *testing.T) {
	params := geminiToolRoundTripParams("gemini-3.7-flash")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	body, _, err := BuildGenerateContentRequest(params, nil, "low", q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.7-flash", "request.json"), body)
}

// TestGeminiQuirks_Gemini36Flash_WireFixture is the 3.6 counterpart.
// The two fixtures differ only in the model-specific thinking level, so
// a diff between them isolates what 3.7 actually changed.
func TestGeminiQuirks_Gemini36Flash_WireFixture(t *testing.T) {
	params := geminiToolRoundTripParams("gemini-3.6-flash")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	body, _, err := BuildGenerateContentRequest(params, nil, "minimal", q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.6-flash", "request.json"), body)
}

// TestGeminiQuirks_ThoughtSignatureSurvivesRoleSwitch pins that moving
// the tool result onto role:"user" did not disturb the functionCall
// part's thoughtSignature, which Gemini 3 requires on every replayed
// call — dropping it is a 400 one turn later.
func TestGeminiQuirks_ThoughtSignatureSurvivesRoleSwitch(t *testing.T) {
	params := geminiToolRoundTripParams("gemini-3.7-flash")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	body, _, err := BuildGenerateContentRequest(params, nil, "", q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	if !strings.Contains(string(body), `"thoughtSignature":"sig-abc"`) {
		t.Errorf("thoughtSignature missing from replayed functionCall: %s", body)
	}
}
