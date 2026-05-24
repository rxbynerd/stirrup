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

// geminiFixtureRoot is the per-package-relative path to the Gemini
// fixture directories. Tests run in their package's source directory,
// so the path is relative to harness/internal/provider/.
const geminiFixtureRoot = "testdata/quirks/gemini"

// geminiQuirksCanonicalParams returns a representative StreamParams
// whose shape exercises every field driven by the Gemini quirks layer:
// the StreamFunctionCallArgsShape branch in the request builder fires
// only when tools are present, so the params always declare at least
// one tool. Reusing the same shape across every contract test keeps
// fixture-diff noise minimal: a difference between two fixtures
// reflects a rule change, not a parameter divergence.
func geminiQuirksCanonicalParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
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

// TestGeminiQuirks_Gemini25Pro_StreamArgsOff pins the wire shape for
// gemini-2.5-pro: the resolved quirks set StreamArgsOff (the
// post-#191 safe default), and the marshalled request body therefore
// emits no streamFunctionCallArguments field at all (omitempty drops
// false). The fixture is the source of truth for the canonical body;
// AssertWireEqual normalises both sides through unmarshal→marshal so
// key ordering and whitespace are not load-bearing.
func TestGeminiQuirks_Gemini25Pro_StreamArgsOff(t *testing.T) {
	params := geminiQuirksCanonicalParams("gemini-2.5-pro")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	if q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape != quirks.StreamArgsOff {
		t.Fatalf("gemini-2.5-pro: expected StreamArgsOff, got %v (Gemini base rule not firing)", q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape)
	}
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	// Negative wire-shape check ahead of the fixture comparison so a
	// failure here names the load-bearing invariant directly: the wire
	// body must NOT enable streamFunctionCallArguments. A future rule
	// that flips this back to true on 2.5 would either need to be
	// removed or replace this fixture in lockstep.
	if strings.Contains(string(body), `"streamFunctionCallArguments":true`) {
		t.Errorf("gemini-2.5-pro: body contains streamFunctionCallArguments=true (post-#191 default violated): %s", body)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-2.5-pro", "request.json"), body)
}

// TestGeminiQuirks_Gemini31PreviewLocked_StreamArgsOff is the
// regression guard for PR #191: Gemini 3.x's streamed-args wire
// format (JSON-path delta records) breaks the parser, so the adapter
// must continue to emit streamFunctionCallArguments=false (which
// omitempty drops from the body entirely) on every 3.x request until
// a verified rule explicitly opts into a different shape. The fixture
// pins the byte-identical wire body produced under the quirks-resolved
// path; a divergence here is a behaviour change in the Gemini 3.x
// pre-existing contract that requires a deliberate review.
func TestGeminiQuirks_Gemini31PreviewLocked_StreamArgsOff(t *testing.T) {
	params := geminiQuirksCanonicalParams("gemini-3.1-pro-preview")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	if q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape != quirks.StreamArgsOff {
		t.Fatalf("gemini-3.1-pro-preview: expected StreamArgsOff, got %v (post-#191 default violated)", q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape)
	}
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	if strings.Contains(string(body), `"streamFunctionCallArguments":true`) {
		t.Errorf("gemini-3.1-pro-preview: body contains streamFunctionCallArguments=true (post-#191 regression): %s", body)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.1-pro-preview", "request.json"), body)
}

// geminiQuirksLogStubServer returns an httptest server that drains
// the request body and returns a minimal valid Gemini SSE response
// (one text part + finishReason=STOP). Used by the logging tests so
// the Stream call completes the full debug/parse path without
// touching a real Vertex AI endpoint.
func geminiQuirksLogStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"+
				"data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
		)
	}))
}

// TestGeminiAdapter_QuirksDebugLogListsAppliedRules pins the
// per-Stream debug line in the Gemini adapter (design §5): the line
// fires at the top of every Stream call and lists the descriptions
// of the rules that contributed to the resolution. Mirrors the
// openai-counterpart TestOpenAIAdapter_DebugLogListsAppliedRules
// so a future change to the resolution-logging convention is caught
// uniformly across adapters.
//
// gemini-2.5-pro is sufficient to trigger the Gemini base rule
// ("Gemini: off streamFunctionCallArguments"); the substring asserted
// is taken verbatim from the rule's Description field, so renaming
// the description requires updating this test in lockstep.
func TestGeminiAdapter_QuirksDebugLogListsAppliedRules(t *testing.T) {
	srv := geminiQuirksLogStubServer(t)
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewGeminiAdapter(staticBearer("test-token"), "test-project", "us-central1", nil)
	adapter.baseURLOverride = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gemini-2.5-pro",
		MaxTokens: 1024,
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
	if !strings.Contains(logOutput, "gemini quirks resolved") {
		t.Errorf("debug log message absent from output: %s", logOutput)
	}
	const wantRule = "Gemini: off streamFunctionCallArguments"
	if !strings.Contains(logOutput, wantRule) {
		t.Errorf("debug log missing rule description substring %q: %s", wantRule, logOutput)
	}
}

