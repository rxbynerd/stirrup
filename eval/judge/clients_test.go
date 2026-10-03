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
	c, err := newAnthropicClient(&http.Client{Timeout: 5 * time.Second}, baseURL, "sk-test-key", "claude-requested")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = noSleep
	return c
}

// noSleep skips retry waits.
func noSleep(context.Context, time.Duration) error { return nil }

func TestAnthropicClient_RequestShape(t *testing.T) {
	srv, rec := stubServer(t, 200, anthropicOKReply)
	temp := 0.0

	resp, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{
		System: "sys", User: "usr", Schema: testSchema, MaxTokens: 777, Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if rec.method != http.MethodPost || rec.path != "/v1/messages" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if got := rec.header.Get("x-api-key"); got != "sk-test-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := rec.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	if rec.header.Get("Authorization") != "" {
		t.Error("Authorization header must not be sent to Anthropic")
	}

	if rec.body["model"] != "claude-requested" || rec.body["system"] != "sys" || rec.body["max_tokens"] != float64(777) {
		t.Errorf("body = %v", rec.body)
	}
	msgs, _ := rec.body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "usr" {
		t.Errorf("messages = %v", rec.body["messages"])
	}
	if v, present := rec.body["temperature"]; !present || v != float64(0) {
		t.Errorf("temperature = %v (present %v), want an explicit 0", v, present)
	}
	cfg, _ := rec.body["output_config"].(map[string]any)
	format, _ := cfg["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("output_config.format = %v", cfg["format"])
	}
	var wantSchema any
	_ = json.Unmarshal(testSchema, &wantSchema)
	if !jsonEqual(t, wantSchema, format["schema"]) {
		t.Errorf("schema = %v", format["schema"])
	}

	if resp.Text != `{"verdict":"pass"}` || resp.Model != "claude-served-1" || resp.StopReason != "end_turn" ||
		resp.InputTokens != 321 || resp.OutputTokens != 17 {
		t.Errorf("response = %+v", resp)
	}
}

func TestAnthropicClient_OmitsTemperatureAndSchemaWhenUnset(t *testing.T) {
	srv, rec := stubServer(t, 200, anthropicOKReply)

	if _, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{System: "s", User: "u", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"temperature", "output_config"} {
		if _, present := rec.body[key]; present {
			t.Errorf("%s sent although unset: %v", key, rec.body[key])
		}
	}
}

func TestAnthropicClient_DefaultBaseURL(t *testing.T) {
	c, err := newAnthropicClient(&http.Client{Timeout: time.Second}, "", "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	if c.endpoint != "https://api.anthropic.com/v1/messages" {
		t.Errorf("endpoint = %q", c.endpoint)
	}
}

func TestAnthropicClient_PreservesBasePathAndQuery(t *testing.T) {
	srv, rec := stubServer(t, 200, anthropicOKReply)

	if _, err := newTestAnthropic(t, srv.URL+"/proxy/?route=judge").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/proxy/v1/messages" || rec.query != "route=judge" {
		t.Errorf("request = %s?%s", rec.path, rec.query)
	}
}

func TestAnthropicClient_PassesStopReasonsThrough(t *testing.T) {
	for _, reason := range []string{"max_tokens", "refusal"} {
		srv, _ := stubServer(t, 200, `{"model":"m","content":[],"stop_reason":"`+reason+`","usage":{}}`)
		resp, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StopReason != reason {
			t.Errorf("stop reason = %q, want %q", resp.StopReason, reason)
		}
	}
}

func TestAnthropicClient_HTTPErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 529} {
		srv, _ := stubServer(t, status, `{"type":"error","error":{"type":"x","message":"boom"}}`)
		_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
		if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) || !strings.Contains(err.Error(), "boom") {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestAnthropicClient_BoundsErrorBody(t *testing.T) {
	srv, _ := stubServer(t, 500, strings.Repeat("x", 100_000))

	_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err == nil || len(err.Error()) > 2*maxErrorBodyBytes {
		t.Errorf("error body not bounded: %d bytes", len(err.Error()))
	}
}

func TestAnthropicClient_MalformedSuccessBody(t *testing.T) {
	srv, _ := stubServer(t, 200, `<html>gateway</html>`)

	_, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("err = %v", err)
	}
}

