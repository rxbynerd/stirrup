package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

type overflowCase struct {
	name   string
	status int
	body   string
	want   bool
}

// authAndUnrelatedCases are error responses no adapter may classify as a
// context overflow.
var authAndUnrelatedCases = []overflowCase{
	{"authentication error", http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, false},
	{"unrelated 400", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`, false},
	{"non-JSON 400", http.StatusBadRequest, `<html>bad request</html>`, false},
}

func errorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireContextOverflow(t *testing.T, err error, want bool) {
	t.Helper()
	must := require.New(t)
	must.Error(err, "Stream returned no error")
	must.Equal(want, types.IsContextOverflow(err), "IsContextOverflow(%q)", err)
}

func TestAnthropicAdapter_ClassifiesContextOverflow(t *testing.T) {
	cases := append([]overflowCase{
		{"prompt too long", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`, true},
		{"input and max_tokens exceed the limit", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 188240 + 21333 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`, true},
		{"recorded claude-haiku-4-5 prompt too long", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 457447 tokens > 200000 maximum"},"request_id":"req_011CfsqkTS5mDXo1yzAaRVJM"}`, true},
		{"recorded claude-haiku-4-5 max_tokens above the model limit", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 250000 > 64000, which is the maximum allowed number of output tokens for claude-haiku-4-5-20251001"},"request_id":"req_011Cfsr8H2dTCkcDgc7oFREJ"}`, false},
		{"prompt too long with another type", http.StatusBadRequest, `{"type":"error","error":{"type":"api_error","message":"prompt is too long"}}`, false},
		{"request body too large", http.StatusRequestEntityTooLarge, `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`, false},
	}, authAndUnrelatedCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewAnthropicAdapter(staticBearer("key"), AuthModeAPIKey)
			adapter.baseURL = errorServer(t, tc.status, tc.body).URL
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
			requireContextOverflow(t, err, tc.want)
		})
	}
}

func TestOpenAIAdapter_ClassifiesContextOverflow(t *testing.T) {
	cases := append([]overflowCase{
		{"context_length_exceeded code", http.StatusBadRequest, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, true},
		{"compatible server message without a code", http.StatusBadRequest, `{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens.","type":"BadRequestError","code":400}}`, true},
		{"code on a non-400 status", http.StatusInternalServerError, `{"error":{"message":"upstream failed","code":"context_length_exceeded"}}`, false},
	}, authAndUnrelatedCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewOpenAICompatibleAdapter(staticBearer("key"), errorServer(t, tc.status, tc.body).URL, OpenAIAuthConfig{}, RetryPolicy{})
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
			requireContextOverflow(t, err, tc.want)
		})
	}
}

func TestOpenAIResponsesAdapter_ClassifiesContextOverflow(t *testing.T) {
	cases := append([]overflowCase{
		{"context_length_exceeded code", http.StatusBadRequest, `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`, true},
		{"context window message without a code", http.StatusBadRequest, `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":null}}`, true},
		{"code on a non-400 status", http.StatusInternalServerError, `{"error":{"message":"upstream failed","code":"context_length_exceeded"}}`, false},
	}, authAndUnrelatedCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewOpenAIResponsesAdapter(staticBearer("key"), errorServer(t, tc.status, tc.body).URL, OpenAIAuthConfig{})
			adapter.RetryPolicy = RetryPolicy{MaxAttempts: 1}
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
			requireContextOverflow(t, err, tc.want)
		})
	}
}

func TestOpenAIResponsesAdapter_ClassifiesStreamedContextOverflow(t *testing.T) {
	cases := []struct {
		name  string
		event string
		data  string
		want  bool
	}{
		{"failed response", "response.failed", `{"response":{"status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`, true},
		{"error event", "error", `{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`, true},
		{"unrelated failure", "response.failed", `{"response":{"status":"failed","error":{"code":"server_error","message":"server overloaded"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			last := streamResponsesLastEvent(t, makeResponsesEvent(tc.event, tc.data))
			must.Equal("error", last.Type, "last event = %+v", last)
			must.Error(last.Error, "last event = %+v", last)
			must.Equal(tc.want, types.IsContextOverflow(last.Error), "IsContextOverflow(%q)", last.Error)
		})
	}
}

func TestGeminiAdapter_ClassifiesContextOverflow(t *testing.T) {
	cases := append([]overflowCase{
		{"input token count over the limit", http.StatusBadRequest, `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, true},
		{"unrelated invalid argument", http.StatusBadRequest, `{"error":{"code":400,"message":"Please use a valid role: user, model.","status":"INVALID_ARGUMENT"}}`, false},
		{"permission denied", http.StatusForbidden, `{"error":{"code":403,"message":"Permission denied on resource project.","status":"PERMISSION_DENIED"}}`, false},
	}, authAndUnrelatedCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newGeminiTestAdapter(errorServer(t, tc.status, tc.body).URL, &stubTokenSource{token: "tok"})
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
			requireContextOverflow(t, err, tc.want)
		})
	}
}

func TestBedrockAdapter_ClassifiesContextOverflow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"input too long", &brtypes.ValidationException{Message: aws.String("Input is too long for requested model.")}, true},
		{"anthropic prompt too long", &brtypes.ValidationException{Message: aws.String("prompt is too long: 208310 tokens > 200000 maximum")}, true},
		{"unrelated validation error", &brtypes.ValidationException{Message: aws.String("Malformed input request: extraneous key [foo] is not permitted")}, false},
		{"access denied", &brtypes.AccessDeniedException{Message: aws.String("You don't have access to the model with the specified model ID.")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &BedrockAdapter{client: &mockBedrockClient{apiErr: tc.err}}
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "anthropic.claude-sonnet-4-6-v1", MaxTokens: 1024})
			requireContextOverflow(t, err, tc.want)
		})
	}

	t.Run("in-stream validation error", func(t *testing.T) {
		must := require.New(t)
		ch := make(chan types.StreamEvent, 8)
		reader := newMockEventReader(nil, &brtypes.ValidationException{Message: aws.String("Input is too long for requested model.")})
		go consumeBedrockStream(context.Background(), reader, ch)
		events := collectEvents(t, ch)
		must.NotEmpty(events, "stream produced no events")
		last := events[len(events)-1]
		must.Equal("error", last.Type, "last event = %+v", last)
		must.True(types.IsContextOverflow(last.Error), "last event = %+v", last)
	})
}
