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
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// openaiBuilderCases enumerates representative StreamParams shapes for
// the builder vs Stream equivalence assertion. Each case exercises a
// different combination of fields the helper has to project: minimal,
// system+temperature, multi-turn with tool round-trip, multiple tools.
func openaiBuilderCases() []struct {
	name   string
	params types.StreamParams
} {
	return []struct {
		name   string
		params types.StreamParams
	}{
		{
			name: "minimal",
			params: types.StreamParams{
				Model:     "gpt-4o",
				MaxTokens: 1024,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			},
		},
		{
			name: "system_and_temperature",
			params: types.StreamParams{
				Model:       "gpt-4o",
				System:      "You are helpful.",
				MaxTokens:   2048,
				Temperature: types.Float64Ptr(0.5),
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
				},
			},
		},
		{
			name: "tools_and_multi_turn",
			params: types.StreamParams{
				Model:     "gpt-4o",
				System:    "Use tools when needed.",
				MaxTokens: 4096,
				Tools: []types.ToolDefinition{
					{
						Name:        "read_file",
						Description: "Read a file from disk",
						InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
					},
					{
						Name:        "write_file",
						Description: "Write a file to disk",
						InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
					},
				},
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read main.go"}}},
					{Role: "assistant", Content: []types.ContentBlock{
						{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
					}},
					{Role: "user", Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
					}},
				},
			},
		},
		{
			// Pins the "Error: " prefix injection in translateMessages
			// (openai.go) at the builder level. Covered by openai_test.go
			// through Stream, but the batch path will call the builder
			// directly; the MatchesStream harness extends that coverage
			// here automatically.
			name: "is_error_tool_result",
			params: types.StreamParams{
				Model:     "gpt-4o",
				MaxTokens: 1024,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "run it"}}},
					{Role: "assistant", Content: []types.ContentBlock{
						{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{}`)},
					}},
					{Role: "user", Content: []types.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "disk full", IsError: true},
					}},
				},
			},
		},
	}
}

// TestBuildOpenAIRequest_MatchesStream pins the invariant that
// buildOpenAIRequest produces the same wire body the Stream method would
// emit. The batch path (phase 6 of #133) reuses the builder and must be
// byte-identical to streaming for the same StreamParams modulo the
// stream toggle.
func TestBuildOpenAIRequest_MatchesStream(t *testing.T) {
	for _, tc := range openaiBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			capturedCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
					return
				}
				capturedCh <- b
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				// Minimal valid chunk + DONE so Stream's SSE consumer
				// terminates cleanly.
				_, _ = fmt.Fprint(w,
					makeOpenAIChunk(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
						"data: [DONE]\n\n",
				)
			}))
			defer srv.Close()

			adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

			ch, err := adapter.Stream(context.Background(), tc.params)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			for range ch {
			}
			captured := <-capturedCh

			q := quirks.DefaultRegistry().Resolve("openai-compatible", tc.params.Model)
			built, err := buildOpenAIRequest(tc.params, true, q, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			builtBytes, err := json.Marshal(built)
			if err != nil {
				t.Fatalf("marshal builder output: %v", err)
			}

			if string(builtBytes) != string(captured) {
				t.Errorf("builder vs Stream body mismatch\n builder: %s\n stream:  %s", builtBytes, captured)
			}
		})
	}
}

// TestBuildOpenAIRequest_ToolChoice pins the tool_choice projection gated
// on the resolved capability. OpenAI's base rule advertises every mode,
// so:
//   - ToolChoiceAuto (zero) emits no tool_choice field;
//   - ToolChoiceRequired emits "tool_choice":"required";
//   - ToolChoiceNone emits "tool_choice":"none";
//   - ToolChoiceTool emits the typed function object (deterministic key
//     order: "type" before "function");
//   - ToolChoiceTool with no name, an invalid name, or an over-length
//     name degrades to auto (no field).
func TestBuildOpenAIRequest_ToolChoice(t *testing.T) {
	base := types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", base.Model)

	cases := []struct {
		name       string
		choice     types.ToolChoiceMode
		toolName   string
		wantSubstr string // empty means "no tool_choice field"
	}{
		{"auto omits field", types.ToolChoiceAuto, "", ""},
		{"required emits required", types.ToolChoiceRequired, "", `"tool_choice":"required"`},
		{"none emits none", types.ToolChoiceNone, "", `"tool_choice":"none"`},
		// The typed openAINamedToolChoice fixes key order: "type" before
		// "function", deterministic across processes (B1).
		{"tool emits typed function object", types.ToolChoiceTool, "read_file", `"tool_choice":{"type":"function","function":{"name":"read_file"}}`},
		{"tool without name degrades to auto", types.ToolChoiceTool, "", ""},
		{"tool with invalid name degrades to auto", types.ToolChoiceTool, "bad name!", ""},
		{"tool with over-length name degrades to auto", types.ToolChoiceTool, strings.Repeat("a", 65), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base
			params.ToolChoice = tc.choice
			params.ToolChoiceName = tc.toolName
			req, err := buildOpenAIRequest(params, true, q, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantSubstr == "" {
				if strings.Contains(string(body), "tool_choice") {
					t.Errorf("expected no tool_choice field, got body: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Errorf("expected %s in body, got: %s", tc.wantSubstr, body)
			}
		})
	}
}

// TestBuildOpenAIRequest_ToolChoice_DeterministicKeyOrder pins B1: the
// named-tool object is a typed struct, not a map[string]any, so its JSON
// key order is identical on every run. The map form produced
// process-dependent ordering (Go's randomised map hash seed), which made
// the fixture pin flaky. Marshalling the same request many times must
// yield byte-identical output.
func TestBuildOpenAIRequest_ToolChoice_DeterministicKeyOrder(t *testing.T) {
	params := types.StreamParams{
		Model:          "gpt-4o",
		MaxTokens:      256,
		ToolChoice:     types.ToolChoiceTool,
		ToolChoiceName: "read_file",
		Messages:       []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools:          []types.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)

	var first string
	for i := 0; i < 20; i++ {
		req, err := buildOpenAIRequest(params, true, q, nil)
		if err != nil {
			t.Fatalf("build (iter %d): %v", i, err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal (iter %d): %v", i, err)
		}
		if i == 0 {
			first = string(body)
			continue
		}
		if string(body) != first {
			t.Fatalf("non-deterministic body on iter %d:\n first: %s\n now:   %s", i, first, body)
		}
	}
}

// TestBuildOpenAIRequest_ToolChoice_PartialCapability exercises the
// per-mode guard branches (B2) that today's full-support builtin rules
// never reach: a capability with Supported=true but a specific mode
// disabled must omit the field rather than fail open and emit the
// disallowed mode on the wire.
func TestBuildOpenAIRequest_ToolChoice_PartialCapability(t *testing.T) {
	t.Run("required disabled", func(t *testing.T) {
		params := types.StreamParams{
			Model:      "gpt-4o",
			MaxTokens:  256,
			ToolChoice: types.ToolChoiceRequired,
			Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		}
		q := quirks.ProviderQuirks{ToolChoice: quirks.ToolChoiceCapability{Supported: true, Required: false}}
		assertNoToolChoice(t, params, q)
	})
	t.Run("none disabled", func(t *testing.T) {
		params := types.StreamParams{
			Model:      "gpt-4o",
			MaxTokens:  256,
			ToolChoice: types.ToolChoiceNone,
			Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		}
		q := quirks.ProviderQuirks{ToolChoice: quirks.ToolChoiceCapability{Supported: true, None: false}}
		assertNoToolChoice(t, params, q)
	})
}

// TestToolChoiceName_InvalidEmitsWarn pins B3's observability half: an
// invalid named-tool name degrades to auto AND emits a slog.Warn that
// carries the grammar but NOT the offending name (which could carry
// log-injection bytes). The shared warnInvalidToolChoiceName helper is
// exercised through the openai projection; a single test covers the
// helper for all three adapters since they call the same function.
func TestToolChoiceName_InvalidEmitsWarn(t *testing.T) {
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const badName = "bad name!\ninjected"
	params := types.StreamParams{
		Model:          "gpt-4o",
		MaxTokens:      256,
		ToolChoice:     types.ToolChoiceTool,
		ToolChoiceName: badName,
		Messages:       []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		Tools:          []types.ToolDefinition{{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("invalid name must degrade to auto, got: %s", body)
	}

	logged := buf.String()
	if !strings.Contains(logged, "failed validation") {
		t.Errorf("expected a validation warn, got log: %s", logged)
	}
	// The raw name must NOT appear in the log line — only its length and
	// the grammar are safe to surface.
	if strings.Contains(logged, "bad name!") || strings.Contains(logged, "injected") {
		t.Errorf("warn leaked the offending name into the log: %s", logged)
	}
	if !strings.Contains(logged, `"grammar"`) {
		t.Errorf("warn missing the grammar attribute: %s", logged)
	}
}

// assertNoToolChoice builds the request and fails if the marshalled body
// carries a tool_choice field.
func assertNoToolChoice(t *testing.T, params types.StreamParams, q quirks.ProviderQuirks) {
	t.Helper()
	req, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("expected no tool_choice field, got body: %s", body)
	}
}

// TestBuildOpenAIRequest_ToolChoiceUnsupportedCapability pins the
// graceful no-op: a zero-value capability emits no tool_choice field
// even for a ToolChoiceRequired request.
func TestBuildOpenAIRequest_ToolChoiceUnsupportedCapability(t *testing.T) {
	params := types.StreamParams{
		Model:      "gpt-4o",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	req, err := buildOpenAIRequest(params, true, quirks.ProviderQuirks{}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("zero-value capability must emit no tool_choice field, got: %s", body)
	}
}

// TestBuildOpenAIRequest_StreamFlag verifies the stream argument drives
// the wire field, so a future batch caller passing false produces a body
// with "stream":false. Pinning this here prevents the helper from
// regressing to a hard-coded true once the batch path lands.
//
// The marshalled-body assertions catch the failure mode the struct-level
// checks miss: if openaiRequest.Stream were tagged omitempty, the struct
// check would still pass while "stream":false silently disappeared from
// the wire body. OpenAI's tag intentionally lacks omitempty, so false
// must serialise as "stream":false — pin both.
func TestBuildOpenAIRequest_StreamFlag(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	reqTrue, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("build stream=true: %v", err)
	}
	if reqTrue.Stream != true {
		t.Errorf("stream=true argument: got Stream=%v, want true", reqTrue.Stream)
	}
	reqFalse, err := buildOpenAIRequest(params, false, q, nil)
	if err != nil {
		t.Fatalf("build stream=false: %v", err)
	}
	if reqFalse.Stream != false {
		t.Errorf("stream=false argument: got Stream=%v, want false", reqFalse.Stream)
	}
	trueBody, err := json.Marshal(reqTrue)
	if err != nil {
		t.Fatalf("marshal stream=true body: %v", err)
	}
	if !strings.Contains(string(trueBody), `"stream":true`) {
		t.Errorf(`expected "stream":true in stream=true body: %s`, trueBody)
	}
	falseBody, err := json.Marshal(reqFalse)
	if err != nil {
		t.Fatalf("marshal stream=false body: %v", err)
	}
	if !strings.Contains(string(falseBody), `"stream":false`) {
		t.Errorf(`expected "stream":false in stream=false body: %s`, falseBody)
	}
}
