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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func joinLines(lines ...string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func makeSSE(event, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n", event, data)
}

func collectEvents(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	var events []types.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func TestAnthropicAdapter_StreamTextDelta(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":" world"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("expected x-api-key=test-key, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("expected anthropic-version=%s, got %q", anthropicAPIVersion, got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Expect: text_delta("Hello"), text_delta(" world"), message_complete
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != "text_delta" || events[0].Text != "Hello" {
		t.Errorf("event[0] = %+v, want text_delta/Hello", events[0])
	}
	if events[1].Type != "text_delta" || events[1].Text != " world" {
		t.Errorf("event[1] = %+v, want text_delta/ world", events[1])
	}
	if events[2].Type != "message_complete" || events[2].StopReason != "end_turn" {
		t.Errorf("event[2] = %+v, want message_complete/end_turn", events[2])
	}
}

func TestAnthropicAdapter_StreamToolUse(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"read_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"\"main.go\"}"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != "tool_call" {
		t.Fatalf("event[0].Type = %q, want tool_call", events[0].Type)
	}
	if events[0].ID != "toolu_123" {
		t.Errorf("event[0].ID = %q, want toolu_123", events[0].ID)
	}
	if events[0].Name != "read_file" {
		t.Errorf("event[0].Name = %q, want read_file", events[0].Name)
	}
	if events[0].Input["path"] != "main.go" {
		t.Errorf("event[0].Input[path] = %v, want main.go", events[0].Input["path"])
	}

	if events[1].Type != "message_complete" || events[1].StopReason != "tool_use" {
		t.Errorf("event[1] = %+v, want message_complete/tool_use", events[1])
	}
}

func TestAnthropicAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("bad-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("expected error body in message, got: %v", err)
	}
}

func TestAnthropicAdapter_HTTPErrorNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}

func TestAnthropicAdapter_HTTPErrorBodyTruncated(t *testing.T) {
	largeBody := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, largeBody)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
	// Body should be truncated to 4096 bytes.
	errMsg := err.Error()
	if len(errMsg) > 4200 {
		t.Errorf("error message too large (%d chars), body should be truncated to 4096 bytes", len(errMsg))
	}
	if !strings.Contains(errMsg, "400") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}

// TestAnthropicAdapter_TransportError_DoesNotLeakCredentials drives the
// transport-error path with a credentialed baseURL pointed at a closed port.
// An operator may put credentials directly in a custom baseURL (userinfo or a
// gateway api_key query param); the *url.Error Go returns from Do embeds that
// URL and does not redact the query string, so the wrapped error must report
// only the dial-level cause (CWE-532, follow-up to #395).
func TestAnthropicAdapter_TransportError_DoesNotLeakCredentials(t *testing.T) {
	// A closed loopback port yields a connection-refused dial error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	u, err := url.Parse(closed)
	if err != nil {
		t.Fatalf("parse closed URL %q: %v", closed, err)
	}
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"

	adapter := NewAnthropicAdapter(staticBearer("key"), AuthModeAPIKey)
	adapter.baseURL = u.String()

	_, err = adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected a transport error dialing a closed port")
	}
	msg := err.Error()
	if strings.Contains(msg, "supersecret") {
		t.Errorf("transport error leaked the query secret: %q", msg)
	}
	if strings.Contains(msg, "user:pass") {
		t.Errorf("transport error leaked userinfo: %q", msg)
	}
}

func TestAnthropicAdapter_RequestBody(t *testing.T) {
	var received anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "claude-sonnet-4-6",
		System:      "You are helpful.",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	// Drain the channel.
	for range ch {
	}

	if !received.Stream {
		t.Error("expected stream=true in request body")
	}
	if received.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", received.Model)
	}
	if received.System != "You are helpful." {
		t.Errorf("system = %q, want 'You are helpful.'", received.System)
	}
	if received.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", received.MaxTokens)
	}
}

