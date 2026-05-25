package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
// the modern key and never the legacy key in the same body. Reasoning-
// class models additionally assert temperature is absent from the
// wire body, because quirksCanonicalParams supplies a non-nil
// Temperature: the suppression path is the live contract for those
// models and the negative assertion here closes the gap with the
// dedicated O1MiniOmitsSamplingParams test.
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
			if q.BehaviourFlags.OpenAI.OmitSamplingParams && strings.Contains(s, `"temperature"`) {
				t.Errorf("model %q: body contains 'temperature' despite OmitSamplingParams=true (suppression failed): %s", model, s)
			}
		})
	}
}

// TestOpenAIRequest_MarshalJSON_ExtraBodyFieldCollision pins the two
// error-return arms in MarshalJSON that guard against a rule author
// bypassing the canonical field set via ExtraBodyFields. Without
// these guards, a rule could side-channel a value for a canonical
// field (e.g. force temperature back on after suppression, or replace
// the model name) by writing to ExtraBodyFields instead of the
// canonical OpenAIBehaviourFlags slots. Each sub-case asserts the
// error fires and that the message contains a substring pinning the
// guard's identity so a refactor cannot silently weaken the check.
func TestOpenAIRequest_MarshalJSON_ExtraBodyFieldCollision(t *testing.T) {
	const wantSubstring = "collides with canonical request field"
	cases := []struct {
		name string
		req  openaiRequest
	}{
		{
			name: "model-key collision (already-emitted canonical field)",
			req: openaiRequest{
				Model:           "gpt-4o",
				MaxTokens:       100,
				ExtraBodyFields: map[string]any{"model": "evil-override"},
			},
		},
		{
			name: "temperature collision against canonical guard when suppressed",
			req: openaiRequest{
				Model:              "o1-mini",
				MaxTokens:          100,
				OmitSamplingParams: true,
				ExtraBodyFields:    map[string]any{"temperature": 0.5},
			},
		},
		{
			name: "max_completion_tokens collision (canonical token field)",
			req: openaiRequest{
				Model:           "gpt-4o",
				MaxTokens:       100,
				ExtraBodyFields: map[string]any{"max_completion_tokens": 99},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := json.Marshal(tc.req)
			if err == nil {
				t.Fatalf("MarshalJSON returned no error; expected collision")
			}
			if !strings.Contains(err.Error(), wantSubstring) {
				t.Errorf("error message = %q, want substring %q", err.Error(), wantSubstring)
			}
		})
	}
}

// TestOpenAIRequest_UnmarshalJSON covers the decode paths that
// MarshalJSON's tests would not exercise: the legacy max_tokens
// branch (Z.ai's wire shape), the extras-catch-all that preserves
// non-canonical top-level keys for round-trip stability, and the
// dual-key error guard introduced alongside this test.
func TestOpenAIRequest_UnmarshalJSON(t *testing.T) {
	t.Run("legacy max_tokens decodes to TokenFieldMaxTokens", func(t *testing.T) {
		raw := []byte(`{"model":"glm-4-plus","messages":[],"stream":true,"max_tokens":4096}`)
		var req openaiRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if req.TokenField != quirks.TokenFieldMaxTokens {
			t.Errorf("TokenField = %v, want TokenFieldMaxTokens", req.TokenField)
		}
		if req.MaxTokens != 4096 {
			t.Errorf("MaxTokens = %d, want 4096", req.MaxTokens)
		}
	})

	t.Run("modern max_completion_tokens decodes to TokenFieldMaxCompletionTokens", func(t *testing.T) {
		raw := []byte(`{"model":"o1-mini","messages":[],"stream":true,"max_completion_tokens":2048}`)
		var req openaiRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if req.TokenField != quirks.TokenFieldMaxCompletionTokens {
			t.Errorf("TokenField = %v, want TokenFieldMaxCompletionTokens", req.TokenField)
		}
		if req.MaxTokens != 2048 {
			t.Errorf("MaxTokens = %d, want 2048", req.MaxTokens)
		}
	})

	t.Run("non-canonical top-level key surfaces in ExtraBodyFields", func(t *testing.T) {
		raw := []byte(`{"model":"glm-4-plus","messages":[],"stream":true,"max_tokens":4096,"tool_stream":true}`)
		var req openaiRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if v, ok := req.ExtraBodyFields["tool_stream"]; !ok || v != true {
			t.Errorf("ExtraBodyFields[tool_stream] = %v (ok=%v), want true", v, ok)
		}
	})

	t.Run("dual token keys are rejected", func(t *testing.T) {
		raw := []byte(`{"model":"gpt-4o","messages":[],"stream":true,"max_completion_tokens":100,"max_tokens":200}`)
		var req openaiRequest
		err := json.Unmarshal(raw, &req)
		if err == nil {
			t.Fatalf("Unmarshal returned no error; expected dual-key rejection")
		}
		if !strings.Contains(err.Error(), "both max_completion_tokens and max_tokens present") {
			t.Errorf("error = %q, want substring 'both max_completion_tokens and max_tokens present'", err.Error())
		}
	})

	t.Run("round-trip with ExtraBodyFields preserves shape", func(t *testing.T) {
		original := openaiRequest{
			Model:           "glm-4-plus",
			Messages:        []openaiMessage{{Role: "user", Content: "hi"}},
			MaxTokens:       4096,
			Stream:          true,
			TokenField:      quirks.TokenFieldMaxTokens,
			ExtraBodyFields: map[string]any{"tool_stream": true},
		}
		out, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var round openaiRequest
		if err := json.Unmarshal(out, &round); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if round.Model != original.Model {
			t.Errorf("Model: got %q, want %q", round.Model, original.Model)
		}
		if round.MaxTokens != original.MaxTokens {
			t.Errorf("MaxTokens: got %d, want %d", round.MaxTokens, original.MaxTokens)
		}
		if round.TokenField != original.TokenField {
			t.Errorf("TokenField: got %v, want %v", round.TokenField, original.TokenField)
		}
		if round.Stream != original.Stream {
			t.Errorf("Stream: got %v, want %v", round.Stream, original.Stream)
		}
		if v, ok := round.ExtraBodyFields["tool_stream"]; !ok || v != true {
			t.Errorf("ExtraBodyFields[tool_stream] = %v (ok=%v), want true", v, ok)
		}
	})
}