// TestGeminiAdapter_QuirksDebugLog_NilLoggerNoPanic confirms the
// adapter falls back to slog.Default() when Logger is nil rather
// than panicking on the DebugContext call. The nil-Logger path is
// guarded in Stream but the test pins it explicitly so a future
// refactor that drops the guard fails this test rather than
// surfacing as a nil-deref panic in production.
func TestGeminiAdapter_QuirksDebugLog_NilLoggerNoPanic(t *testing.T) {
	srv := geminiQuirksLogStubServer(t)
	defer srv.Close()

	adapter := NewGeminiAdapter(staticBearer("test-token"), "test-project", "us-central1", nil)
	adapter.baseURLOverride = srv.URL
	adapter.Logger = nil

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gemini-2.5-pro",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
}

// TestGeminiQuirks_ZeroValueIsIdenticalToDefaultRegistry pins the
// load-bearing design invariant from PR #316: the registry-resolved
// ProviderQuirks value for any Gemini model is byte-identical, on
// the wire, to a zero-value ProviderQuirks. The base rule
// ("Gemini: off streamFunctionCallArguments") writes StreamArgsOff,
// which IS the zero value of GeminiStreamArgsShape, so the rule is
// functionally a no-op — but the registry path is the canonical
// post-#191 default and callers that bypass the registry would
// regress silently if the rule ever wrote a non-zero value.
//
// The test marshals both paths to JSON and asserts bytes.Equal.
// A future rule that diverges the registry-resolved value from
// the zero-value default would fail this test, forcing a deliberate
// decision rather than a silent behaviour change.
func TestGeminiQuirks_ZeroValueIsIdenticalToDefaultRegistry(t *testing.T) {
	params := geminiQuirksCanonicalParams("gemini-3.1-pro-preview")

	pre, _, err := BuildGenerateContentRequest(params, nil, quirks.ProviderQuirks{})
	if err != nil {
		t.Fatalf("zero-value build: %v", err)
	}
	post, _, err := BuildGenerateContentRequest(params, nil, quirks.DefaultRegistry().Resolve("gemini", "gemini-3.1-pro-preview"))
	if err != nil {
		t.Fatalf("registry-resolved build: %v", err)
	}
	if !bytes.Equal(pre, post) {
		t.Errorf("zero-value vs registry-resolved wire shape diverged:\n pre:  %s\n post: %s", pre, post)
	}
}

// TestGeminiQuirks_OmitSamplingParams_NoOpForGemini is the
// cross-family isolation sanity check: a Gemini resolution must
// never touch the OpenAI behaviour flags. Without this guard a
// future rule author who copy-pastes from the openai-compatible
// rule set and forgets to retarget BehaviourFlags.Gemini could
// silently flip OmitSamplingParams on the OpenAI sub-struct of a
// Gemini-keyed quirks result — a no-op for the Gemini adapter today
// but the kind of latent cross-talk that surfaces months later when
// the same ProviderQuirks value is read by a different code path.
func TestGeminiQuirks_OmitSamplingParams_NoOpForGemini(t *testing.T) {
	for _, model := range []string{"gemini-2.5-pro", "gemini-3.1-pro-preview", "gemini-unknown-future"} {
		t.Run(model, func(t *testing.T) {
			q := quirks.DefaultRegistry().Resolve("gemini", model)
			if q.BehaviourFlags.OpenAI.OmitSamplingParams {
				t.Errorf("gemini/%s: OpenAI.OmitSamplingParams = true; Gemini rules must not touch OpenAI flags", model)
			}
			if q.BehaviourFlags.OpenAI.TokenField != quirks.TokenFieldMaxCompletionTokens {
				t.Errorf("gemini/%s: OpenAI.TokenField = %v, want zero default", model, q.BehaviourFlags.OpenAI.TokenField)
			}
			if len(q.BehaviourFlags.OpenAI.ExtraBodyFields) != 0 {
				t.Errorf("gemini/%s: OpenAI.ExtraBodyFields = %+v, want empty", model, q.BehaviourFlags.OpenAI.ExtraBodyFields)
			}
		})
	}
}
