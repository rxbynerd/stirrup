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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
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

func TestAnthropicAdapter_Stream_EmitsTextDeltas(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":" world"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	// The handler runs on the server goroutine, so it reports with assert.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("test-key", r.Header.Get("x-api-key"), "x-api-key header")
		check.Equal(anthropicAPIVersion, r.Header.Get("anthropic-version"), "anthropic-version header")
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
	must.NoError(err)

	events := collectEvents(t, ch)

	must.Len(events, 3, "events: %+v", events)
	check.Equal("text_delta", events[0].Type)
	check.Equal("Hello", events[0].Text)
	check.Equal("text_delta", events[1].Type)
	check.Equal(" world", events[1].Text)
	check.Equal("message_complete", events[2].Type)
	check.Equal("end_turn", events[2].StopReason)
}

// TestAnthropicAdapter_LongSSELineRoundTrips guards against a regression to
// bufio.Scanner's default 64 KB per-line budget: an SSE data line larger
// than that must round-trip as ordinary content instead of aborting with
// bufio.ErrTooLong.
func TestAnthropicAdapter_LongSSELineRoundTrips(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	longText := strings.Repeat("a", 200*1024)
	encoded, err := json.Marshal(longText)
	must.NoError(err)

	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", fmt.Sprintf(`{"index":0,"delta":{"type":"text_delta","text":%s}}`, encoded)),
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

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	must.NoError(err)

	events := collectEvents(t, ch)

	must.Len(events, 2, "events: %+v", events)
	check.Equal("text_delta", events[0].Type)
	check.Len(events[0].Text, len(longText), "text delta length")
	check.Equal("message_complete", events[1].Type)
	check.Equal("end_turn", events[1].StopReason)
}

func TestAnthropicAdapter_Stream_EmitsToolCall(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)

	events := collectEvents(t, ch)

	must.Len(events, 2, "events: %+v", events)
	must.Equal("tool_call", events[0].Type)
	check.Equal("toolu_123", events[0].ID)
	check.Equal("read_file", events[0].Name)
	check.Equal("main.go", events[0].Input["path"])

	check.Equal("message_complete", events[1].Type)
	check.Equal("tool_use", events[1].StopReason)
}

func TestAnthropicAdapter_HTTPError_IncludesResponseBody(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "invalid x-api-key")
}

func TestAnthropicAdapter_HTTPError_IncludesStatusWithoutBody(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "500")
}

func TestAnthropicAdapter_HTTPError_TruncatesLargeBody(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.Error(err)
	// The body is truncated to 4096 bytes.
	errMsg := err.Error()
	check.LessOrEqual(len(errMsg), 4200, "error message should truncate the 8 KiB body")
	check.Contains(errMsg, "400")
}

// TestAnthropicAdapter_TransportError_DoesNotLeakCredentials drives the
// transport-error path with a credentialed baseURL pointed at a closed
// port. An operator may put credentials directly in a custom baseURL
// (userinfo or a gateway api_key query param); the *url.Error Go returns
// from Do embeds that URL and does not redact the query string, so the
// wrapped error must report only the dial-level cause.
func TestAnthropicAdapter_TransportError_DoesNotLeakCredentials(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	u, err := url.Parse(closed)
	must.NoError(err, "parse closed URL %q", closed)
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"

	adapter := NewAnthropicAdapter(staticBearer("key"), AuthModeAPIKey)
	adapter.baseURL = u.String()

	_, err = adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	must.Error(err, "dialing a closed port must fail")
	msg := err.Error()
	check.NotContains(msg, "supersecret", "transport error must not leak the query secret")
	check.NotContains(msg, "user:pass", "transport error must not leak userinfo")
}