// quirksLogStubServer returns an httptest server that drains the
// request body and returns an empty [DONE] SSE stream. Used by the
// logging tests so the Stream call completes the full warn/debug
// path without touching a real OpenAI endpoint.
func quirksLogStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestOpenAIAdapter_OmitSamplingParams_WarnsOnSuppressedTemperatureWithRuleDescription
// pins design risk 2: when OmitSamplingParams suppresses a caller-
// supplied non-nil Temperature, the warn log must (a) fire, (b) name
// the rule that caused the suppression so an operator can identify
// the source without reading code, and (c) NOT include the suppressed
// value itself (sidechannel concern — the caller's value would
// otherwise propagate into any log sink that captures warn records).
func TestOpenAIAdapter_OmitSamplingParams_WarnsOnSuppressedTemperatureWithRuleDescription(t *testing.T) {
	srv := quirksLogStubServer(t)
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "o1-mini",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "openai quirks suppressed caller temperature") {
		t.Errorf("warn log message absent from output: %s", logOutput)
	}
	// The reasoning-class rule description must appear in the warn
	// record so the operator can trace which rule fired. The exact
	// substring is the rule's Description field — if the rule text
	// changes the substring below must change with it.
	const wantRule = "OpenAI reasoning-class"
	if !strings.Contains(logOutput, wantRule) {
		t.Errorf("warn log missing rule description substring %q: %s", wantRule, logOutput)
	}
	// The suppressed value must NOT appear in the log. The number
	// "0.5" alone could collide with a metric value or a duration, so
	// pin against the named attribute key as well.
	if strings.Contains(logOutput, "temperature.suppressed") {
		t.Errorf("warn log contains legacy 'temperature.suppressed' attribute; the suppressed value must not leak: %s", logOutput)
	}
}

// TestOpenAIAdapter_DebugLogListsAppliedRules pins the per-Stream
// debug line from design §5: the line fires at the top of every
// Stream call and lists the descriptions of the rules that
// contributed to the resolution. Empty rule list is still logged so a
// future grep against the line never misses a resolution.
func TestOpenAIAdapter_DebugLogListsAppliedRules(t *testing.T) {
	srv := quirksLogStubServer(t)
	defer srv.Close()

	cases := []struct {
		name  string
		model string
		// Substrings the debug record must contain. Empty means no
		// rule fired — the debug record fires anyway with `rules:[]`.
		wantRuleSubstrings []string
	}{
		{
			name:               "reasoning-class rule fires",
			model:              "o1-mini",
			wantRuleSubstrings: []string{"OpenAI reasoning-class"},
		},
		{
			name:               "no rule fires for gpt-4o",
			model:              "gpt-4o",
			wantRuleSubstrings: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
			adapter.Logger = logger

			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:     tc.model,
				MaxTokens: 4096,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			for range ch {
			}

			logOutput := buf.String()
			if !strings.Contains(logOutput, "openai quirks resolved") {
				t.Errorf("debug log message absent: %s", logOutput)
			}
			for _, want := range tc.wantRuleSubstrings {
				if !strings.Contains(logOutput, want) {
					t.Errorf("debug log missing rule substring %q: %s", want, logOutput)
				}
			}
		})
	}
}