// TestAnthropicAdapter_TemperatureWireShape pins the unset-vs-explicit-zero
// semantics for StreamParams.Temperature (issue #200). A nil pointer must
// omit the "temperature" key entirely so callers who did not set it are
// not silently pinned to Anthropic's greedy-decoding behaviour at 0; an
// explicit Float64Ptr(0.0) must transmit "temperature":0.
func TestAnthropicAdapter_TemperatureWireShape(t *testing.T) {
	cases := []struct {
		name              string
		temperature       *float64
		wantTemperature   bool
		wantTempSubstring string
	}{
		{name: "nil omitted", temperature: nil, wantTemperature: false},
		{name: "explicit zero serialised", temperature: types.Float64Ptr(0.0), wantTemperature: true, wantTempSubstring: `"temperature":0`},
		{name: "non-zero serialised", temperature: types.Float64Ptr(0.5), wantTemperature: true, wantTempSubstring: `"temperature":0.5`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rawBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
			}))
			defer srv.Close()

			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = srv.URL

			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:       "claude-sonnet-4-6",
				MaxTokens:   1024,
				Temperature: tc.temperature,
			})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}

			body := string(rawBody)
			hasKey := strings.Contains(body, `"temperature"`)
			if tc.wantTemperature && !hasKey {
				t.Errorf("missing 'temperature' for non-nil pointer: %s", body)
			}
			if !tc.wantTemperature && hasKey {
				t.Errorf("contains 'temperature' for nil pointer (omitempty broken): %s", body)
			}
			if tc.wantTempSubstring != "" && !strings.Contains(body, tc.wantTempSubstring) {
				t.Errorf("missing %q in body: %s", tc.wantTempSubstring, body)
			}
		})
	}
}

// TestAnthropicAdapter_TemperatureSuppressedForNoSamplingParamsModels pins
// the fix for the model families that reject a non-default temperature
// outright (Claude Opus 4.7+, Sonnet 5, Fable 5 / Mythos 5 all return a 400
// rather than ignoring the field). Even though the harness loop always
// resolves a non-nil default temperature when RunConfig.Temperature is
// unset (core.defaultTemperature), the wire body must omit "temperature"
// entirely for these models. claude-sonnet-4-6 is the negative control: it
// still accepts a non-default temperature, so the key must survive there.
func TestAnthropicAdapter_TemperatureSuppressedForNoSamplingParamsModels(t *testing.T) {
	cases := []struct {
		model       string
		wantOmitted bool
	}{
		{model: "claude-opus-4-7", wantOmitted: true},
		{model: "claude-opus-4-8", wantOmitted: true},
		{model: "claude-sonnet-5", wantOmitted: true},
		{model: "claude-fable-5", wantOmitted: true},
		{model: "claude-mythos-5", wantOmitted: true},
		{model: "claude-sonnet-4-6", wantOmitted: false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			var rawBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
			}))
			defer srv.Close()

			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = srv.URL

			// Deliberately non-nil, non-default: pins that suppression
			// wins even when a caller (or the loop's own default) set an
			// explicit temperature.
			ch, err := adapter.Stream(context.Background(), types.StreamParams{
				Model:       tc.model,
				MaxTokens:   1024,
				Temperature: types.Float64Ptr(0.1),
			})
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}

			hasKey := strings.Contains(string(rawBody), `"temperature"`)
			if tc.wantOmitted && hasKey {
				t.Errorf("%s: body contains 'temperature' despite OmitSamplingParams suppression: %s", tc.model, rawBody)
			}
			if !tc.wantOmitted && !hasKey {
				t.Errorf("%s: body missing 'temperature' (over-broad suppression?): %s", tc.model, rawBody)
			}
		})
	}
}

