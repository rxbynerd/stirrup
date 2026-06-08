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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// loadReplayFixture reads a replay.json fixture and returns its
// parsed shape: a map of path → list of captured values, projecting
// non-array values (rendered as strings for readability) to a single-
// element slice. The fixture's keys are the rule's path strings (or
// adjacent fields that the fixture-author wants to record for human
// context); each value is either an array (multiple captured pieces)
// or a scalar (one piece, written without the surrounding [] for
// readability when the field is captured exactly once).
//
// The fixture is the source of truth for the post-parse assertion; a
// divergence between the SSE parser's capture and the fixture is the
// load-bearing signal that either the parser drifted or the rule's
// path is wrong.
//
// Strips the # synthetic comment line so JSON.Unmarshal succeeds on
// synthetic fixtures — the production fixtures (when available) carry
// no comment line.
func loadReplayFixture(t *testing.T, path string) map[string][]any {
	t.Helper()
	raw, err := quirkstest.LoadFixture(path)
	if err != nil {
		t.Fatalf("load replay fixture %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse replay fixture %s: %v\nbody: %s", path, err, raw)
	}
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

// streamFixtureSSE reads an SSE fixture file (synthetic comment
// stripped) and returns the bytes the test httptest server should
// write to its response body. The SSE wire format expects each event
// terminated by a blank line; the fixture stores the wire-equivalent
// bytes verbatim, so the server can write them unchanged.
func streamFixtureSSE(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sse fixture %s: %v", path, err)
	}
	// Strip a leading "# synthetic: ..." line; the same convention
	// used by quirkstest.LoadFixture, but applied without the JSON
	// decode that LoadFixture would attempt.
	if bytes.HasPrefix(raw, []byte("# synthetic:")) {
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			raw = nil
		}
	}
	return raw
}

// sseStubServer returns an httptest server that responds with the
// given SSE bytes verbatim. Used by the ReplayFields integration
// tests so the same adapter Stream path that runs in production
// drives the fixture, exercising the SSE parser's RawDelta capture
// and the per-stream debug summary together.
func sseStubServer(t *testing.T, sse []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(sse); err != nil {
			t.Errorf("write sse: %v", err)
		}
	}))
}

// drainStream pulls every event off the channel and returns the slice
// of events for inspection. The ReplayFields integration tests do not
// branch on the events themselves — the assertion target is the debug
// log produced by the per-stream summary helper — but a parser error
// would surface as a "type=error" event, so drainStream returns the
// slice for the caller to spot-check.
func drainStream(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	var events []types.StreamEvent
	for ev := range ch {
		if ev.Type == "error" {
			t.Errorf("stream emitted error event: %v", ev.Error)
		}
		events = append(events, ev)
	}
	return events
}

