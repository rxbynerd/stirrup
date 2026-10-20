package judge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// recordedRequest is what a test server saw.
type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
}

// stubServer replies with status and body to every request and records the
// last one.
func stubServer(t *testing.T, status int, reply string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.method, rec.path, rec.query, rec.header = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		rec.body = map[string]any{}
		_ = json.Unmarshal(raw, &rec.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// hangingServer never answers until the test ends.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

var testSchema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`)

const anthropicOKReply = `{
  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-served-1",
  "content": [{"type": "thinking", "thinking": "hmm"}, {"type": "text", "text": "{\"verdict\":"}, {"type": "text", "text": "\"pass\"}"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 321, "output_tokens": 17}
}`

func newTestAnthropic(t *testing.T, baseURL string) *anthropicClient {
	t.Helper()
	must := require.New(t)
	c, err := newAnthropicClient(&http.Client{Timeout: 5 * time.Second}, baseURL, "sk-test-key", "claude-requested")
	must.NoError(err)
	c.sleep = noSleep
	return c
}

// noSleep skips retry waits.
func noSleep(context.Context, time.Duration) error { return nil }

func TestAnthropicClient_RequestShape(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, rec := stubServer(t, 200, anthropicOKReply)
	temp := 0.0

	resp, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{
		System: "sys", User: "usr", Schema: testSchema, MaxTokens: 777, Temperature: &temp,
	})
	must.NoError(err)

	check.Equal(http.MethodPost, rec.method)
	check.Equal("/v1/messages", rec.path)
	check.Equal("sk-test-key", rec.header.Get("x-api-key"))
	check.Equal("2023-06-01", rec.header.Get("anthropic-version"))
	check.Equal("application/json", rec.header.Get("Content-Type"))
	check.Empty(rec.header.Get("Authorization"), "Authorization must not be sent to Anthropic")

	check.Equal("claude-requested", rec.body["model"])
	check.Equal("sys", rec.body["system"])
	check.InDelta(777, rec.body["max_tokens"], 0)

	msgs, _ := rec.body["messages"].([]any)
	must.Len(msgs, 1)
	msg, _ := msgs[0].(map[string]any)
	check.Equal("user", msg["role"])
	check.Equal("usr", msg["content"])

	check.InDelta(0, rec.body["temperature"], 0, "an explicit zero temperature is sent")

	cfg, _ := rec.body["output_config"].(map[string]any)
	format, _ := cfg["format"].(map[string]any)
	check.Equal("json_schema", format["type"])
	schemaJSON, err := json.Marshal(format["schema"])
	must.NoError(err)
	check.JSONEq(string(testSchema), string(schemaJSON))

	check.JSONEq(`{"verdict":"pass"}`, resp.Text)
	check.Equal("claude-served-1", resp.Model)
	check.Equal("end_turn", resp.StopReason)
	check.Equal(321, resp.InputTokens)
	check.Equal(17, resp.OutputTokens)
}

func TestAnthropicClient_OmitsTemperatureAndSchemaWhenUnset(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, rec := stubServer(t, 200, anthropicOKReply)

	_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{System: "s", User: "u", MaxTokens: 10})
	must.NoError(err)
	check.NotContains(rec.body, "temperature", "temperature is sent only when set")
	check.NotContains(rec.body, "output_config", "output_config is sent only when a schema is set")
}

func TestAnthropicClient_DefaultBaseURL(t *testing.T) {
	must := require.New(t)
	c, err := newAnthropicClient(&http.Client{Timeout: time.Second}, "", "k", "m")
	must.NoError(err)
	must.Equal("https://api.anthropic.com/v1/messages", c.endpoint)
}

func TestAnthropicClient_PreservesBasePathAndQuery(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, rec := stubServer(t, 200, anthropicOKReply)

	_, err := newTestAnthropic(t, srv.URL+"/proxy/?route=judge").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.NoError(err)
	check.Equal("/proxy/v1/messages", rec.path)
	check.Equal("route=judge", rec.query)
}

func TestAnthropicClient_PassesStopReasonsThrough(t *testing.T) {
	for _, reason := range []string{"max_tokens", "refusal"} {
		t.Run(reason, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			srv, _ := stubServer(t, 200, `{"model":"m","content":[],"stop_reason":"`+reason+`","usage":{}}`)
			resp, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.NoError(err)
			check.Equal(reason, resp.StopReason)
		})
	}
}

func TestAnthropicClient_HTTPErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 529} {
		t.Run("http "+strconv.Itoa(status), func(t *testing.T) {
			must := require.New(t)
			srv, _ := stubServer(t, status, `{"type":"error","error":{"type":"x","message":"boom"}}`)
			_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.ErrorContains(err, "HTTP "+strconv.Itoa(status))
			must.ErrorContains(err, "boom")
		})
	}
}

func TestAnthropicClient_BoundsErrorBody(t *testing.T) {
	must := require.New(t)
	srv, _ := stubServer(t, 500, strings.Repeat("x", 100_000))

	_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.Error(err)
	must.LessOrEqual(len(err.Error()), 2*maxErrorBodyBytes, "error body not bounded")
}

func TestAnthropicClient_MalformedSuccessBody(t *testing.T) {
	must := require.New(t)
	srv, _ := stubServer(t, 200, `<html>gateway</html>`)

	_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.ErrorContains(err, "decode response")
}

func TestAnthropicClient_Timeout(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := hangingServer(t)
	c, err := newAnthropicClient(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL+"?key=hunter2", "k", "m")
	must.NoError(err)

	start := time.Now()
	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.Error(err, "expected a timeout error")
	check.Less(time.Since(start), 3*time.Second, "timeout not honoured")
	check.NotContains(err.Error(), "hunter2", "error leaks the request URL")
	check.NotContains(err.Error(), srv.URL, "error leaks the request URL")
}

func TestAnthropicClient_ContextCancellation(t *testing.T) {
	must := require.New(t)
	srv := hangingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := newTestAnthropic(t, srv.URL).Complete(ctx, JudgeRequest{User: "u", MaxTokens: 1})
	must.Error(err, "expected a cancellation error")
}

const openaiOKReply = `{
  "id": "chatcmpl-1", "model": "gpt-served-1",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "{\"verdict\":\"fail\"}", "refusal": null}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 210, "completion_tokens": 9}
}`

func newTestOpenAI(t *testing.T, baseURL, key string) *openaiClient {
	t.Helper()
	must := require.New(t)
	c, err := newOpenAIClient(&http.Client{Timeout: 5 * time.Second}, baseURL, key, "gpt-requested")
	must.NoError(err)
	c.sleep = noSleep
	return c
}

func TestOpenAIClient_RequestShape(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, rec := stubServer(t, 200, openaiOKReply)
	temp := 0.3

	resp, err := newTestOpenAI(t, srv.URL+"/v1", "or-key").Complete(context.Background(), JudgeRequest{
		System: "sys", User: "usr", Schema: testSchema, MaxTokens: 555, Temperature: &temp,
	})
	must.NoError(err)

	check.Equal(http.MethodPost, rec.method)
	check.Equal("/v1/chat/completions", rec.path)
	check.Equal("Bearer or-key", rec.header.Get("Authorization"))
	check.Empty(rec.header.Get("x-api-key"), "x-api-key must not be sent to an OpenAI-compatible endpoint")

	check.Equal("gpt-requested", rec.body["model"])
	check.InDelta(555, rec.body["max_completion_tokens"], 0)
	check.NotContains(rec.body, "max_tokens", "legacy max_tokens must not be sent")
	check.InDelta(0.3, rec.body["temperature"], 1e-9)

	msgs, _ := rec.body["messages"].([]any)
	must.Len(msgs, 2)
	system, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	check.Equal("system", system["role"])
	check.Equal("sys", system["content"])
	check.Equal("user", user["role"])
	check.Equal("usr", user["content"])

	rf, _ := rec.body["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	check.Equal("json_schema", rf["type"])
	check.Equal("verdict", js["name"])
	strict, _ := js["strict"].(bool)
	check.True(strict, "json_schema must be strict")
	schemaJSON, err := json.Marshal(js["schema"])
	must.NoError(err)
	check.JSONEq(string(testSchema), string(schemaJSON))

	check.JSONEq(`{"verdict":"fail"}`, resp.Text)
	check.Equal("gpt-served-1", resp.Model)
	check.Equal("end_turn", resp.StopReason)
	check.Equal(210, resp.InputTokens)
	check.Equal(9, resp.OutputTokens)
}

func TestOpenAIClient_OmitsTemperatureAndSchemaWhenUnset(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, rec := stubServer(t, 200, openaiOKReply)

	_, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{System: "s", User: "u", MaxTokens: 10})
	must.NoError(err)
	check.NotContains(rec.body, "temperature", "temperature is sent only when set")
	check.NotContains(rec.body, "response_format", "response_format is sent only when a schema is set")
}

func TestOpenAIClient_SendsZeroTemperatureWhenExplicit(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, rec := stubServer(t, 200, openaiOKReply)
	zero := 0.0

	_, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 10, Temperature: &zero})
	must.NoError(err)
	check.InDelta(0, rec.body["temperature"], 0, "an explicit zero temperature is sent")
}

func TestOpenAIClient_NoAuthorizationHeaderWithoutKey(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, rec := stubServer(t, 200, openaiOKReply)

	_, err := newTestOpenAI(t, srv.URL, "").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 10})
	must.NoError(err)
	check.Empty(rec.header.Get("Authorization"), "no Authorization header without a key")
}

func TestOpenAIClient_RequiresBaseURL(t *testing.T) {
	must := require.New(t)
	_, err := newOpenAIClient(&http.Client{Timeout: time.Second}, "", "k", "m")
	must.Error(err, "expected an error for an empty base URL")
}

func TestOpenAIClient_MapsFinishReasons(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal", "tool_calls": "tool_calls"}
	for finish, want := range cases {
		t.Run(finish, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			srv, _ := stubServer(t, 200, `{"model":"m","choices":[{"message":{"content":"x"},"finish_reason":"`+finish+`"}],"usage":{}}`)
			resp, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.NoError(err)
			check.Equal(want, resp.StopReason)
		})
	}
}

func TestOpenAIClient_RefusalFieldOverridesFinishReason(t *testing.T) {
	must := require.New(t)
	srv, _ := stubServer(t, 200, `{"model":"m","choices":[{"message":{"content":null,"refusal":"I cannot help with that."},"finish_reason":"stop"}],"usage":{}}`)

	resp, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.NoError(err)
	must.Equal("refusal", resp.StopReason)
}

func TestOpenAIClient_ErrorsInSuccessfulResponses(t *testing.T) {
	cases := map[string]string{
		"error object": `{"error":{"message":"upstream exploded","code":502}}`,
		"no choices":   `{"model":"m","choices":[],"usage":{}}`,
		"not json":     `<html>`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			srv, _ := stubServer(t, 200, reply)
			_, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.Error(err)
		})
	}
}

func TestOpenAIClient_HTTPErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		t.Run("http "+strconv.Itoa(status), func(t *testing.T) {
			must := require.New(t)
			srv, _ := stubServer(t, status, `{"error":{"message":"boom"}}`)
			_, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.ErrorContains(err, "HTTP "+strconv.Itoa(status))
			must.ErrorContains(err, "boom")
		})
	}
}

func TestOpenAIClient_Timeout(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := hangingServer(t)
	c, err := newOpenAIClient(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL+"/v1?api-key=hunter2", "k", "m")
	must.NoError(err)

	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.Error(err, "expected a timeout error")
	check.NotContains(err.Error(), "hunter2", "error leaks the request URL")
	check.NotContains(err.Error(), srv.URL, "error leaks the request URL")
}

func TestClients_RedactTheKeyFromProviderText(t *testing.T) {
	const key = "sk-test-0123456789abcdef"
	echo := func(r *http.Request) string {
		return r.Header.Get("x-api-key") + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	cases := map[string]struct {
		status int
		reply  func(r *http.Request) string
	}{
		"error body": {http.StatusUnauthorized, func(r *http.Request) string {
			return "bad key\n" + echo(r) + "\r\nauth=" + r.Header.Get("Authorization")
		}},
		"key across truncation": {http.StatusBadRequest, func(r *http.Request) string { return strings.Repeat("x", maxErrorBodyBytes-12) + echo(r) }},
		"json-escaped key":      {http.StatusBadRequest, func(r *http.Request) string { b, _ := json.Marshal(echo(r)); return string(b) }},
		"error on HTTP 200":     {http.StatusOK, func(r *http.Request) string { return `{"error":{"message":"key ` + echo(r) + ` revoked\nretry"}}` }},
	}
	clients := map[string]func(url string) JudgeClient{
		"anthropic": func(url string) JudgeClient {
			c, _ := newAnthropicClient(&http.Client{Timeout: 5 * time.Second}, url, key, "m")
			return c
		},
		"openai": func(url string) JudgeClient {
			c, _ := newOpenAIClient(&http.Client{Timeout: 5 * time.Second}, url, key, "m")
			return c
		},
	}
	for name, tc := range cases {
		for client, build := range clients {
			if tc.status == http.StatusOK && client == "anthropic" {
				continue
			}
			t.Run(name+"/"+client, func(t *testing.T) {
				must := require.New(t)
				check := assert.New(t)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.reply(r))
				}))
				t.Cleanup(srv.Close)
				_, err := build(srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
				must.Error(err)
				msg := err.Error()
				check.NotContains(msg, key[:8], "error carries key material")
				check.NotContains(msg, "\n", "error spans several lines")
				check.NotContains(msg, "\r", "error spans several lines")
				check.LessOrEqual(len(msg), maxErrorBodyBytes+64, "error is not bounded near maxErrorBodyBytes")
			})
		}
	}
}

func TestRedactSecret_MasksRawAndEscapedOccurrences(t *testing.T) {
	const key = `sk-te"st\0123456789`
	cases := map[string]struct {
		in, secret, want string
	}{
		"raw":          {"key " + key + " end", key, "key [redacted] end"},
		"json escaped": {`key sk-te\"st\\0123456789 end`, key, "key [redacted] end"},
		"bearer":       {"Authorization: Bearer " + key, key, "Authorization: Bearer [redacted]"},
		"repeated":     {key + key, key, "[redacted][redacted]"},
		"short secret": {"the word secret", "secret", "the word secret"},
		"no secret":    {"anything", "", "anything"},
		"absent":       {"nothing here", key, "nothing here"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, redactSecret(tc.in, tc.secret))
		})
	}
}

