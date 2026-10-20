package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// loadReplayFixture reads a replay.json fixture and returns a map of
// path → list of captured values, projecting a scalar value to a
// single-element slice. Strips the # synthetic comment line, if present,
// so JSON.Unmarshal succeeds on synthetic fixtures.
func loadReplayFixture(t *testing.T, path string) map[string][]any {
	t.Helper()
	must := require.New(t)
	raw, err := quirkstest.LoadFixture(path)
	must.NoError(err, "load replay fixture %s", path)
	var doc map[string]any
	must.NoError(json.Unmarshal(raw, &doc), "parse replay fixture %s, body: %s", path, raw)
	out := map[string][]any{}
	for k, v := range doc {
		if arr, ok := v.([]any); ok {
			out[k] = arr
		} else {
			out[k] = []any{v}
		}
	}
	return out
}

// streamFixtureSSE reads an SSE fixture file (synthetic comment stripped)
// and returns the wire-equivalent bytes verbatim for a test server to write.
func streamFixtureSSE(t *testing.T, path string) []byte {
	t.Helper()
	must := require.New(t)
	raw, err := os.ReadFile(path)
	must.NoError(err, "read sse fixture %s", path)

	if bytes.HasPrefix(raw, []byte("# synthetic:")) {
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			raw = nil
		}
	}
	return raw
}

// sseStubServer returns an httptest server that responds with the given
// SSE bytes verbatim, so a test can drive the real adapter Stream path.
func sseStubServer(t *testing.T, sse []byte) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler runs on the server's goroutine, where require must not be used.
		_, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(sse)
		check.NoError(err, "write sse")
	}))
}

// drainStream pulls every event off the channel, failing the test if any
// is a "type=error" event, and returns the slice for inspection.
func drainStream(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	check := assert.New(t)
	var events []types.StreamEvent
	for ev := range ch {
		// The channel is read to close even after an error event, so the producer never blocks.
		check.NotEqual("error", ev.Type, "stream emitted error event: %v", ev.Error)
		events = append(events, ev)
	}
	return events
}

// extractReplayFieldsLog parses the slog JSON output and returns the
// per-path capture count from the "quirks replay fields captured" record.
// Returns nil if the log line was not emitted.
func extractReplayFieldsLog(t *testing.T, logOutput string) map[string]int {
	t.Helper()
	must := require.New(t)

	for _, line := range strings.Split(logOutput, "\n") {
		if !strings.Contains(line, "quirks replay fields captured") {
			continue
		}
		var record map[string]any
		must.NoError(json.Unmarshal([]byte(line), &record), "parse log line: %s", line)
		raw, ok := record["replay_fields_captured"].(map[string]any)
		must.True(ok, "replay_fields_captured missing or not a map in: %s", line)
		out := map[string]int{}
		for path, summary := range raw {
			s, ok := summary.(map[string]any)
			if !ok {
				continue
			}
			count, _ := s["count"].(float64)
			out[path] = int(count)
		}
		return out
	}
	return nil
}

// streamOpenAICompatibleWithDebugLog streams an SSE fixture through the
// OpenAI-compatible adapter for model. It returns the stream events and the
// debug-level log output.
func streamOpenAICompatibleWithDebugLog(t *testing.T, ssePath, model string) ([]types.StreamEvent, string) {
	t.Helper()
	must := require.New(t)
	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	must.NoError(err)
	events := drainStream(t, ch)
	return events, buf.String()
}

// streamGeminiWithDebugLog streams an SSE fixture through the Gemini adapter
// for model and returns the debug-level log output.
func streamGeminiWithDebugLog(t *testing.T, ssePath, model string, tools []types.ToolDefinition) string {
	t.Helper()
	must := require.New(t)
	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewGeminiAdapter(staticBearer("test-token"), "test-project", "us-central1", nil)
	adapter.baseURLOverride = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: tools,
	})
	must.NoError(err)
	drainStream(t, ch)
	return buf.String()
}

// requireReplayCaptureCounts checks that the debug log reports every path in
// want, each captured once per value in the fixture.
func requireReplayCaptureCounts(t *testing.T, want map[string][]any, logOutput string) {
	t.Helper()
	must := require.New(t)
	got := extractReplayFieldsLog(t, logOutput)
	must.NotNil(got, "debug log missing 'quirks replay fields captured' record; log:\n%s", logOutput)
	for path, wantPieces := range want {
		t.Run(path, func(t *testing.T) {
			must := require.New(t)
			gotCount, ok := got[path]
			must.True(ok, "path not captured; got summary %v", got)
			must.Equal(len(wantPieces), gotCount)
		})
	}
}

// TestReplayFields_DeepSeekReasoner_PreservesReasoningContent asserts the
// parse-side hook captures reasoning_content from every chunk's delta and
// surfaces the per-stream summary on the debug logger.
func TestReplayFields_DeepSeekReasoner_PreservesReasoningContent(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek-reasoner/replay.json"

	_, logOutput := streamOpenAICompatibleWithDebugLog(t, ssePath, "deepseek-reasoner")

	want := loadReplayFixture(t, replayPath)
	delete(want, "content") // Present for fixture readability; not a captured path.
	requireReplayCaptureCounts(t, want, logOutput)
}

// TestReplayFields_DeepSeekV4_PreservesReasoningContent mirrors the
// reasoner test for the v4 family (same rule shape, different glob).
func TestReplayFields_DeepSeekV4_PreservesReasoningContent(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-v4/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek-v4/replay.json"

	_, logOutput := streamOpenAICompatibleWithDebugLog(t, ssePath, "deepseek-v4")

	want := loadReplayFixture(t, replayPath)
	delete(want, "content") // Present for fixture readability; not a captured path.
	requireReplayCaptureCounts(t, want, logOutput)
}