// TestAnthropicAdapter_OmitSamplingParams_WarnsOnSuppressedTemperature
// mirrors the OpenAI adapter's design-risk-2 coverage: when the quirk
// suppresses a caller-supplied non-nil Temperature, the warn log must fire,
// name the rule that caused the suppression, and never include the
// suppressed value itself.
func TestAnthropicAdapter_OmitSamplingParams_WarnsOnSuppressedTemperature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "claude-sonnet-5",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "anthropic quirks suppressed caller temperature") {
		t.Errorf("warn log message absent from output: %s", logOutput)
	}
	const wantRule = "Claude Sonnet 5"
	if !strings.Contains(logOutput, wantRule) {
		t.Errorf("warn log missing rule description substring %q: %s", wantRule, logOutput)
	}
	if strings.Contains(logOutput, "0.5") {
		t.Errorf("warn log leaks the suppressed temperature value: %s", logOutput)
	}
}

// TestAnthropicAdapter_OmitSamplingParams_NoWarnWhenTemperatureUnset covers
// the "double negative" the suppression logic must handle correctly:
// a caller who never set Temperature at all, on a model in the omit
// family. The wire body must still omit "temperature" (the loop's own
// default-temperature resolution supplies a non-nil pointer in
// production, but buildAnthropicRequest must not depend on that — a
// direct nil is the base case), and — unlike
// TestAnthropicAdapter_OmitSamplingParams_WarnsOnSuppressedTemperature —
// the WARN log must NOT fire, since there is no caller-supplied value to
// warn about suppressing (mirrors the `params.Temperature != nil` guard
// on the log condition).
func TestAnthropicAdapter_OmitSamplingParams_NoWarnWhenTemperatureUnset(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "claude-sonnet-5",
		MaxTokens:   4096,
		Temperature: nil,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	if strings.Contains(string(rawBody), `"temperature"`) {
		t.Errorf("body contains 'temperature' for nil Temperature on an omit-family model: %s", rawBody)
	}
	if strings.Contains(buf.String(), "anthropic quirks suppressed caller temperature") {
		t.Errorf("warn log fired despite no caller-supplied Temperature to suppress: %s", buf.String())
	}
}