func TestNewClient_DoesNotFollowRedirects(t *testing.T) {
	for _, provider := range []string{types.JudgeProviderAnthropic, types.JudgeProviderOpenAICompatible} {
		t.Run(provider, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var forwarded atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
			}))
			t.Cleanup(target.Close)
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL+"/stolen")
				w.WriteHeader(http.StatusTemporaryRedirect)
				_, _ = io.WriteString(w, `<a href="`+target.URL+`/stolen">Temporary Redirect</a>`)
			}))
			t.Cleanup(redirector.Close)

			c, err := NewClient(types.JudgeLLMConfig{Provider: provider, Model: "m", BaseURL: redirector.URL}, "sk-test-0123456789abcdef")
			must.NoError(err)
			_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.ErrorContains(err, "HTTP 307")
			check.NotContains(err.Error(), target.URL, "error names the redirect target")
			check.NotContains(err.Error(), "stolen", "error names the redirect target")
			check.Equal(int32(0), forwarded.Load(), "redirect target received requests")
		})
	}
}

// sequenceServer answers successive requests with the given statuses,
// repeating the last, and counts requests. A 200 gets okReply.
func sequenceServer(t *testing.T, okReply string, header http.Header, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		status := statuses[min(i, len(statuses)-1)]
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, okReply)
			return
		}
		_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// recordingSleep records each requested wait without waiting.