func TestAnthropicClient_Timeout(t *testing.T) {
	srv := hangingServer(t)
	c, err := newAnthropicClient(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL+"?key=hunter2", "k", "m")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout not honoured: %v", time.Since(start))
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks the request URL: %v", err)
	}
}

func TestAnthropicClient_ContextCancellation(t *testing.T) {
	srv := hangingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	if _, err := newTestAnthropic(t, srv.URL).Complete(ctx, JudgeRequest{User: "u", MaxTokens: 1}); err == nil {
		t.Fatal("expected a cancellation error")
	}
}

const openaiOKReply = `{
  "id": "chatcmpl-1", "model": "gpt-served-1",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "{\"verdict\":\"fail\"}", "refusal": null}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 210, "completion_tokens": 9}
}`

func newTestOpenAI(t *testing.T, baseURL, key string) *openaiClient {
	t.Helper()
	c, err := newOpenAIClient(&http.Client{Timeout: 5 * time.Second}, baseURL, key, "gpt-requested")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = noSleep
	return c
}

func TestOpenAIClient_RequestShape(t *testing.T) {
	srv, rec := stubServer(t, 200, openaiOKReply)
	temp := 0.3

	resp, err := newTestOpenAI(t, srv.URL+"/v1", "or-key").Complete(context.Background(), JudgeRequest{
		System: "sys", User: "usr", Schema: testSchema, MaxTokens: 555, Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if rec.method != http.MethodPost || rec.path != "/v1/chat/completions" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer or-key" {
		t.Errorf("Authorization = %q", got)
	}
	if rec.header.Get("x-api-key") != "" {
		t.Error("x-api-key must not be sent to an OpenAI-compatible endpoint")
	}

	if rec.body["model"] != "gpt-requested" {
		t.Errorf("model = %v", rec.body["model"])
	}
	if rec.body["max_completion_tokens"] != float64(555) {
		t.Errorf("max_completion_tokens = %v", rec.body["max_completion_tokens"])
	}
	if _, legacy := rec.body["max_tokens"]; legacy {
		t.Error("legacy max_tokens must not be sent")
	}
	if rec.body["temperature"] != 0.3 {
		t.Errorf("temperature = %v", rec.body["temperature"])
	}
	msgs, _ := rec.body["messages"].([]any)
	if len(msgs) != 2 ||
		msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "sys" ||
		msgs[1].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["content"] != "usr" {
		t.Errorf("messages = %v", rec.body["messages"])
	}
	rf, _ := rec.body["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "verdict" || js["strict"] != true {
		t.Errorf("response_format = %v", rec.body["response_format"])
	}
	var want any
	_ = json.Unmarshal(testSchema, &want)
	if !jsonEqual(t, want, js["schema"]) {
		t.Errorf("schema = %v", js["schema"])
	}

	if resp.Text != `{"verdict":"fail"}` || resp.Model != "gpt-served-1" || resp.StopReason != "end_turn" ||
		resp.InputTokens != 210 || resp.OutputTokens != 9 {
		t.Errorf("response = %+v", resp)
	}
}

func TestOpenAIClient_OmitsTemperatureAndSchemaWhenUnset(t *testing.T) {
	srv, rec := stubServer(t, 200, openaiOKReply)

	if _, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{System: "s", User: "u", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"temperature", "response_format"} {
		if _, present := rec.body[key]; present {
			t.Errorf("%s sent although unset: %v", key, rec.body[key])
		}
	}
}

func TestOpenAIClient_SendsZeroTemperatureWhenExplicit(t *testing.T) {
	srv, rec := stubServer(t, 200, openaiOKReply)
	zero := 0.0

	if _, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 10, Temperature: &zero}); err != nil {
		t.Fatal(err)
	}
	if v, present := rec.body["temperature"]; !present || v != float64(0) {
		t.Errorf("temperature = %v (present %v), want an explicit 0", v, present)
	}
}