// TestAnthropicAdapter_DebugLogListsAppliedRules pins the per-Stream debug
// line, mirroring TestOpenAIAdapter_DebugLogListsAppliedRules: the line
// fires at the top of every Stream call and lists the descriptions of the
// rules that contributed to the resolution. An empty rule list is still
// logged (never happens for "anthropic" in practice, since the base "*"
// tool-choice/structured-result/parallel-call rules always fire, but the
// no-omission-rule case for claude-sonnet-4-6 is the useful negative here).
func TestAnthropicAdapter_DebugLogListsAppliedRules(t *testing.T) {
	cases := []struct {
		name               string
		model              string
		wantRuleSubstrings []string
	}{
		{
			name:               "sonnet-5 omission rule fires",
			model:              "claude-sonnet-5",
			wantRuleSubstrings: []string{"Claude Sonnet 5"},
		},
		{
			name:               "sonnet-4-6 has no omission rule",
			model:              "claude-sonnet-4-6",
			wantRuleSubstrings: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
			}))
			defer srv.Close()

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = srv.URL
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
			if !strings.Contains(logOutput, "anthropic quirks resolved") {
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

func TestSSE_DeltaForUnknownIndex(t *testing.T) {
	// Send a content_block_delta for an index that has no content_block_start.
	// The adapter should skip it silently — no panic, no error event.
	body := joinLines(
		makeSSE("content_block_delta", `{"index":5,"delta":{"type":"text_delta","text":"orphan"}}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Should only get message_complete, no text_delta for the orphan, no error.
	for _, ev := range events {
		if ev.Type == "error" {
			t.Errorf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == "text_delta" {
			t.Errorf("unexpected text_delta event for unknown index: %q", ev.Text)
		}
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (message_complete), got %d: %+v", len(events), events)
	}
	if events[0].Type != "message_complete" {
		t.Errorf("event[0].Type = %q, want message_complete", events[0].Type)
	}
}

func TestSSE_MalformedContentBlockStart(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{not valid json`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" {
			foundError = true
			if ev.Error == nil {
				t.Error("error event has nil Error field")
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed content_block_start JSON")
	}
}

func TestSSE_MalformedToolInput(t *testing.T) {
	// Accumulate invalid JSON via input_json_delta, then stop the block.
	// The adapter should emit an error when it tries to unmarshal.
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"read_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"INVALID"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" {
			foundError = true
			if ev.Error == nil {
				t.Error("error event has nil Error field")
			} else if !strings.Contains(ev.Error.Error(), "tool input JSON") {
				t.Errorf("expected error about tool input JSON, got: %v", ev.Error)
			}
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed tool input JSON")
	}
}

func TestSSE_MultipleBlocks(t *testing.T) {
	// Two tool_use blocks at different indices, interleaved.
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_aaa","name":"read_file"}}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"toolu_bbb","name":"write_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"b.go\",\"content\":\"x\"}"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Expect: tool_call(aaa), tool_call(bbb), message_complete.
	var toolCalls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "error" {
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool_call events, got %d", len(toolCalls))
	}

	// First tool call should be toolu_aaa / read_file.
	if toolCalls[0].ID != "toolu_aaa" {
		t.Errorf("toolCalls[0].ID = %q, want toolu_aaa", toolCalls[0].ID)
	}
	if toolCalls[0].Name != "read_file" {
		t.Errorf("toolCalls[0].Name = %q, want read_file", toolCalls[0].Name)
	}
	if toolCalls[0].Input["path"] != "a.go" {
		t.Errorf("toolCalls[0].Input[path] = %v, want a.go", toolCalls[0].Input["path"])
	}

	// Second tool call should be toolu_bbb / write_file.
	if toolCalls[1].ID != "toolu_bbb" {
		t.Errorf("toolCalls[1].ID = %q, want toolu_bbb", toolCalls[1].ID)
	}
	if toolCalls[1].Name != "write_file" {
		t.Errorf("toolCalls[1].Name = %q, want write_file", toolCalls[1].Name)
	}
	if toolCalls[1].Input["path"] != "b.go" {
		t.Errorf("toolCalls[1].Input[path] = %v, want b.go", toolCalls[1].Input["path"])
	}
}

func TestAnthropicAdapter_ContextCancellation(t *testing.T) {
	// Server that never finishes sending events.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := adapter.Stream(ctx, types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()

	events := collectEvents(t, ch)
	// Should get an error event from context cancellation.
	if len(events) == 0 {
		t.Fatal("expected at least one event after cancellation")
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Errorf("last event type = %q, want error", last.Type)
	}
}

// TestAnthropicAdapter_BearerClosureError asserts that a failure
// inside the bearer closure (e.g. a federation source whose STS
// exchange returned a 4xx) is surfaced synchronously by Stream
// without ever hitting the upstream API. Without this, a
// credential-layer failure would result in a half-built request that
// only error out after the network round-trip, masking the original
// cause behind a HTTP-shaped failure.
func TestAnthropicAdapter_BearerClosureError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("anthropic adapter should not have hit the network when the bearer closure errors")
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(erroringBearer("federation: STS returned 401"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected error from bearer closure failure")
	}
	if !strings.Contains(err.Error(), "STS returned 401") {
		t.Errorf("error should preserve closure cause, got: %v", err)
	}
}

func TestAnthropicAdapter_HasTimeout(t *testing.T) {
	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	if adapter.httpClient.Timeout == 0 {
		t.Error("HTTP client should have a non-zero timeout")
	}
	tr, ok := adapter.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout should be non-zero")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout should be non-zero")
	}
}

// providerHistogramFinder collects a named float64 histogram from a
// ManualReader-backed MeterProvider; used by the per-adapter metric tests.
func providerHistogramFinder(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Histogram[float64], bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect(): %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				return h, true
			}
		}
	}
	return metricdata.Histogram[float64]{}, false
}