type recordingSleep struct {
	mu    sync.Mutex
	waits []time.Duration
	err   error
}

func (s *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, d)
	return s.err
}

func TestClients_RetryTransientFailures(t *testing.T) {
	type build func(t *testing.T, url string, sleep *recordingSleep, timeout time.Duration) JudgeClient
	clients := map[string]struct {
		ok    string
		build build
	}{
		"anthropic": {anthropicOKReply, func(t *testing.T, url string, sleep *recordingSleep, timeout time.Duration) JudgeClient {
			must := require.New(t)
			c, err := newAnthropicClient(&http.Client{Timeout: timeout}, url, "k", "m")
			must.NoError(err)
			c.sleep = sleep.sleep
			return c
		}},
		"openai": {openaiOKReply, func(t *testing.T, url string, sleep *recordingSleep, timeout time.Duration) JudgeClient {
			must := require.New(t)
			c, err := newOpenAIClient(&http.Client{Timeout: timeout}, url, "k", "m")
			must.NoError(err)
			c.sleep = sleep.sleep
			return c
		}},
	}
	retryAfter := func(v string) http.Header { return http.Header{"Retry-After": []string{v}} }
	cases := []struct {
		name         string
		statuses     []int
		header       http.Header
		timeout      time.Duration
		sleepErr     error
		wantRequests int32
		wantErr      string
		checkWaits   func(t *testing.T, waits []time.Duration)
	}{
		{
			name: "overloaded twice then ok", statuses: []int{529, 529, 200}, wantRequests: 3,
			checkWaits: func(t *testing.T, waits []time.Duration) {
				must := require.New(t)
				check := assert.New(t)
				must.Len(waits, 2)
				check.GreaterOrEqual(waits[0], 250*time.Millisecond, "first wait is jittered between 250ms and 500ms")
				check.LessOrEqual(waits[0], 500*time.Millisecond, "first wait is jittered between 250ms and 500ms")
				check.GreaterOrEqual(waits[1], 500*time.Millisecond, "second wait is jittered between 500ms and 1s")
				check.LessOrEqual(waits[1], time.Second, "second wait is jittered between 500ms and 1s")
			},
		},
		{
			name: "rate limited with retry-after seconds", statuses: []int{429, 200}, header: retryAfter("1"), wantRequests: 2,
			checkWaits: func(t *testing.T, waits []time.Duration) {
				check := assert.New(t)
				must := require.New(t)
				must.Len(waits, 1)
				check.Equal(time.Second, waits[0])
			},
		},
		{
			name: "retry-after http date", statuses: []int{503, 200}, header: retryAfter(time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)), wantRequests: 2,
			checkWaits: func(t *testing.T, waits []time.Duration) {
				must := require.New(t)
				check := assert.New(t)
				must.Len(waits, 1)
				check.GreaterOrEqual(waits[0], time.Second, "wait follows the Retry-After date")
				check.LessOrEqual(waits[0], 3*time.Second, "wait follows the Retry-After date")
			},
		},
		{name: "bad request is not retried", statuses: []int{400}, wantRequests: 1, wantErr: "HTTP 400"},
		{name: "unauthorised is not retried", statuses: []int{401}, wantRequests: 1, wantErr: "HTTP 401"},
		{name: "persistent outage stops at the attempt cap", statuses: []int{503}, wantRequests: 3, wantErr: "after 3 attempts"},
		{name: "retry-after beyond the deadline stops at once", statuses: []int{503}, header: retryAfter("30"), timeout: 2 * time.Second, wantRequests: 1, wantErr: "exceeds the remaining timeout"},
		{name: "cancelled wait stops", statuses: []int{503}, sleepErr: context.Canceled, wantRequests: 1, wantErr: "retry abandoned"},
	}
	for name, client := range clients {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				check := assert.New(t)
				must := require.New(t)
				srv, requests := sequenceServer(t, client.ok, tc.header, tc.statuses...)
				sleep := &recordingSleep{err: tc.sleepErr}
				timeout := tc.timeout
				if timeout == 0 {
					timeout = 10 * time.Second
				}
				_, err := client.build(t, srv.URL, sleep, timeout).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
				if tc.wantErr == "" {
					must.NoError(err)
				} else {
					must.ErrorContains(err, tc.wantErr)
				}
				check.Equal(tc.wantRequests, requests.Load())
				if tc.checkWaits != nil {
					tc.checkWaits(t, sleep.waits)
				}
			})
		}
	}
}