// TestReplayFields_DeepSeekV4Gateway_PreservesReasoningContent drives the
// OpenRouter-style prefixed id (deepseek/deepseek-v4-flash) through the
// production Stream path; the gateway sibling rule must fire since the
// bare deepseek-v4* glob cannot match across the slash.
func TestReplayFields_DeepSeekV4Gateway_PreservesReasoningContent(t *testing.T) {
	must := require.New(t)
	const ssePath = "testdata/quirks/openai-compatible/deepseek/deepseek-v4-flash/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek/deepseek-v4-flash/replay.json"

	events, logOutput := streamOpenAICompatibleWithDebugLog(t, ssePath, "deepseek/deepseek-v4-flash")

	want := loadReplayFixture(t, replayPath)
	delete(want, "content") // Present for fixture readability; not a captured path.
	requireReplayCaptureCounts(t, want, logOutput)

	var complete *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			complete = &events[i]
		}
	}
	must.NotNil(complete, "no message_complete event in %v", events)
	wantFlat := `"Weighing the options before answering."`
	must.Equal(wantFlat, string(complete.ReplayFields["reasoning_content"]))
}

// TestReplayFields_Gemini3_PreservesThoughtSignature drives the
// gemini-3.1-pro-preview/response.sse fixture through the Gemini adapter
// and asserts the per-stream summary names the thoughtSignature path.
func TestReplayFields_Gemini3_PreservesThoughtSignature(t *testing.T) {
	const ssePath = "testdata/quirks/gemini/gemini-3.1-pro-preview/response.sse"
	const replayPath = "testdata/quirks/gemini/gemini-3.1-pro-preview/replay.json"

	logOutput := streamGeminiWithDebugLog(t, ssePath, "gemini-3.1-pro-preview", []types.ToolDefinition{
		{
			Name:        "read_file",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
	})

	requireReplayCaptureCounts(t, loadReplayFixture(t, replayPath), logOutput)
}

// TestReplayFields_GPT4o_NoCaptureWhenNoRuleFires pins the negative case:
// a model with no ReplayFields rule produces no debug summary line.
func TestReplayFields_GPT4o_NoCaptureWhenNoRuleFires(t *testing.T) {
	must := require.New(t)
	// Reuses the o1-mini fixture: gpt-4o has no dedicated SSE fixture, and
	// only the *absence* of the debug line is under test here.
	_, logOutput := streamOpenAICompatibleWithDebugLog(t, "testdata/quirks/openai-compatible/o1-mini/response.sse", "gpt-4o")

	must.NotContains(logOutput, "quirks replay fields captured",
		"replay-fields debug line surfaced when no rule fired; log:\n%s", logOutput)
}

// TestReplayFields_Gemini25_NoCaptureWhenNoRuleFires mirrors the gpt-4o
// negative test: a 2.5 stream must not fire the gemini-3* rule.
func TestReplayFields_Gemini25_NoCaptureWhenNoRuleFires(t *testing.T) {
	must := require.New(t)
	logOutput := streamGeminiWithDebugLog(t, "testdata/quirks/gemini/gemini-2.5-pro/response.sse", "gemini-2.5-pro", nil)

	must.NotContains(logOutput, "quirks replay fields captured",
		"replay-fields debug line surfaced when no rule fired; log:\n%s", logOutput)
}

// TestReplayFields_DeepSeekReasoner_LogIsLengthOnly asserts the debug log
// records only the captured length, never the verbatim reasoning_content
// value.
func TestReplayFields_DeepSeekReasoner_LogIsLengthOnly(t *testing.T) {
	must := require.New(t)
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"

	_, logOutput := streamOpenAICompatibleWithDebugLog(t, ssePath, "deepseek-reasoner")

	must.Contains(logOutput, "quirks replay fields captured",
		"debug log missing 'quirks replay fields captured' record; log:\n%s", logOutput)

	for _, tc := range []struct {
		name   string
		leaked string
	}{
		{"step-by-step reasoning", "Let me think step by step"},
		{"arithmetic reasoning", "The user asked for 2+2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(logOutput, tc.leaked, "debug log leaked reasoning_content: %s", logOutput)
		})
	}
}

// TestReplayFieldsCapture_AcrossCallChunks_AccumulatesInOrder asserts a
// value arriving across multiple chunks accumulates in order, rather
// than being overwritten by the last chunk's piece.
func TestReplayFieldsCapture_AcrossCallChunks_AccumulatesInOrder(t *testing.T) {
	must := require.New(t)
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"

	_, logOutput := streamOpenAICompatibleWithDebugLog(t, ssePath, "deepseek-reasoner")

	got := extractReplayFieldsLog(t, logOutput)
	// Fixture has 3 reasoning_content chunks: an empty initial role chunk
	// plus two real pieces; the walker captures every non-nil value.
	wantCount := 3
	must.Equal(wantCount, got["reasoning_content"], "captured pieces; got %v", got)
}

// TestQuirksRegistry_Resolve_SetsReplayFields asserts the registry resolves
// each replay-capturing model to its ReplayFields paths.
func TestQuirksRegistry_Resolve_SetsReplayFields(t *testing.T) {
	cases := []struct {
		name         string
		providerType string
		model        string
		wantPaths    []string
	}{
		{"deepseek-reasoner", "openai-compatible", "deepseek-reasoner", []string{"reasoning_content"}},
		{"deepseek-v4", "openai-compatible", "deepseek-v4", []string{"reasoning_content"}},
		{"gemini-3", "gemini", "gemini-3.1-pro-preview", []string{"candidates[].content.parts[].thoughtSignature"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			q := quirks.DefaultRegistry().Resolve(tc.providerType, tc.model)
			must.Equal(tc.wantPaths, q.ReplayFields)
		})
	}
}