// providerHistogramTotalCount sums DataPoint counts for a named histogram.
// Returns 0 when the histogram is absent.
func providerHistogramTotalCount(t *testing.T, reader *sdkmetric.ManualReader, name string) uint64 {
	t.Helper()
	h, ok := providerHistogramFinder(t, reader, name)
	if !ok {
		return 0
	}
	var total uint64
	for _, dp := range h.DataPoints {
		total += dp.Count
	}
	return total
}

func TestAnthropicAdapter_RecordsLatencyAndTTFB(t *testing.T) {
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hi"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch { // drain to allow goroutine to record latency
	}

	// Both histograms should record exactly one observation per Stream call.
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got != 1 {
		t.Errorf("provider_latency count = %d, want 1", got)
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"); got != 1 {
		t.Errorf("provider_ttfb count = %d, want 1", got)
	}

	// Confirm provider.type / provider.model attributes are set on the latency
	// histogram. We pick the first data point and verify both keys.
	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	if !ok || len(h.DataPoints) == 0 {
		t.Fatal("expected at least one provider_latency data point")
	}
	attrs := h.DataPoints[0].Attributes
	if v, ok := attrs.Value("provider.type"); !ok || v.AsString() != "anthropic" {
		t.Errorf("provider.type attr = %v ok=%v, want anthropic", v.AsString(), ok)
	}
	if v, ok := attrs.Value("provider.model"); !ok || v.AsString() != "claude-sonnet-4-6" {
		t.Errorf("provider.model attr = %v ok=%v, want claude-sonnet-4-6", v.AsString(), ok)
	}
}

// On HTTP error, latency must still be recorded (one sample) but TTFB must not
// fire because no stream events were observed.
func TestAnthropicAdapter_RecordsLatencyOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := NewAnthropicAdapter(staticBearer("bad-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Metrics = metrics

	if _, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	}); err == nil {
		t.Fatal("expected error")
	}

	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got != 1 {
		t.Errorf("provider_latency count = %d, want 1 (error path still records)", got)
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"); got != 0 {
		t.Errorf("provider_ttfb count = %d, want 0 (no stream observed)", got)
	}
}

// TestAnthropicAdapter_StaticKeyModeUsesXApiKey asserts that AuthModeAPIKey
// sends the credential in the x-api-key header and does NOT set
// Authorization. This pins the static-key code path against a future
// regression that would silently swap the header on every request.
// Together with TestAnthropicAdapter_WIFModeUsesAuthorizationBearer this
// captures the BLOCKING B2 contract from issue #117.
func TestAnthropicAdapter_StaticKeyModeUsesXApiKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-ant-api03-fake" {
			t.Errorf("x-api-key = %q, want sk-ant-api03-fake", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization should be empty in API key mode, got %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("sk-ant-api03-fake"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
}

// TestAnthropicAdapter_WIFModeUsesAuthorizationBearer asserts that
// AuthModeBearer sends the credential as Authorization: Bearer and
// does NOT set x-api-key. WIF OAuth access tokens (sk-ant-oat01-...)
// are rejected by Anthropic's /v1/messages endpoint when sent via
// x-api-key; this is the issue #117 BLOCKING B2 invariant — pinning
// the test prevents a future regression that would 401 every
// WIF-authenticated run.
func TestAnthropicAdapter_WIFModeUsesAuthorizationBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-fake" {
			t.Errorf("Authorization = %q, want Bearer sk-ant-oat01-fake", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key should be empty in WIF mode, got %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("sk-ant-oat01-fake"), AuthModeBearer)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
}

// TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI is a regression
// guard for issue #194 cross-provider data leakage: ContentBlock now
// carries a Gemini-private `thought_signature` field, and a multi-provider
// run (model router) can route history blocks produced by the Gemini
// adapter into an Anthropic request. The adapter must serialise messages
// through a local wire type that omits ThoughtSignature so Vertex's
// encrypted chain-of-thought blob never reaches Anthropic infrastructure.
//
// The assertion is structural: the marshalled request body must not
// contain the substring "thought_signature" anywhere, even when an input
// ContentBlock carries a populated value.
func TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	// Construct a message history that mirrors what the harness builds
	// after a Gemini 3.x turn that emitted a thoughtSignature: an
	// assistant message with a tool_use ContentBlock carrying the blob.
	const sig = "AY89SIGBLOB=="
	messages := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: "read it"},
			},
		},
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{
					Type:             "tool_use",
					ID:               "toolu_1",
					Name:             "read_file",
					Input:            json.RawMessage(`{"path":"main.go"}`),
					ThoughtSignature: sig,
				},
			},
		},
	}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		Messages:  messages,
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	if len(capturedBody) == 0 {
		t.Fatal("expected the Anthropic server to receive a request body")
	}
	bodyStr := string(capturedBody)
	if strings.Contains(bodyStr, "thought_signature") {
		t.Errorf("Anthropic request body contains \"thought_signature\" — Gemini-private state leaked to Anthropic API.\nbody = %s", bodyStr)
	}
	if strings.Contains(bodyStr, sig) {
		t.Errorf("Anthropic request body contains the signature value %q.\nbody = %s", sig, bodyStr)
	}
}

// fastRetryPolicy returns a RetryPolicy with millisecond-scale delays so
// retry tests run quickly without weakening the assertions (MaxAttempts,
// MaxDelay, WallClockBudget still bound the loop the same way production
// values would).
func fastRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: time.Second,
	}
}

// TestAnthropicAdapter_RetriesOn429ThenSucceeds pins SF1 (v0.1 core review):
// providerRetry is documented as harness-wide but was wired only into the
// openai-compatible adapter. Anthropic is the default provider, so a single
// 429 failing the turn outright was the highest-impact instance of the gap.
func TestAnthropicAdapter_RetriesOn429ThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.RetryPolicy = fastRetryPolicy()

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	collectEvents(t, ch)

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("server attempts: got %d, want 2 (one 429, one success)", got)
	}
}

// TestAnthropicAdapter_RetryDisabled_ExactlyOneAttempt asserts that a zero
// RetryPolicy (the adapter's default when the factory does not set the
// field, and what RetryPolicyFromConfig(nil) produces) makes exactly one
// attempt — DoWithRetry's documented behaviour for a zero policy.
func TestAnthropicAdapter_RetryDisabled_ExactlyOneAttempt(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	// adapter.RetryPolicy left at its zero value.

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected an error for a persistent 429 with retry disabled")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("server attempts: got %d, want 1", got)
	}
}

// TestAnthropicAdapter_StreamFailureAfterStartIsNotRetried asserts the
// streaming-safety boundary: DoWithRetry governs only the pre-stream
// request/response exchange. Once the server has returned 200 and the
// adapter has begun handing events to the caller, a mid-stream transport
// failure must surface as a terminal error event on the channel — never as
// a second HTTP request. Retrying after bytes have already reached the
// caller would risk duplicating partially-emitted output.
//
// The handler writes a 200 response with one SSE event, flushes it to the
// wire, then hijacks and closes the raw connection — an abrupt mid-chunk
// drop the chunked-transfer-encoding reader surfaces as a transport error,
// exactly the class DoWithRetry would retry if it occurred before any
// response had been received.
func TestAnthropicAdapter_StreamFailureAfterStartIsNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`))
		_, _ = fmt.Fprint(w, makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.RetryPolicy = fastRetryPolicy()

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	if len(events) == 0 {
		t.Fatal("expected at least the pre-drop text_delta event")
	}
	if last := events[len(events)-1]; last.Type != "error" {
		t.Errorf("expected a terminal error event after the mid-stream drop, got %+v", last)
	}

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("server attempts: got %d, want 1 — a mid-stream failure must not be retried", got)
	}
}