func TestAnthropicAdapter_RequestBody_WireFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var received struct {
		Model        string          `json:"model"`
		System       json.RawMessage `json:"system"`
		MaxTokens    int             `json:"max_tokens"`
		CacheControl json.RawMessage `json:"cache_control"`
		Stream       bool            `json:"stream"`
	}

	// The handler runs on the server goroutine, so it reports with assert.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.NoError(json.NewDecoder(r.Body).Decode(&received), "decode request body")
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
		CacheKey:    "k",
	})
	must.NoError(err)

	for range ch {
	}

	check.True(received.Stream, "stream=true in request body")
	check.Equal("claude-sonnet-4-6", received.Model)
	check.JSONEq(`[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}]`, string(received.System))
	check.JSONEq(`{"type":"ephemeral"}`, string(received.CacheControl))
	check.Equal(4096, received.MaxTokens)
}

// TestAnthropicAdapter_TemperatureWireShape pins the unset-vs-explicit-zero
// semantics for StreamParams.Temperature: a nil pointer must omit the
// "temperature" key entirely, but an explicit Float64Ptr(0.0) must
// transmit "temperature":0.
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
			must := require.New(t)
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
			must.NoError(err)
			for range ch {
			}

			body := string(rawBody)
			if tc.wantTemperature {
				must.Contains(body, `"temperature"`, "non-nil temperature must be serialised")
			} else {
				must.NotContains(body, `"temperature"`, "nil temperature must be omitted")
			}
			if tc.wantTempSubstring != "" {
				must.Contains(body, tc.wantTempSubstring)
			}
		})
	}
}

// TestAnthropicAdapter_TemperatureSuppressedForNoSamplingParamsModels pins
// the model families that reject a non-default temperature outright
// (Claude Opus 4.7+, Sonnet 5, Fable 5 / Mythos 5 return a 400 rather than
// ignoring the field): the wire body must omit "temperature" entirely for
// these models. claude-sonnet-4-6 is the negative control.
func TestAnthropicAdapter_TemperatureSuppressedForNoSamplingParamsModels(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		wantOmitted bool
	}{
		{name: "opus 4.7 omits temperature", model: "claude-opus-4-7", wantOmitted: true},
		{name: "opus 4.8 omits temperature", model: "claude-opus-4-8", wantOmitted: true},
		{name: "sonnet 5 omits temperature", model: "claude-sonnet-5", wantOmitted: true},
		{name: "fable 5 omits temperature", model: "claude-fable-5", wantOmitted: true},
		{name: "mythos 5 omits temperature", model: "claude-mythos-5", wantOmitted: true},
		{name: "sonnet 4.6 keeps temperature", model: "claude-sonnet-4-6", wantOmitted: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
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
				Model:       tc.model,
				MaxTokens:   1024,
				Temperature: types.Float64Ptr(0.1),
			})
			must.NoError(err)
			for range ch {
			}

			if tc.wantOmitted {
				must.NotContains(string(rawBody), `"temperature"`, "OmitSamplingParams must suppress temperature")
			} else {
				must.Contains(string(rawBody), `"temperature"`, "temperature must not be suppressed for this model")
			}
		})
	}
}

// TestAnthropicAdapter_OmitSamplingParams_WarnsOnSuppressedTemperature
// asserts that when the quirk suppresses a caller-supplied non-nil
// Temperature, the warn log fires, names the suppressing rule, and never
// includes the suppressed value itself.
func TestAnthropicAdapter_OmitSamplingParams_WarnsOnSuppressedTemperature(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	// Strip the time attribute: the value-leak assertion below matches the
	// raw output for "0.5", and a timestamp's fractional seconds can
	// otherwise produce that substring.
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:       "claude-sonnet-5",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	})
	must.NoError(err)
	for range ch {
	}

	logOutput := buf.String()
	check.Contains(logOutput, "anthropic quirks suppressed caller temperature", "warn log message")
	check.Contains(logOutput, "Claude Sonnet 5", "warn log should name the suppressing rule")
	check.NotContains(logOutput, "0.5", "warn log must not leak the suppressed temperature")
}

