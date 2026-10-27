package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// geminiRecordingServer answers every request with body and records the
// request bodies it received.
func geminiRecordingServer(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	check := assert.New(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// The part layout mirrors a live gemini-3.8-flash tool-calling turn with
// includeThoughts: thought parts first, then answer text, then the
// functionCall carrying the turn's thoughtSignature.
func TestGeminiAdapter_ThoughtPartsStreamAsThinkingDelta(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	const sig = "AY89a18t+thought-signature=="
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"**Plan**\n\n","thought":true}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Read the file first.","thought":true}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Reading it."}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}},"thoughtSignature":"`+sig+`"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":40}}`)
	srv, bodies := geminiRecordingServer(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:    "gemini-3.8-flash",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go"}}}},
	})
	must.NoError(err)
	events := collectEvents(t, ch)

	thinking, text := splitDeltas(events)
	check.Equal("**Plan**\n\n|Read the file first.", strings.Join(thinking, "|"), "thinking_delta fragments")
	check.Equal("Reading it.", text, "answer text")

	var calls []types.StreamEvent
	var stop string
	for _, ev := range events {
		switch ev.Type {
		case "tool_call":
			calls = append(calls, ev)
		case "message_complete":
			stop = ev.StopReason
		case "error":
			must.FailNowf("unexpected error event", "%v", ev.Error)
		}
	}
	must.Len(calls, 1, "tool calls")
	check.Equal(sig, calls[0].ThoughtSignature, "tool call thought signature")
	check.Equal("a.go", calls[0].Input["path"], "tool call path")
	check.Equal("tool_use", stop, "stop reason")

	sent := bodies()
	must.Len(sent, 1)
	check.Contains(sent[0], `"thinkingConfig":{"includeThoughts":true}`, "request did not ask for thoughts")
}

// With reasoningSummary off no thoughts are requested, and a replayed
// history never sends a thought part back.
func TestGeminiAdapter_ReasoningSummaryOff_OmitsThoughtsOnReplay(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, bodies := geminiRecordingServer(t, makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`))
	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	input, _ := json.Marshal(map[string]any{"path": "a.go"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:            "gemini-3.8-flash",
		ReasoningSummary: types.ReasoningSummaryOff,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "text", Text: "Reading it."},
				{Type: "tool_use", ID: "gemini-1-0", Name: "read_file", Input: input, ThoughtSignature: "sig"},
			}},
			{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "gemini-1-0", Content: "package a"}}},
		},
	})
	must.NoError(err)
	collectEvents(t, ch)

	sent := bodies()
	must.Len(sent, 1)
	check.NotContains(sent[0], "includeThoughts", "reasoningSummary off still configured thinking")
	check.NotContains(sent[0], "thinkingConfig", "reasoningSummary off still configured thinking")
	check.NotContains(sent[0], `"thought"`, "replayed history carries a thought part")
	check.Contains(sent[0], `"thoughtSignature":"sig"`, "replayed functionCall lost its signature")
}

// TestBuildGenerateContentRequest_IncludeThoughtsGate pins which requests
// ask for thought summaries: a model whose quirks accept a thinking
// config, or any model given a reasoningEffort. A model with neither
// receives no thinkingConfig, since models that do not think reject one.
func TestBuildGenerateContentRequest_IncludeThoughtsGate(t *testing.T) {
	cases := []struct {
		name          string
		model, effort string
		summary       string
		want          string
	}{
		{name: "legacy flash without effort omits thinking config", model: "gemini-2.0-flash", want: ""},
		{name: "2.5 pro without effort omits thinking config", model: "gemini-2.5-pro", want: ""},
		{name: "2.5 flash-lite without effort omits thinking config", model: "gemini-2.5-flash-lite", want: ""},
		{name: "effort low sets thinking level and includes thoughts", model: "gemini-2.0-flash", effort: "low", want: `{"thinkingLevel":"LOW","includeThoughts":true}`},
		{name: "effort low with summary off sets thinking level only", model: "gemini-2.0-flash", effort: "low", summary: types.ReasoningSummaryOff, want: `{"thinkingLevel":"LOW"}`},
		{name: "3.8 flash includes thoughts by default", model: "gemini-3.8-flash", want: `{"includeThoughts":true}`},
		{name: "3.6 flash includes thoughts by default", model: "gemini-3.6-flash", want: `{"includeThoughts":true}`},
		{name: "3.1 pro preview includes thoughts by default", model: "gemini-3.1-pro-preview", want: `{"includeThoughts":true}`},
		{name: "2.5 flash includes thoughts by default", model: "gemini-2.5-flash", want: `{"includeThoughts":true}`},
		{name: "summary off omits thinking config", model: "gemini-3.8-flash", summary: types.ReasoningSummaryOff, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			params := types.StreamParams{
				Model:            tc.model,
				ReasoningEffort:  tc.effort,
				ReasoningSummary: tc.summary,
				Messages:         []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
			}
			body, _, err := BuildGenerateContentRequest(params, nil, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: tc.model}))
			must.NoError(err, "build request")
			var req struct {
				GenerationConfig *struct {
					ThinkingConfig json.RawMessage `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			must.NoError(json.Unmarshal(body, &req), "decode request")
			var got string
			if req.GenerationConfig != nil {
				got = string(req.GenerationConfig.ThinkingConfig)
			}
			must.Equal(tc.want, got, "thinkingConfig")
		})
	}
}