func TestOpenAIClient_NoAuthorizationHeaderWithoutKey(t *testing.T) {
	srv, rec := stubServer(t, 200, openaiOKReply)

	if _, err := newTestOpenAI(t, srv.URL, "").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if got := rec.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

func TestOpenAIClient_RequiresBaseURL(t *testing.T) {
	if _, err := newOpenAIClient(&http.Client{Timeout: time.Second}, "", "k", "m"); err == nil {
		t.Error("expected an error for an empty base URL")
	}
}

func TestOpenAIClient_MapsFinishReasons(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal", "tool_calls": "tool_calls"}
	for finish, want := range cases {
		srv, _ := stubServer(t, 200, `{"model":"m","choices":[{"message":{"content":"x"},"finish_reason":"`+finish+`"}],"usage":{}}`)
		resp, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StopReason != want {
			t.Errorf("finish_reason %q -> %q, want %q", finish, resp.StopReason, want)
		}
	}
}

func TestOpenAIClient_RefusalFieldOverridesFinishReason(t *testing.T) {
	srv, _ := stubServer(t, 200, `{"model":"m","choices":[{"message":{"content":null,"refusal":"I cannot help with that."},"finish_reason":"stop"}],"usage":{}}`)

	resp, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "refusal" {
		t.Errorf("stop reason = %q, want refusal", resp.StopReason)
	}
}

func TestOpenAIClient_ErrorsInSuccessfulResponses(t *testing.T) {
	cases := map[string]string{
		"error object": `{"error":{"message":"upstream exploded","code":502}}`,
		"no choices":   `{"model":"m","choices":[],"usage":{}}`,
		"not json":     `<html>`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := stubServer(t, 200, reply)
			if _, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestOpenAIClient_HTTPErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		srv, _ := stubServer(t, status, `{"error":{"message":"boom"}}`)
		_, err := newTestOpenAI(t, srv.URL, "k").Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
		if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) || !strings.Contains(err.Error(), "boom") {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestOpenAIClient_Timeout(t *testing.T) {
	srv := hangingServer(t)
	c, err := newOpenAIClient(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL+"/v1?api-key=hunter2", "k", "m")
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks the request URL: %v", err)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("marshal: %v %v", err1, err2)
	}
	return string(x) == string(y)
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
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.reply(r))
				}))
				t.Cleanup(srv.Close)
				_, err := build(srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
				if err == nil {
					t.Fatal("expected an error")
				}
				msg := err.Error()
				if strings.Contains(msg, key[:8]) {
					t.Errorf("error carries key material: %q", msg)
				}
				if strings.ContainsAny(msg, "\r\n") {
					t.Errorf("error spans several lines: %q", msg)
				}
				if len(msg) > maxErrorBodyBytes+64 {
					t.Errorf("error is %d bytes, want it bounded near %d", len(msg), maxErrorBodyBytes)
				}
			})
		}
	}
}