// TestAnthropicAdapter_OmitSamplingParams_NoWarnWhenTemperatureUnset covers
// the double-negative case: a caller who never set Temperature at all, on
// a model in the omit family. The wire body must still omit "temperature",
// but unlike the WarnsOnSuppressedTemperature test, the WARN log must NOT
// fire since there is no caller-supplied value to warn about suppressing.
func TestAnthropicAdapter_OmitSamplingParams_NoWarnWhenTemperatureUnset(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	for range ch {
	}

	check.NotContains(string(rawBody), `"temperature"`, "nil temperature must be omitted on an omit-family model")
	check.NotContains(buf.String(), "anthropic quirks suppressed caller temperature", "no warn without a caller-supplied temperature")
}

// TestAnthropicAdapter_DebugLogListsAppliedRules pins the per-Stream debug
// line: it fires at the top of every Stream call and lists the
// descriptions of the rules that contributed to the resolution.
// claude-sonnet-4-6 is the useful negative (no omission rule fires).
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
			must := require.New(t)
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
			must.NoError(err)
			for range ch {
			}

			logOutput := buf.String()
			must.Contains(logOutput, "anthropic quirks resolved", "debug log message")
			for _, want := range tc.wantRuleSubstrings {
				must.Contains(logOutput, want, "debug log should list the applied rule")
			}
		})
	}
}

// TestAnthropicAdapter_DebugLog_PromptCacheUsage pins the per-stream cache
// health line: one record carrying the three usage counts when
// message_delta reports usage, and none when no input is reported.
func TestAnthropicAdapter_DebugLog_PromptCacheUsage(t *testing.T) {
	stop := makeSSE("message_stop", `{}`)
	cases := []struct {
		name   string
		stream string
		want   map[string]float64 // nil means "no record"
	}{
		{
			name:   "usage reported",
			stream: makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":4,"cache_creation_input_tokens":310,"cache_read_input_tokens":2162,"output_tokens":9}}`) + stop,
			want:   map[string]float64{"cache.read": 2162, "cache.write": 310, "input.uncached": 4},
		},
		{
			name:   "negative uncached input logs zero",
			stream: makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":-5,"cache_creation_input_tokens":310,"cache_read_input_tokens":2162,"output_tokens":9}}`) + stop,
			want:   map[string]float64{"cache.read": 2162, "cache.write": 310, "input.uncached": 0},
		},
		{
			name:   "no usage",
			stream: makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`) + stop,
		},
		{
			name:   "zero usage",
			stream: makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9}}`) + stop,
		},
		{
			name:   "message_start usage without message_delta",
			stream: makeSSE("message_start", `{"message":{"usage":{"input_tokens":4,"cache_creation_input_tokens":310,"cache_read_input_tokens":2162,"output_tokens":1}}}`) + stop,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
			adapter.baseURL = serveSSE(t, tc.stream).URL
			adapter.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 256})
			must.NoError(err)
			for range ch {
			}

			var records []map[string]any
			dec := json.NewDecoder(&buf)
			for dec.More() {
				var rec map[string]any
				must.NoError(dec.Decode(&rec), "decode log")
				if rec["msg"] == "anthropic prompt cache" {
					records = append(records, rec)
				}
			}
			if tc.want == nil {
				must.Empty(records, "prompt cache records")
				return
			}
			must.Len(records, 1, "prompt cache records")
			for key, want := range tc.want {
				must.InDelta(want, records[0][key], 0, "log field %s", key)
			}
			must.Equal("claude-sonnet-5-5", records[0]["provider.model"])
		})
	}
}

func TestSSE_ContentDelta_UnknownIndexIsSkipped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	events := collectEvents(t, ch)

	for _, ev := range events {
		check.NotEqual("error", ev.Type, "unexpected error event: %v", ev.Error)
		check.NotEqual("text_delta", ev.Type, "unexpected text_delta event for unknown index: %q", ev.Text)
	}
	must.Len(events, 1, "expected only message_complete: %+v", events)
	must.Equal("message_complete", events[0].Type)
}

