package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func effortParams(model, effort string, withTools bool) types.StreamParams {
	p := types.StreamParams{
		Model:           model,
		MaxTokens:       1024,
		Temperature:     types.Float64Ptr(0.1),
		ReasoningEffort: effort,
		Messages:        []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	if withTools {
		p.Tools = []types.ToolDefinition{{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
	}
	return p
}

// countingServer counts requests so a test can assert that a pre-send
// rejection never reached the API.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestOpenAIRequest_ReasoningEffortProjection pins which models receive a
// top-level reasoning_effort on Chat Completions: only those whose
// resolved allow-list names the level. A model with no list must not get
// the key, because non-reasoning models reject it.
func TestOpenAIRequest_ReasoningEffortProjection(t *testing.T) {
	cases := []struct {
		model, effort, want string // want "" means "no reasoning_effort key"
	}{
		{"gpt-6-astra", "high", `"reasoning_effort":"high"`},
		{"gpt-6-luna", "LOW", `"reasoning_effort":"low"`},
		{"deepseek-flash", "medium", `"reasoning_effort":"medium"`},
		{"deepseek-v4-pro", "max", `"reasoning_effort":"max"`},
		{"gpt-4o", "high", ""},
		{"openai/gpt-6-luna", "high", ""},
		{"gpt-6-astra", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			q := quirks.DefaultRegistry().Resolve("openai-compatible", tc.model)
			req, err := buildOpenAIRequest(params, true, q, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.want == "" {
				if strings.Contains(string(body), "reasoning_effort") {
					t.Errorf("unexpected reasoning_effort: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("want %s in %s", tc.want, body)
			}
		})
	}
}

// TestOpenAIRequest_GPT6WireShape pins the reasoning-class wire shape for a
// first-party gpt-6 id: temperature omitted and max_completion_tokens as
// the budget key.
func TestOpenAIRequest_GPT6WireShape(t *testing.T) {
	params := effortParams("gpt-6.1-sol", "", false)
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, _ := json.Marshal(req)
	s := string(body)
	if strings.Contains(s, `"temperature"`) {
		t.Errorf("temperature must be omitted: %s", s)
	}
	if !strings.Contains(s, `"max_completion_tokens":1024`) {
		t.Errorf("want max_completion_tokens: %s", s)
	}
}

// TestOpenAIRequest_ReasoningEffortRoundTrip pins that reasoning_effort
// decodes back onto its own field rather than into ExtraBodyFields, where
// a re-marshal would reject it as a canonical-field collision.
func TestOpenAIRequest_ReasoningEffortRoundTrip(t *testing.T) {
	in := openaiRequest{Model: "gpt-6-astra", ReasoningEffort: "xhigh", MaxTokens: 8}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out openaiRequest
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ReasoningEffort != "xhigh" || len(out.ExtraBodyFields) != 0 {
		t.Errorf("round-trip: ReasoningEffort = %q, ExtraBodyFields = %v", out.ReasoningEffort, out.ExtraBodyFields)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("re-marshal: %v", err)
	}
}

// TestOpenAIAdapter_RejectsBeforeSend pins the Chat Completions pre-send
// guards: tools on a first-party gpt-6 id, and an effort level outside the
// model's allow-list. Neither may reach the API.
func TestOpenAIAdapter_RejectsBeforeSend(t *testing.T) {
	cases := []struct {
		name    string
		params  types.StreamParams
		wantErr string
	}{
		{"gpt-6 tools", effortParams("gpt-6-astra", "", true), `use provider type "openai-responses"`},
		{"gpt-6 minimal", effortParams("gpt-6-sol", "minimal", false), `reasoningEffort "minimal" is not supported`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := countingServer(t)
			adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
			_, err := adapter.Stream(context.Background(), tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Stream() error = %v, want %q", err, tc.wantErr)
			}
			if hits.Load() != 0 {
				t.Errorf("server received %d requests, want 0", hits.Load())
			}
		})
	}
}

// TestOpenAIAdapter_GatewayGPT6AllowsTools pins that the tools guard is
// first-party only: a gateway-prefixed gpt-6 id passes the pre-send check.
func TestOpenAIAdapter_GatewayGPT6AllowsTools(t *testing.T) {
	params := effortParams("openai/gpt-6-luna", "", true)
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	if err := checkOpenAIRequestSupported(params, q); err != nil {
		t.Errorf("checkOpenAIRequestSupported: %v", err)
	}
}

// TestResponsesRequest_GPT6ReasoningAndSampling pins the Responses wire
// shape for gpt-6: reasoning.effort projected, temperature omitted; and
// that gpt-5.4 keeps its temperature and gets no reasoning object.
func TestResponsesRequest_GPT6ReasoningAndSampling(t *testing.T) {
	cases := []struct {
		model, effort string
		want, absent  []string
	}{
		{"gpt-6.1-sol", "xhigh", []string{`"reasoning":{"effort":"xhigh"}`}, []string{`"temperature"`}},
		{"gpt-6-astra", "", nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-5.4", "high", []string{`"temperature":0.1`}, []string{`"reasoning"`}},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(params, q, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(body), w) {
					t.Errorf("want %s in %s", w, body)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(string(body), a) {
					t.Errorf("unexpected %s in %s", a, body)
				}
			}
			var back responsesRequest
			if err := json.Unmarshal(body, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.ReasoningEffort != req.ReasoningEffort {
				t.Errorf("round-trip ReasoningEffort = %q, want %q", back.ReasoningEffort, req.ReasoningEffort)
			}
		})
	}
}

// TestResponsesAdapter_RejectsUnsupportedEffortBeforeSend mirrors the Chat
// Completions guard on the Responses surface.
func TestResponsesAdapter_RejectsUnsupportedEffortBeforeSend(t *testing.T) {
	srv, hits := countingServer(t)
	adapter := NewOpenAIResponsesAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{})
	_, err := adapter.Stream(context.Background(), effortParams("gpt-6-astra", "minimal", true))
	if err == nil || !strings.Contains(err.Error(), `reasoningEffort "minimal" is not supported`) {
		t.Fatalf("Stream() error = %v, want unsupported-effort error", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server received %d requests, want 0", hits.Load())
	}
}
