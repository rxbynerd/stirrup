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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
// gemini-2.5-pro: the resolved quirks set StreamArgsOff, so the
// marshalled request body emits no streamFunctionCallArguments field
// at all (omitempty drops false). The fixture is the source of truth
// for the canonical body; AssertWireEqual normalises both sides
// through unmarshal→marshal so key ordering and whitespace are not
// load-bearing.
func TestGeminiQuirks_Gemini25Pro_StreamArgsOff(t *testing.T) {
	must := require.New(t)
	params := geminiQuirksCanonicalParams("gemini-2.5-pro")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	must.Equal(quirks.StreamArgsOff, q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape,
		"gemini-2.5-pro: Gemini base rule must set StreamArgsOff")
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err)
	// Negative check ahead of the fixture comparison so a failure names
	// the load-bearing invariant directly: the wire body must not
	// enable streamFunctionCallArguments.
	must.NotContains(string(body), `"streamFunctionCallArguments":true`,
		"gemini-2.5-pro: request body must not enable streamFunctionCallArguments")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-2.5-pro", "request.json"), body)
}

// TestGeminiQuirks_Gemini31PreviewLocked_StreamArgsOff pins that
// Gemini 3.x's streamed-args wire format (JSON-path delta records)
// breaks the parser, so the adapter must continue to emit
// streamFunctionCallArguments=false (which omitempty drops from the
// body entirely) on every 3.x request until a verified rule explicitly
// opts into a different shape.
func TestGeminiQuirks_Gemini31PreviewLocked_StreamArgsOff(t *testing.T) {
	must := require.New(t)
	params := geminiQuirksCanonicalParams("gemini-3.1-pro-preview")
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	must.Equal(quirks.StreamArgsOff, q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape,
		"gemini-3.1-pro-preview: Gemini base rule must set StreamArgsOff")
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	must.NoError(err)
	must.NotContains(string(body), `"streamFunctionCallArguments":true`,
		"gemini-3.1-pro-preview: request body must not enable streamFunctionCallArguments")
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath(geminiFixtureRoot, "gemini-3.1-pro-preview", "request.json"), body)
}

// geminiQuirksLogStubServer returns an httptest server that drains
// the request body and returns a minimal valid Gemini SSE response
// (one text part + finishReason=STOP). Used by the logging tests so
// the Stream call completes the full debug/parse path without
// touching a real Vertex AI endpoint.
func geminiQuirksLogStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"+
				"data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
		)
	}))
}

// TestGeminiAdapter_QuirksDebugLogListsAppliedRules pins the
// per-Stream debug line in the Gemini adapter: it fires at the top of
// every Stream call and lists the descriptions of the rules that
// contributed to the resolution. The substring asserted is taken
// verbatim from the rule's Description field, so renaming the
// description requires updating this test in lockstep.
func TestGeminiAdapter_QuirksDebugLogListsAppliedRules(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	for range ch {
	}

	logOutput := buf.String()
	check.Contains(logOutput, "gemini quirks resolved")
	const wantRule = "Gemini: off streamFunctionCallArguments"
	check.Contains(logOutput, wantRule)
}

// TestGeminiAdapter_QuirksDebugLog_NilLoggerNoPanic confirms the
// adapter falls back to slog.Default() when Logger is nil rather
// than panicking on the DebugContext call. The nil-Logger path is
// guarded in Stream but the test pins it explicitly so a future
// refactor that drops the guard fails this test rather than
// surfacing as a nil-deref panic in production.
func TestGeminiAdapter_QuirksDebugLog_NilLoggerNoPanic(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	for range ch {
	}
}

// TestGeminiQuirks_ZeroValueIsIdenticalToDefaultRegistry pins that the
// registry-resolved ProviderQuirks value for a Gemini model with no
// model-specific rule is byte-identical, on the wire, to a zero-value
// ProviderQuirks: the base rule writes StreamArgsOff, which is the zero
// value of GeminiStreamArgsShape, so callers that bypass the registry
// would regress silently only if a base rule wrote a non-zero value.
func TestGeminiQuirks_ZeroValueIsIdenticalToDefaultRegistry(t *testing.T) {
	must := require.New(t)
	params := geminiQuirksCanonicalParams("gemini-2.5-pro")

	pre, _, err := BuildGenerateContentRequest(params, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	post, _, err := BuildGenerateContentRequest(params, nil, quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro"))
	must.NoError(err)
	must.Equal(string(pre), string(post), "zero-value and registry-resolved wire shapes must match")
}

// TestGeminiQuirks_OmitSamplingParams_NoOpForGemini checks that a Gemini
// resolution leaves the OpenAI behaviour flags at their zero values, so a
// Gemini-keyed ProviderQuirks never carries OpenAI settings.
func TestGeminiQuirks_OmitSamplingParams_NoOpForGemini(t *testing.T) {
	for _, model := range []string{"gemini-2.5-pro", "gemini-3.1-pro-preview", "gemini-unknown-future"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			q := quirks.DefaultRegistry().Resolve("gemini", model)
			must.False(q.BehaviourFlags.OpenAI.OmitSamplingParams,
				"Gemini rules must not set OpenAI.OmitSamplingParams")
			must.Equal(quirks.TokenFieldMaxCompletionTokens, q.BehaviourFlags.OpenAI.TokenField,
				"Gemini rules must leave OpenAI.TokenField at its zero default")
			must.Empty(q.BehaviourFlags.OpenAI.ExtraBodyFields,
				"Gemini rules must not add OpenAI.ExtraBodyFields")
		})
	}
}

func TestGeminiRequest_WarnsWhenQuirkSuppressesTemperature(t *testing.T) {
	run := func(t *testing.T, model string, temperature *float64) string {
		t.Helper()
		must := require.New(t)
		srv := geminiQuirksLogStubServer(t)
		defer srv.Close()

		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		adapter := NewGeminiAdapter(staticBearer("test-token"), "test-project", "us-central1", nil)
		adapter.baseURLOverride = srv.URL
		adapter.Logger = logger

		ch, err := adapter.Stream(context.Background(), types.StreamParams{
			Model:       model,
			MaxTokens:   1024,
			Temperature: temperature,
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			},
		})
		must.NoError(err)
		for range ch {
		}
		return buf.String()
	}

	t.Run("suppressing rule with a caller temperature warns without the value", func(t *testing.T) {
		check := assert.New(t)
		out := run(t, "gemini-3.7-pro", types.Float64Ptr(0.123456))
		check.Contains(out, "gemini quirks suppressed caller temperature")
		check.Contains(out, "omit deprecated sampling params")
		check.NotContains(out, "0.123456")
	})

	t.Run("suppressing rule without a caller temperature stays quiet", func(t *testing.T) {
		check := assert.New(t)
		out := run(t, "gemini-3.7-pro", nil)
		check.NotContains(out, "suppressed caller temperature")
	})

	t.Run("model without a suppressing rule stays quiet", func(t *testing.T) {
		check := assert.New(t)
		out := run(t, "gemini-2.5-pro", types.Float64Ptr(0.5))
		check.NotContains(out, "suppressed caller temperature")
	})
}