func TestSSE_MalformedContentBlockStart_EmitsErrorEvent(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)

	events := collectEvents(t, ch)

	var errorEvents []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "error" {
			errorEvents = append(errorEvents, ev)
		}
	}
	must.NotEmpty(errorEvents, "expected an error event for malformed content_block_start JSON")
	for _, ev := range errorEvents {
		check.Error(ev.Error, "error event has nil Error field")
	}
}

func TestSSE_MalformedToolInput_MarksToolCall(t *testing.T) {
	must := require.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"read_file"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"INVALID"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`),
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
	must.NoError(err)

	events := collectEvents(t, ch)
	requireMarkedToolCall(t, events, "toolu_bad", "read_file", `{"path":INVALID`)
}

func TestSSE_MultipleToolBlocks_EmitSeparateToolCalls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	events := collectEvents(t, ch)

	var toolCalls []types.StreamEvent
	for _, ev := range events {
		must.NotEqual("error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}

	must.Len(toolCalls, 2, "expected 2 tool_call events")

	check.Equal("toolu_aaa", toolCalls[0].ID)
	check.Equal("read_file", toolCalls[0].Name)
	check.Equal("a.go", toolCalls[0].Input["path"])

	check.Equal("toolu_bbb", toolCalls[1].ID)
	check.Equal("write_file", toolCalls[1].Name)
	check.Equal("b.go", toolCalls[1].Input["path"])
}

func TestAnthropicAdapter_ContextCancel_EndsWithErrorEvent(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

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
	must.NoError(err)

	cancel()

	events := collectEvents(t, ch)

	must.NotEmpty(events, "expected at least one event after cancellation")
	last := events[len(events)-1]
	must.Equal("error", last.Type)
}

// TestAnthropicAdapter_BearerError_SkipsRequest asserts that a failure inside
// the bearer closure (e.g. a federation source whose STS exchange
// returned a 4xx) is surfaced synchronously by Stream without ever
// hitting the upstream API.
func TestAnthropicAdapter_BearerError_SkipsRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("anthropic adapter should not have hit the network when the bearer closure errors")
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(erroringBearer("federation: STS returned 401"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
	})
	must.Error(err, "expected error from bearer closure failure")
	must.ErrorContains(err, "STS returned 401", "error should preserve closure cause")
}

func TestAnthropicAdapter_StreamingClient_Timeouts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	check.Zero(adapter.httpClient.Timeout, "a total deadline cuts long streams, which idleTimeoutBody bounds instead")
	tr, ok := adapter.httpClient.Transport.(*http.Transport)
	must.True(ok, "expected *http.Transport")
	check.NotZero(tr.TLSHandshakeTimeout, "TLSHandshakeTimeout should be non-zero")
	check.NotZero(tr.ResponseHeaderTimeout, "ResponseHeaderTimeout should be non-zero")
}

// providerHistogramFinder collects a named float64 histogram from a
// ManualReader-backed MeterProvider; used by the per-adapter metric tests.
func providerHistogramFinder(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Histogram[float64], bool) {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm), "Collect()")
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

func TestAnthropicAdapter_Metrics_RecordsLatencyAndTTFB(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err, "NewMetricsForTesting")

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	must.NoError(err)
	for range ch {
	}

	check.Equal(uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	check.Equal(uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")

	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	must.True(ok, "provider_latency histogram is missing")
	must.NotEmpty(h.DataPoints, "expected at least one provider_latency data point")
	attrs := h.DataPoints[0].Attributes
	v, ok := attrs.Value("provider.type")
	check.True(ok, "provider.type attribute is missing")
	check.Equal("anthropic", v.AsString(), "provider.type attribute")
	v, ok = attrs.Value("provider.model")
	check.True(ok, "provider.model attribute is missing")
	check.Equal("claude-sonnet-4-6", v.AsString(), "provider.model attribute")

	check.Equal(uint64(1), providerHistogramTotalCount(t, reader, "gen_ai.client.operation.time_to_first_chunk"), "time_to_first_chunk count")
	ttfc, _ := providerHistogramFinder(t, reader, "gen_ai.client.operation.time_to_first_chunk")
	genAIAttrs := ttfc.DataPoints[0].Attributes
	wantHost, wantPort := observability.ServerEndpointFromURL(srv.URL)
	for key, want := range map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.request.model":  "claude-sonnet-4-6",
		"server.address":        wantHost,
	} {
		v, ok := genAIAttrs.Value(attribute.Key(key))
		check.True(ok, "time_to_first_chunk %s is missing", key)
		check.Equal(want, v.AsString(), "time_to_first_chunk %s", key)
	}
	v, ok = genAIAttrs.Value("server.port")
	check.True(ok, "time_to_first_chunk server.port is missing")
	check.Equal(int64(wantPort), v.AsInt64(), "time_to_first_chunk server.port")
}

// On HTTP error, latency must still be recorded (one sample) but TTFB must not
// fire because no stream events were observed.
func TestAnthropicAdapter_Metrics_RecordsLatencyOnHTTPError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	must.NoError(err, "NewMetricsForTesting")

	adapter := NewAnthropicAdapter(staticBearer("bad-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	adapter.Metrics = metrics

	_, err = adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	must.Error(err)

	check.Equal(uint64(1), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count (error path still records)")
	check.Equal(uint64(0), providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count (no stream observed)")
}

// TestAnthropicAdapter_APIKeyMode_UsesXApiKeyHeader asserts that AuthModeAPIKey
// sends the credential in the x-api-key header and does NOT set
// Authorization.
func TestAnthropicAdapter_APIKeyMode_UsesXApiKeyHeader(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("sk-ant-api03-fake", r.Header.Get("x-api-key"), "x-api-key header")
		check.Empty(r.Header.Get("Authorization"), "Authorization should be empty in API key mode")
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
	must.NoError(err)
	for range ch {
	}
}

// TestAnthropicAdapter_BearerMode_UsesAuthorizationHeader asserts that
// AuthModeBearer sends the credential as Authorization: Bearer and does
// NOT set x-api-key. WIF OAuth access tokens (sk-ant-oat01-...) are
// rejected by Anthropic's /v1/messages endpoint when sent via x-api-key.
func TestAnthropicAdapter_BearerMode_UsesAuthorizationHeader(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("Bearer sk-ant-oat01-fake", r.Header.Get("Authorization"), "Authorization header")
		check.Empty(r.Header.Get("x-api-key"), "x-api-key should be empty in WIF mode")
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
	must.NoError(err)
	for range ch {
	}
}

// TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI is a regression
// guard for cross-provider data leakage: ContentBlock carries a
// Gemini-private `thought_signature` field, and a multi-provider run
// (model router) can route history blocks produced by the Gemini adapter
// into an Anthropic request. The adapter must serialise messages through
// a local wire type that omits ThoughtSignature so Vertex's encrypted
// chain-of-thought blob never reaches Anthropic infrastructure.
func TestAnthropic_ThoughtSignatureNotLeakedToAnthropicAPI(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	for range ch {
	}

	must.NotEmpty(capturedBody, "expected the Anthropic server to receive a request body")
	bodyStr := string(capturedBody)
	check.NotContains(bodyStr, "thought_signature", "Gemini-private state must not reach the Anthropic API")
	check.NotContains(bodyStr, sig, "the thought signature value must not reach the Anthropic API")
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

// TestAnthropicAdapter_Retry_SucceedsAfter429 asserts providerRetry
// applies on the Anthropic adapter, not just openai-compatible.
func TestAnthropicAdapter_Retry_SucceedsAfter429(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	collectEvents(t, ch)

	must.Equal(int32(2), atomic.LoadInt32(&attempts), "server attempts: one 429, then one success")
}

// TestAnthropicAdapter_RetryDisabled_ExactlyOneAttempt asserts that a zero
// RetryPolicy (the adapter's default when the factory does not set the
// field, and what RetryPolicyFromConfig(nil) produces) makes exactly one
// attempt — DoWithRetry's documented behaviour for a zero policy.
func TestAnthropicAdapter_RetryDisabled_ExactlyOneAttempt(t *testing.T) {
	must := require.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
	})
	must.Error(err, "a persistent 429 with retry disabled must fail")
	must.Equal(int32(1), atomic.LoadInt32(&attempts), "server attempts")
}

// TestAnthropicAdapter_StreamFailureAfterStartIsNotRetried asserts the
// streaming-safety boundary: DoWithRetry governs only the pre-stream
// request/response exchange. Once the server has returned 200 and the
// adapter has begun handing events to the caller, a mid-stream transport
// failure must surface as a terminal error event on the channel, never as
// a second HTTP request (which would risk duplicating partial output).
// The handler simulates this by hijacking and closing the connection
// mid-stream after flushing one SSE event.
func TestAnthropicAdapter_StreamFailureAfterStartIsNotRetried(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
		// Runs on the server goroutine, so failures use assert rather than require.
		if !check.True(ok, "ResponseWriter does not support Hijack") {
			return
		}
		conn, _, err := hj.Hijack()
		if !check.NoError(err, "hijack") {
			return
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
	must.NoError(err)

	events := collectEvents(t, ch)
	must.NotEmpty(events, "expected at least the pre-drop text_delta event")
	last := events[len(events)-1]
	check.Equal("error", last.Type, "terminal event after the mid-stream drop: %+v", last)

	must.Equal(int32(1), atomic.LoadInt32(&attempts), "a mid-stream failure must not be retried")
}

// streamAnthropicSSE serves body as a 200 SSE response and returns every
// StreamEvent the adapter emits for it.
func streamAnthropicSSE(t *testing.T, body string) []types.StreamEvent {
	t.Helper()
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-sonnet-5-5",
		MaxTokens: 1024,
	})
	must.NoError(err)
	return collectEvents(t, ch)
}

func TestSSE_MessageDeltaRefusal_CarriesStopDetails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"I can't help with that."}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"This request was declined because it conflicts with Anthropic's Usage Policy.","recommended_model":null}},"usage":{"output_tokens":12}}`),
		makeSSE("message_stop", `{"type":"message_stop"}`),
	))

	last := events[len(events)-1]
	must.Equal("message_complete", last.Type)
	must.Equal("refusal", last.StopReason)
	want := types.StopDetails{
		Type:        "refusal",
		Category:    "cyber",
		Explanation: "This request was declined because it conflicts with Anthropic's Usage Policy.",
	}
	must.NotNil(last.StopDetails)
	check.Equal(want, *last.StopDetails)
	check.Equal(12, last.OutputTokens)
}

