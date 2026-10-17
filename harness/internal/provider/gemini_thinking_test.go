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

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// geminiRecordingServer answers every request with body and records the
// request bodies it received.
func geminiRecordingServer(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
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
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	thinking, text := splitDeltas(events)
	if got, want := strings.Join(thinking, "|"), "**Plan**\n\n|Read the file first."; got != want {
		t.Errorf("thinking_delta fragments = %q, want %q", got, want)
	}
	if text != "Reading it." {
		t.Errorf("answer text = %q, want %q", text, "Reading it.")
	}
	var calls []types.StreamEvent
	var stop string
	for _, ev := range events {
		switch ev.Type {
		case "tool_call":
			calls = append(calls, ev)
		case "message_complete":
			stop = ev.StopReason
		case "error":
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
	}
	if len(calls) != 1 || calls[0].ThoughtSignature != sig || calls[0].Input["path"] != "a.go" {
		t.Fatalf("tool calls = %+v, want one read_file(a.go) carrying the signature", calls)
	}
	if stop != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use", stop)
	}

	sent := bodies()
	if len(sent) != 1 || !strings.Contains(sent[0], `"thinkingConfig":{"includeThoughts":true}`) {
		t.Errorf("request did not ask for thoughts: %v", sent)
	}
}

// With reasoningSummary off no thoughts are requested, and a replayed
// history never sends a thought part back.
func TestGeminiAdapter_ReasoningSummaryOffAndReplay(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	collectEvents(t, ch)

	sent := bodies()
	if len(sent) != 1 {
		t.Fatalf("got %d requests, want 1", len(sent))
	}
	if strings.Contains(sent[0], "includeThoughts") || strings.Contains(sent[0], "thinkingConfig") {
		t.Errorf("reasoningSummary off still configured thinking: %s", sent[0])
	}
	if strings.Contains(sent[0], `"thought"`) {
		t.Errorf("replayed history carries a thought part: %s", sent[0])
	}
	if !strings.Contains(sent[0], `"thoughtSignature":"sig"`) {
		t.Errorf("replayed functionCall lost its signature: %s", sent[0])
	}
}

// TestBuildGenerateContentRequest_IncludeThoughtsGate pins which requests
// ask for thought summaries: a model whose quirks accept a thinking
// config, or any model given a reasoningEffort. A model with neither
// receives no thinkingConfig, since models that do not think reject one.
func TestBuildGenerateContentRequest_IncludeThoughtsGate(t *testing.T) {
	cases := []struct {
		model, effort, summary string
		want                   string
	}{
		{model: "gemini-2.0-flash", want: ""},
		{model: "gemini-2.5-pro", want: ""},
		{model: "gemini-2.5-flash-lite", want: ""},
		{model: "gemini-2.0-flash", effort: "low", want: `{"thinkingLevel":"LOW","includeThoughts":true}`},
		{model: "gemini-2.0-flash", effort: "low", summary: types.ReasoningSummaryOff, want: `{"thinkingLevel":"LOW"}`},
		{model: "gemini-3.8-flash", want: `{"includeThoughts":true}`},
		{model: "gemini-3.6-flash", want: `{"includeThoughts":true}`},
		{model: "gemini-3.1-pro-preview", want: `{"includeThoughts":true}`},
		{model: "gemini-2.5-flash", want: `{"includeThoughts":true}`},
		{model: "gemini-3.8-flash", summary: types.ReasoningSummaryOff, want: ""},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s effort=%q summary=%q", tc.model, tc.effort, tc.summary), func(t *testing.T) {
			params := types.StreamParams{
				Model:            tc.model,
				ReasoningEffort:  tc.effort,
				ReasoningSummary: tc.summary,
				Messages:         []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
			}
			body, _, err := BuildGenerateContentRequest(params, nil, quirks.DefaultRegistry().Resolve("gemini", tc.model))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var req struct {
				GenerationConfig *struct {
					ThinkingConfig json.RawMessage `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			var got string
			if req.GenerationConfig != nil {
				got = string(req.GenerationConfig.ThinkingConfig)
			}
			if got != tc.want {
				t.Errorf("thinkingConfig = %s, want %s", got, tc.want)
			}
		})
	}
}