func TestPostJSON_DeadlineBoundsAllAttempts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, requests := sequenceServer(t, anthropicOKReply, nil, 503)
	c, err := newAnthropicClient(&http.Client{Timeout: 700 * time.Millisecond}, srv.URL, "k", "m")
	must.NoError(err)
	start := time.Now()
	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	must.Error(err)
	check.LessOrEqual(time.Since(start), 1500*time.Millisecond, "retries ran past a 700ms timeout")
	n := requests.Load()
	check.GreaterOrEqual(n, int32(1))
	check.LessOrEqual(n, int32(2), "the deadline stops retries before the attempt cap")
}

func TestParseRetryAfter_SecondsAndHTTPDates(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{name: "empty header", input: "", want: 0},
		{name: "delta seconds", input: "2", want: 2 * time.Second},
		{name: "delta seconds with surrounding spaces", input: " 5 ", want: 5 * time.Second},
		{name: "zero seconds", input: "0", want: 0},
		{name: "negative seconds", input: "-3", want: 0},
		{name: "non-numeric value", input: "soon", want: 0},
		{name: "huge delta seconds capped at an hour", input: "99999999999999", want: time.Hour},
		{name: "future http date", input: "Thu, 01 Oct 2026 12:00:10 GMT", want: 10 * time.Second},
		{name: "past http date", input: "Thu, 01 Oct 2026 11:59:00 GMT", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, parseRetryAfter(tc.input, now))
		})
	}
}