func TestSSE_MessageDeltaRefusal_NullCategory(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":null,"explanation":null}}}`),
		makeSSE("message_stop", `{"type":"message_stop"}`),
	))

	last := events[len(events)-1]
	must.NotNil(last.StopDetails)
	must.Equal(types.StopDetails{Type: "refusal"}, *last.StopDetails)
}

func TestSSE_MessageDeltaStopDetails_AbsentOrUnparseable(t *testing.T) {
	cases := map[string]string{
		"null":       `{"delta":{"stop_reason":"end_turn","stop_details":null}}`,
		"absent":     `{"delta":{"stop_reason":"end_turn"}}`,
		"empty":      `{"delta":{"stop_reason":"end_turn","stop_details":{}}}`,
		"not object": `{"delta":{"stop_reason":"end_turn","stop_details":"refusal"}}`,
	}
	for name, delta := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			events := streamAnthropicSSE(t, joinLines(
				makeSSE("message_delta", delta),
				makeSSE("message_stop", `{}`),
			))
			must.Len(events, 1, "events: %+v", events)
			must.Equal("message_complete", events[0].Type)
			must.Equal("end_turn", events[0].StopReason)
			must.Nil(events[0].StopDetails)
		})
	}
}

// TestSSE_MessageDeltaStopDetails_WrongShapeKeepsValidFields pins that a
// malformed field degrades on its own: the refusal type survives a
// category of the wrong JSON type.
func TestSSE_MessageDeltaStopDetails_WrongShapeKeepsValidFields(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("message_delta", `{"delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":{"nested":true},"explanation":42}}}`),
		makeSSE("message_stop", `{}`),
	))
	last := events[len(events)-1]
	must.NotNil(last.StopDetails)
	must.Equal(types.StopDetails{Type: "refusal", Category: "other"}, *last.StopDetails)
}

