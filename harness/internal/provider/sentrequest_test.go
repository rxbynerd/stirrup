package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestSentRequest_TemperatureMatchesWireBody(t *testing.T) {
	anthropic := func(url string) ProviderAdapter {
		a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
		a.baseURL = url
		return a
	}
	openaiCompatible := func(url string) ProviderAdapter {
		return NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
	}
	responses := func(url string) ProviderAdapter {
		return NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
	}
	gemini := func(url string) ProviderAdapter {
		return newGeminiTestAdapter(url, &stubTokenSource{token: "t"})
	}
	anthropicStream := makeSSE("message_stop", `{}`)
	openaiStream := "data: [DONE]\n\n"
	responsesStream := makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`)
	geminiStream := makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	cases := []struct {
		name     string
		model    string
		adapter  func(url string) ProviderAdapter
		response string
		want     bool
	}{
		{"anthropic keeps temperature", "claude-sonnet-4-6", anthropic, anthropicStream, true},
		{"anthropic omits it for a no-sampling model", "claude-opus-4-7", anthropic, anthropicStream, false},
		{"openai-compatible keeps temperature", "gpt-4o", openaiCompatible, openaiStream, true},
		{"openai-compatible omits it for a no-sampling model", "o1-mini", openaiCompatible, openaiStream, false},
		{"openai-responses keeps temperature", "gpt-4.1", responses, responsesStream, true},
		{"openai-responses omits it for a no-sampling model", "gpt-5.5", responses, responsesStream, false},
		{"gemini keeps temperature", "gemini-2.5-pro", gemini, geminiStream, true},
		{"gemini omits it for a no-sampling model", "gemini-3.7-flash", gemini, geminiStream, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var rawBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()

			ctx, sent := WithSentRequest(context.Background())
			ch, err := tc.adapter(srv.URL).Stream(ctx, types.StreamParams{
				Model:       tc.model,
				MaxTokens:   1024,
				Temperature: types.Float64Ptr(0.3),
			})
			must.NoError(err)
			for range ch {
			}

			check.Equal(tc.want, strings.Contains(string(rawBody), `"temperature"`), "wire body: %s", rawBody)
			if !tc.want {
				check.Nil(sent.Temperature(), "adapter reported a temperature the body omits")
				return
			}
			must.NotNil(sent.Temperature(), "adapter reported no temperature")
			check.InDelta(0.3, *sent.Temperature(), 1e-9)
		})
	}
}

func TestSentRequest_UnsetTemperatureReportsNothing(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()
	adapter := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ctx, sent := WithSentRequest(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	must.NoError(err)
	for range ch {
	}
	must.Nil(sent.Temperature())
}

func TestSentRequest_ZeroTemperatureIsReported(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeSSE("message_stop", `{}`))
	}))
	defer srv.Close()
	adapter := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ctx, sent := WithSentRequest(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024, Temperature: types.Float64Ptr(0)})
	must.NoError(err)
	for range ch {
	}
	must.NotNil(sent.Temperature())
	must.InDelta(0.0, *sent.Temperature(), 0)
}

func TestSentRequest_BedrockReportsTemperatureOnlyWhenSent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		want  bool
	}{
		{name: "keeps temperature", model: "anthropic.claude-sonnet-4-6-v1", want: true},
		{name: "omits it for a no-sampling model", model: "us.anthropic.claude-sonnet-5-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			client := &mockBedrockClient{apiErr: fmt.Errorf("stop after build")}
			adapter := &BedrockAdapter{client: client}

			ctx, sent := WithSentRequest(context.Background())
			_, err := adapter.Stream(ctx, types.StreamParams{
				Model:       tc.model,
				MaxTokens:   1024,
				Temperature: types.Float64Ptr(0.3),
			})
			must.Error(err)
			if !tc.want {
				must.Nil(sent.Temperature())
				return
			}
			must.NotNil(sent.Temperature())
			must.InDelta(0.3, *sent.Temperature(), 1e-9)
		})
	}
}

func TestSentRequest_AbsentSinkIsHarmless(t *testing.T) {
	must := require.New(t)
	must.NotPanics(func() { recordSentTemperature(context.Background(), types.Float64Ptr(0.5)) })
}