func TestRedactSecret(t *testing.T) {
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
		if got := redactSecret(tc.in, tc.secret); got != tc.want {
			t.Errorf("%s: redactSecret(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestNewClient_DoesNotFollowRedirects(t *testing.T) {
	for _, provider := range []string{types.JudgeProviderAnthropic, types.JudgeProviderOpenAICompatible} {
		t.Run(provider, func(t *testing.T) {
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
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
				t.Fatalf("err = %v, want an HTTP 307 error", err)
			}
			if strings.Contains(err.Error(), target.URL) || strings.Contains(err.Error(), "stolen") {
				t.Errorf("error names the redirect target: %v", err)
			}
			if n := forwarded.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, want 0", n)
			}
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
			c, err := newAnthropicClient(&http.Client{Timeout: timeout}, url, "k", "m")
			if err != nil {
				t.Fatal(err)
			}
			c.sleep = sleep.sleep
			return c
		}},
		"openai": {openaiOKReply, func(t *testing.T, url string, sleep *recordingSleep, timeout time.Duration) JudgeClient {
			c, err := newOpenAIClient(&http.Client{Timeout: timeout}, url, "k", "m")
			if err != nil {
				t.Fatal(err)
			}
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
				if len(waits) != 2 || waits[0] < 250*time.Millisecond || waits[0] > 500*time.Millisecond ||
					waits[1] < 500*time.Millisecond || waits[1] > time.Second {
					t.Errorf("waits = %v, want jittered 250-500ms then 500ms-1s", waits)
				}
			},
		},
		{
			name: "rate limited with retry-after seconds", statuses: []int{429, 200}, header: retryAfter("1"), wantRequests: 2,
			checkWaits: func(t *testing.T, waits []time.Duration) {
				if len(waits) != 1 || waits[0] != time.Second {
					t.Errorf("waits = %v, want [1s]", waits)
				}
			},
		},
		{
			name: "retry-after http date", statuses: []int{503, 200}, header: retryAfter(time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)), wantRequests: 2,
			checkWaits: func(t *testing.T, waits []time.Duration) {
				if len(waits) != 1 || waits[0] < time.Second || waits[0] > 3*time.Second {
					t.Errorf("waits = %v, want one wait of about 2-3s", waits)
				}
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
				srv, requests := sequenceServer(t, client.ok, tc.header, tc.statuses...)
				sleep := &recordingSleep{err: tc.sleepErr}
				timeout := tc.timeout
				if timeout == 0 {
					timeout = 10 * time.Second
				}
				_, err := client.build(t, srv.URL, sleep, timeout).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
				if tc.wantErr == "" && err != nil {
					t.Fatalf("Complete: %v", err)
				}
				if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if got := requests.Load(); got != tc.wantRequests {
					t.Errorf("server saw %d requests, want %d", got, tc.wantRequests)
				}
				if tc.checkWaits != nil {
					tc.checkWaits(t, sleep.waits)
				}
			})
		}
	}
}

func TestPostJSON_DeadlineBoundsAllAttempts(t *testing.T) {
	srv, requests := sequenceServer(t, anthropicOKReply, nil, 503)
	c, err := newAnthropicClient(&http.Client{Timeout: 700 * time.Millisecond}, srv.URL, "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("retries ran %s past a 700ms timeout", elapsed)
	}
	if n := requests.Load(); n < 1 || n > 2 {
		t.Errorf("server saw %d requests, want the deadline to stop retries before the attempt cap", n)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"2":                             2 * time.Second,
		" 5 ":                           5 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"99999999999999":                time.Hour,
		"Thu, 01 Oct 2026 12:00:10 GMT": 10 * time.Second,
		"Thu, 01 Oct 2026 11:59:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBackoffIsJitteredExponential(t *testing.T) {
	for attempt, bounds := range map[int][2]time.Duration{1: {250 * time.Millisecond, 500 * time.Millisecond}, 2: {500 * time.Millisecond, time.Second}} {
		seen := map[time.Duration]bool{}
		for range 200 {
			d := backoff(attempt)
			if d < bounds[0] || d > bounds[1] {
				t.Fatalf("backoff(%d) = %s, outside [%s, %s]", attempt, d, bounds[0], bounds[1])
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Errorf("backoff(%d) is not jittered", attempt)
		}
	}
}

func TestClients_RejectOversizedResponses(t *testing.T) {
	huge := strings.Repeat("x", maxResponseBytes)
	for name, build := range map[string]func(url string) JudgeClient{
		"anthropic": func(url string) JudgeClient { return newTestAnthropic(t, url) },
		"openai":    func(url string) JudgeClient { return newTestOpenAI(t, url, "k") },
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := stubServer(t, 200, `{"model":"m","pad":"`+huge+`"}`)
			_, err := build(srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1})
			if err == nil || !strings.Contains(err.Error(), "provider response exceeds") {
				t.Fatalf("err = %v, want the response-size error", err)
			}
		})
	}
	t.Run("exactly at the cap", func(t *testing.T) {
		body := `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{},"pad":"`
		body += strings.Repeat("x", maxResponseBytes-len(body)-2) + `"}`
		srv, _ := stubServer(t, 200, body)
		if _, err := newTestAnthropic(t, srv.URL).Complete(context.Background(), JudgeRequest{User: "u", MaxTokens: 1}); err != nil {
			t.Fatalf("a %d-byte response was rejected: %v", len(body), err)
		}
	})
}
