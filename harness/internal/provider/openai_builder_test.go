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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			// (openai.go) at the builder level, so the batch path (which
			// calls the builder directly) inherits the same coverage.
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

// buildOpenAIBody builds the request for params with streaming enabled and
// returns its JSON body. It calls require, so it must run on the test goroutine.
func buildOpenAIBody(t *testing.T, params types.StreamParams, q quirks.ProviderQuirks) string {
	t.Helper()
	req, err := buildOpenAIRequest(params, true, q, nil)
	require.NoError(t, err, "build request")
	return marshalBody(t, req)
}

// marshalBody returns the JSON encoding of v. It calls require, so it must
// run on the test goroutine.
func marshalBody(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err, "marshal request")
	return string(body)
}

// TestBuildOpenAIRequest_MatchesStream pins the invariant that
// buildOpenAIRequest produces the same wire body the Stream method would
// emit. The batch path reuses the builder and must be byte-identical to
// streaming for the same StreamParams modulo the stream toggle.
func TestBuildOpenAIRequest_MatchesStream(t *testing.T) {
	for _, tc := range openaiBuilderCases() {
		t.Run(tc.name, func(t *testing.T) {
			capturedCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The handler runs on the server goroutine, so it reports with assert.
				b, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err, "read request body") {
					capturedCh <- nil
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
			require.NoError(t, err, "Stream()")
			for range ch {
			}
			captured := <-capturedCh

			q := quirks.DefaultRegistry().Resolve("openai-compatible", tc.params.Model)
			require.Equal(t, string(captured), buildOpenAIBody(t, tc.params, q))
		})
	}
}

// TestBuildOpenAIRequest_ToolChoice_ProjectsEachMode pins the tool_choice
// projection gated on the resolved capability. OpenAI's base rule advertises
// every mode, so:
//   - ToolChoiceAuto (zero) emits no tool_choice field;
//   - ToolChoiceRequired emits "tool_choice":"required";
//   - ToolChoiceNone emits "tool_choice":"none";
//   - ToolChoiceTool emits the typed function object (deterministic key
//     order: "type" before "function");
//   - ToolChoiceTool with no name, an invalid name, or an over-length
//     name degrades to auto (no field).
func TestBuildOpenAIRequest_ToolChoice_ProjectsEachMode(t *testing.T) {
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
		// "function", deterministic across processes.
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
			body := buildOpenAIBody(t, params, q)
			if tc.wantSubstr == "" {
				require.NotContains(t, body, "tool_choice")
				return
			}
			require.Contains(t, body, tc.wantSubstr)
		})
	}
}

// TestBuildOpenAIRequest_ToolChoice_DeterministicKeyOrder pins that the
// named-tool object is a typed struct, so its JSON key order is the same on
// every run. encoding/json sorts map keys, which would put "function" before
// "type". Marshalling the same request repeatedly must yield byte-identical
// output.
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

	first := buildOpenAIBody(t, params, q)
	for i := 1; i < 20; i++ {
		require.Equal(t, first, buildOpenAIBody(t, params, q), "iteration %d", i)
	}
}

// TestBuildOpenAIRequest_ToolChoice_PartialCapability exercises the
// per-mode guard branches that today's full-support builtin rules never
// reach: a capability with Supported=true but a specific mode
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
		require.NotContains(t, buildOpenAIBody(t, params, q), "tool_choice")
	})
	t.Run("none disabled", func(t *testing.T) {
		params := types.StreamParams{
			Model:      "gpt-4o",
			MaxTokens:  256,
			ToolChoice: types.ToolChoiceNone,
			Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
		}
		q := quirks.ProviderQuirks{ToolChoice: quirks.ToolChoiceCapability{Supported: true, None: false}}
		require.NotContains(t, buildOpenAIBody(t, params, q), "tool_choice")
	})
}

// TestToolChoiceName_Invalid_WarnsWithoutOffendingName pins the observability
// half: an invalid named-tool name degrades to auto AND emits a slog.Warn that
// carries the grammar but NOT the offending name (which could carry
// log-injection bytes). The shared warnInvalidToolChoiceName helper is
// exercised through the openai projection; a single test covers the
// helper for all three adapters since they call the same function.
func TestToolChoiceName_Invalid_WarnsWithoutOffendingName(t *testing.T) {
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
	require.NotContains(t, buildOpenAIBody(t, params, q), "tool_choice", "invalid name must degrade to auto")

	logged := buf.String()
	require.Contains(t, logged, "failed validation")
	// Only the name's length and the grammar are safe to surface in the log.
	require.NotContains(t, logged, "bad name!", "warn leaked the offending name")
	require.NotContains(t, logged, "injected", "warn leaked the offending name")
	require.Contains(t, logged, `"grammar"`)
}

// TestBuildOpenAIRequest_ToolChoice_UnsupportedCapabilityOmitsField pins the
// graceful no-op: a zero-value capability emits no tool_choice field even for
// a ToolChoiceRequired request.
func TestBuildOpenAIRequest_ToolChoice_UnsupportedCapabilityOmitsField(t *testing.T) {
	params := types.StreamParams{
		Model:      "gpt-4o",
		MaxTokens:  256,
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	require.NotContains(t, buildOpenAIBody(t, params, quirks.ProviderQuirks{}), "tool_choice",
		"zero-value capability must emit no tool_choice field")
}

// TestBuildOpenAIRequest_StreamFlag verifies the stream argument drives
// the wire field: stream=false must serialise as "stream":false.
//
// The marshalled-body checks guard against openaiRequest.Stream being tagged
// omitempty, which the struct-level checks would not catch. OpenAI's tag
// intentionally lacks omitempty, so false must appear on the wire.
func TestBuildOpenAIRequest_StreamFlag(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "x"}}}},
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)

	reqTrue, err := buildOpenAIRequest(params, true, q, nil)
	require.NoError(t, err, "build stream=true")
	require.True(t, reqTrue.Stream)

	reqFalse, err := buildOpenAIRequest(params, false, q, nil)
	require.NoError(t, err, "build stream=false")
	require.False(t, reqFalse.Stream)

	require.Contains(t, marshalBody(t, reqTrue), `"stream":true`)
	require.Contains(t, marshalBody(t, reqFalse), `"stream":false`)
}