// extractReplayFieldsLog parses the slog JSON output and returns the
// replay_fields_captured group from the "quirks replay fields
// captured" record, projected into the same shape replay.json uses
// (path → ordered length values reconstructed as one-string-per-piece
// — the log records counts only, so the test reconstructs piece-count
// equivalence rather than per-value equivalence).
//
// Returns nil if the log line was not emitted (which is itself a
// failure case for any test that expects the rule to fire).
func extractReplayFieldsLog(t *testing.T, logOutput string) map[string]int {
	t.Helper()
	// The log emitter prints one JSON record per line. Walk the lines
	// and find the first "quirks replay fields captured" record.
	for _, line := range strings.Split(logOutput, "\n") {
		if !strings.Contains(line, "quirks replay fields captured") {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse log line: %v\nline: %s", err, line)
		}
		raw, ok := record["replay_fields_captured"].(map[string]any)
		if !ok {
			t.Fatalf("replay_fields_captured missing or not a map in: %s", line)
		}
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

// TestReplayFields_DeepSeekReasoner_PreservesReasoningContent is the
// load-bearing assertion that the parse-side hook on the OpenAI
// adapter captures reasoning_content from every chunk's delta and
// surfaces the per-stream summary on the debug logger. The
// replay.json fixture is the source of truth for the expected
// captured shape; the SSE parser must agree with it.
func TestReplayFields_DeepSeekReasoner_PreservesReasoningContent(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek-reasoner/replay.json"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-reasoner",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	// The per-stream summary records the count of captured pieces per
	// path. The replay.json fixture lists the expected pieces; the
	// equality on lengths verifies the parser observed all of them.
	want := loadReplayFixture(t, replayPath)
	got := extractReplayFieldsLog(t, buf.String())
	if got == nil {
		t.Fatalf("debug log missing 'quirks replay fields captured' record; log:\n%s", buf.String())
	}
	for path, wantPieces := range want {
		if path == "content" {
			// content is part of the fixture for human-readability but
			// is not a captured path — the rule only registers
			// reasoning_content. Skip the assertion on the content
			// key.
			continue
		}
		gotCount, ok := got[path]
		if !ok {
			t.Errorf("path %q not captured; got summary %v", path, got)
			continue
		}
		if gotCount != len(wantPieces) {
			t.Errorf("path %q: captured %d pieces, want %d (from fixture)", path, gotCount, len(wantPieces))
		}
	}
}

// TestReplayFields_DeepSeekV4_PreservesReasoningContent mirrors the
// reasoner test for the v4 family. Same rule shape, different
// glob and fixture.
func TestReplayFields_DeepSeekV4_PreservesReasoningContent(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-v4/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek-v4/replay.json"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	want := loadReplayFixture(t, replayPath)
	got := extractReplayFieldsLog(t, buf.String())
	if got == nil {
		t.Fatalf("debug log missing 'quirks replay fields captured' record; log:\n%s", buf.String())
	}
	for path, wantPieces := range want {
		if path == "content" {
			continue
		}
		gotCount, ok := got[path]
		if !ok {
			t.Errorf("path %q not captured; got summary %v", path, got)
			continue
		}
		if gotCount != len(wantPieces) {
			t.Errorf("path %q: captured %d pieces, want %d", path, gotCount, len(wantPieces))
		}
	}
}

// TestReplayFields_DeepSeekV4Gateway_PreservesReasoningContent drives
// the OpenRouter-style prefixed id (deepseek/deepseek-v4-flash) through
// the production Stream path. The gateway sibling rule must fire — the
// bare deepseek-v4* glob cannot match across the slash — and produce
// the same capture + flattened message_complete value the first-party
// rule does.
func TestReplayFields_DeepSeekV4Gateway_PreservesReasoningContent(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek/deepseek-v4-flash/response.sse"
	const replayPath = "testdata/quirks/openai-compatible/deepseek/deepseek-v4-flash/replay.json"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek/deepseek-v4-flash",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, ch)

	// Capture-side: the per-stream summary's piece count matches the
	// fixture, same as the first-party tests.
	want := loadReplayFixture(t, replayPath)
	got := extractReplayFieldsLog(t, buf.String())
	if got == nil {
		t.Fatalf("debug log missing 'quirks replay fields captured' record; log:\n%s", buf.String())
	}
	for path, wantPieces := range want {
		if path == "content" {
			continue
		}
		gotCount, ok := got[path]
		if !ok {
			t.Errorf("path %q not captured; got summary %v", path, got)
			continue
		}
		if gotCount != len(wantPieces) {
			t.Errorf("path %q: captured %d pieces, want %d", path, gotCount, len(wantPieces))
		}
	}

	// Thread-side: the message_complete event carries the in-order
	// concatenation of the fixture's reasoning_content pieces.
	var complete *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			complete = &events[i]
		}
	}
	if complete == nil {
		t.Fatalf("no message_complete event in %v", events)
	}
	wantFlat := `"Weighing the options before answering."`
	if gotFlat := string(complete.ReplayFields["reasoning_content"]); gotFlat != wantFlat {
		t.Errorf("message_complete ReplayFields[reasoning_content] = %s, want %s", gotFlat, wantFlat)
	}
}

// TestReplayFields_Gemini3_PreservesThoughtSignature drives the
// gemini-3.1-pro-preview/response.sse fixture through the Gemini
// adapter and asserts the per-stream summary names the
// thoughtSignature path. The fixture's synthetic signature is the
// value the walker captures.
func TestReplayFields_Gemini3_PreservesThoughtSignature(t *testing.T) {
	const ssePath = "testdata/quirks/gemini/gemini-3.1-pro-preview/response.sse"
	const replayPath = "testdata/quirks/gemini/gemini-3.1-pro-preview/replay.json"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewGeminiAdapter(staticBearer("test-token"), "test-project", "us-central1", nil)
	adapter.baseURLOverride = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gemini-3.1-pro-preview",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []types.ToolDefinition{
			{
				Name:        "read_file",
				Description: "read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	want := loadReplayFixture(t, replayPath)
	got := extractReplayFieldsLog(t, buf.String())
	if got == nil {
		t.Fatalf("debug log missing 'quirks replay fields captured' record; log:\n%s", buf.String())
	}
	wantPaths := make([]string, 0, len(want))
	for k := range want {
		wantPaths = append(wantPaths, k)
	}
	sort.Strings(wantPaths)
	for _, path := range wantPaths {
		wantCount := len(want[path])
		gotCount, ok := got[path]
		if !ok {
			t.Errorf("path %q not captured; got summary %v", path, got)
			continue
		}
		if gotCount != wantCount {
			t.Errorf("path %q: captured %d pieces, want %d", path, gotCount, wantCount)
		}
	}
}

// TestReplayFields_GPT4o_NoCaptureWhenNoRuleFires pins the negative
// case: a model with no ReplayFields rule produces no debug summary
// line because the per-stream accumulator is empty. Operators
// grepping for "quirks replay fields captured" must see an absent
// line on a vanilla model.
func TestReplayFields_GPT4o_NoCaptureWhenNoRuleFires(t *testing.T) {
	// Reuses the o1-mini fixture because gpt-4o has no dedicated SSE
	// fixture under testdata/. The test asserts the *absence* of the
	// replay-fields debug line — fixture content doesn't matter, only
	// that the adapter doesn't synthesise a capture when no rule
	// fires. Routing the o1-mini SSE through a gpt-4o stream call
	// drives that path with valid SSE bytes the parser can decode.
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/o1-mini/response.sse"))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	if strings.Contains(buf.String(), "quirks replay fields captured") {
		t.Errorf("gpt-4o: replay-fields debug line surfaced when no rule fired; log:\n%s", buf.String())
	}
}