func TestBackoff_JitteredWithinExponentialBounds(t *testing.T) {
	cases := []struct {
		attempt  int
		min, max time.Duration
	}{
		{attempt: 1, min: 250 * time.Millisecond, max: 500 * time.Millisecond},
		{attempt: 2, min: 500 * time.Millisecond, max: time.Second},
	}
	for _, tc := range cases {
		t.Run("attempt "+strconv.Itoa(tc.attempt), func(t *testing.T) {
			must := require.New(t)
			seen := map[time.Duration]bool{}
			for range 200 {
				d := backoff(tc.attempt)
				must.GreaterOrEqual(d, tc.min)
				must.LessOrEqual(d, tc.max)
				seen[d] = true
			}
			must.Greater(len(seen), 1, "backoff is not jittered")
		})
	}
}

func TestClients_EnforcesResponseSizeLimit(t *testing.T) {
	huge := strings.Repeat("x", maxResponseBytes)
	builders := map[string]func(t *testing.T, url string) JudgeClient{
		"anthropic": func(t *testing.T, url string) JudgeClient { return newTestAnthropic(t, url) },
		"openai":    func(t *testing.T, url string) JudgeClient { return newTestOpenAI(t, url, "k") },
	}
	for name, build := range builders {
		t.Run(name+" rejects an oversized response", func(t *testing.T) {
			must := require.New(t)
			srv, _ := stubServer(t, 200, `{"model":"m","pad":"`+huge+`"}`)
			_, err := build(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			must.ErrorContains(err, "provider response exceeds")
		})
	}
	t.Run("accepts a response exactly at the cap", func(t *testing.T) {
		must := require.New(t)
		body := `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{},"pad":"`
		body += strings.Repeat("x", maxResponseBytes-len(body)-2) + `"}`
		srv, _ := stubServer(t, 200, body)
		_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
		must.NoError(err, "a %d-byte response was rejected", len(body))
	})
}