func TestParseAnthropicStopDetails_BoundsAndCategories(t *testing.T) {
	longExplanation := strings.Repeat("a", maxStopDetailsExplanationBytes-1) + "é" + "tail"
	cases := []struct {
		name string
		raw  string
		want types.StopDetails
	}{
		{
			name: "documented category kept",
			raw:  `{"type":"refusal","category":"reasoning_extraction"}`,
			want: types.StopDetails{Type: "refusal", Category: "reasoning_extraction"},
		},
		{
			name: "undocumented category is other",
			raw:  `{"type":"refusal","category":"new_policy_area"}`,
			want: types.StopDetails{Type: "refusal", Category: "other"},
		},
		{
			name: "numeric category is other",
			raw:  `{"type":"refusal","category":7}`,
			want: types.StopDetails{Type: "refusal", Category: "other"},
		},
		{
			name: "empty category stays empty",
			raw:  `{"type":"refusal","category":""}`,
			want: types.StopDetails{Type: "refusal"},
		},
		{
			name: "type capped",
			raw:  `{"type":"` + strings.Repeat("t", 100) + `"}`,
			want: types.StopDetails{Type: strings.Repeat("t", maxStopDetailsTypeBytes)},
		},
		{
			name: "explanation capped on a rune boundary",
			raw:  `{"type":"refusal","explanation":"` + longExplanation + `"}`,
			want: types.StopDetails{Type: "refusal", Explanation: strings.Repeat("a", maxStopDetailsExplanationBytes-1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := parseAnthropicStopDetails(json.RawMessage(tc.raw))
			must.NotNil(got)
			must.Equal(tc.want, *got)
			must.True(utf8.ValidString(got.Explanation), "explanation %q is not valid UTF-8", got.Explanation)
		})
	}
}

func TestSSE_ErrorEventSurfacesErrorType(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		makeSSE("error", `{"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	))

	must.Len(events, 2, "events: %+v", events)
	check.Equal("text_delta", events[0].Type)
	last := events[1]
	must.Equal("error", last.Type)
	must.EqualError(last.Error, "anthropic API stream error (overloaded_error): Overloaded")
}

func TestSSE_ErrorEventBeforeContent(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("error", `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`),
		makeSSE("message_stop", `{}`),
	))
	must.Len(events, 1, "events: %+v", events)
	must.Equal("error", events[0].Type)
	must.EqualError(events[0].Error, "anthropic API stream error (api_error): Internal server error")
}

func TestSSE_TruncatedErrorEventData(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		makeSSE("error", `{"type":"error","error":{"type":"overloaded_er`),
	))
	last := events[len(events)-1]
	must.Equal("error", last.Type)
	must.EqualError(last.Error, "anthropic API stream error")
}

func TestAnthropicStreamError_DegenerateBodies(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{name: "error message", data: `{"error":{"message":"boom"}}`, want: "anthropic API stream error: boom"},
		{name: "empty object", data: `{}`, want: "anthropic API stream error"},
		{name: "not json", data: `not json`, want: "anthropic API stream error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, anthropicStreamError(tc.data).Error())
		})
	}
}