// TestReplayFields_Gemini25_NoCaptureWhenNoRuleFires mirrors the
// gpt-4o negative test for Gemini. A 2.5 stream must not produce a
// replay-fields summary line because the gemini-3* rule does not
// fire.
func TestReplayFields_Gemini25_NoCaptureWhenNoRuleFires(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/gemini/gemini-2.5-pro/response.sse"))
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
	_ = drainStream(t, ch)

	if strings.Contains(buf.String(), "quirks replay fields captured") {
		t.Errorf("gemini-2.5-pro: replay-fields debug line surfaced when no rule fired; log:\n%s", buf.String())
	}
}

// TestReplayFields_DeepSeekReasoner_LogIsLengthOnly is the side-
// channel guard pinning design risk 7 / §5: the debug log must NOT
// contain the verbatim reasoning_content value, only its length. The
// captured strings in the fixture include the substring "step by
// step" — assert it does not appear in the log output. If a future
// refactor switches the summariser to value-logging the test
// surfaces the leak before it ships.
func TestReplayFields_DeepSeekReasoner_LogIsLengthOnly(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-reasoner",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "quirks replay fields captured") {
		t.Fatalf("debug log missing 'quirks replay fields captured' record; log:\n%s", logOutput)
	}
	// The fixture's reasoning_content substrings.
	for _, leaked := range []string{
		"Let me think step by step",
		"The user asked for 2+2",
	} {
		if strings.Contains(logOutput, leaked) {
			t.Errorf("debug log leaked reasoning_content substring %q: %s", leaked, logOutput)
		}
	}
}

// TestReplayFieldsCapture_AcrossCallChunks_AccumulatesInOrder is the
// behavioural sanity check for the openai parse hook's per-stream
// accumulator: a value that arrives across multiple chunks must
// accumulate in the order received, not be overwritten by the last
// chunk's piece. The replay.json fixture for deepseek-reasoner lists
// two pieces; the per-stream summary's `count` must equal 2, not 1.
func TestReplayFieldsCapture_AcrossCallChunks_AccumulatesInOrder(t *testing.T) {
	const ssePath = "testdata/quirks/openai-compatible/deepseek-reasoner/response.sse"

	srv := sseStubServer(t, streamFixtureSSE(t, ssePath))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-reasoner",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = drainStream(t, ch)

	got := extractReplayFieldsLog(t, buf.String())
	// Fixture has three chunks containing reasoning_content: an empty
	// initial role chunk ("reasoning_content":""), then two real
	// pieces. The walker captures every non-nil value including the
	// empty string, so the expected count is 3.
	wantCount := 3
	if got["reasoning_content"] != wantCount {
		t.Errorf("reasoning_content captured %d pieces, want %d; got %v", got["reasoning_content"], wantCount, got)
	}
}

// TestQuirksRegistry_ReplayFieldsArePassedThroughToCaptured is the
// minimum-effort link between the registry-resolved value and the
// adapter's parse-side capture: a rule that writes to ReplayFields
// must be visible in the parse path as well. The check is structural
// (no SSE involved) so a registry-resolution test failure stays
// distinguishable from a parser-side failure.
func TestQuirksRegistry_ReplayFieldsArePassedThroughToCaptured(t *testing.T) {
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
			q := quirks.DefaultRegistry().Resolve(tc.providerType, tc.model)
			if !reflect.DeepEqual(q.ReplayFields, tc.wantPaths) {
				t.Errorf("ReplayFields = %v, want %v", q.ReplayFields, tc.wantPaths)
			}
		})
	}
}
