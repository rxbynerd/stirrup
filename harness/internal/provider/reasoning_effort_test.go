package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
// shape for the GPT-5.4+ and GPT-6 families: reasoning.effort is projected
// where the model documents it, and temperature is omitted whenever an
// effort is sent or the model rejects sampling params. gpt-5.4 with no
// effort keeps its temperature (its default effort is none). The GPT-5.x
// rows are documented, not probed.
func TestResponsesRequest_GPT6ReasoningAndSampling(t *testing.T) {
	cases := []struct {
		model, effort string
		want, absent  []string
	}{
		{"gpt-6.1-sol", "xhigh", []string{`"reasoning":{"effort":"xhigh"}`}, []string{`"temperature"`}},
		{"gpt-6-astra", "", nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-5.4", "", []string{`"temperature":0.1`}, []string{`"reasoning"`}},
		{"gpt-5.4", "high", []string{`"reasoning":{"effort":"high"}`}, []string{`"temperature"`}},
		// gpt-5.4-mini inherits the gpt-5.4 rule; only gpt-5.4 is documented,
		// so this row is inferred.
		{"gpt-5.4-mini", "xhigh", []string{`"reasoning":{"effort":"xhigh"}`}, []string{`"temperature"`}},
		{"gpt-5.5", "", nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-5.5", "low", []string{`"reasoning":{"effort":"low"}`}, []string{`"temperature"`}},
		{"gpt-5.6-sol", "max", []string{`"reasoning":{"effort":"max"}`}, []string{`"temperature"`}},
		{"gpt-5.6-luna", "", nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-4.1", "high", []string{`"temperature":0.1`}, []string{`"reasoning"`}},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(params, q, nil, "")
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

// TestResponsesAdapter_GPT5EffortAllowListsBeforeSend pins the GPT-5.x
// Responses allow-lists at the pre-send guard: minimal is documented for
// none of them, and max only for gpt-5.6. The documented none level is
// outside the provider-neutral enum, so it is refused on every model.
func TestResponsesAdapter_GPT5EffortAllowListsBeforeSend(t *testing.T) {
	cases := []struct{ model, effort string }{
		{"gpt-5.6-terra", "minimal"},
		{"gpt-5.5", "minimal"},
		{"gpt-5.5", "max"},
		{"gpt-5.4", "max"},
		{"gpt-5.4", "minimal"},
		{"gpt-5.4", "none"},
		{"gpt-5.5", "none"},
		{"gpt-5.6-sol", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			srv, hits := countingServer(t)
			adapter := NewOpenAIResponsesAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{})
			_, err := adapter.Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			want := `reasoningEffort "` + tc.effort + `" is not supported`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Stream() error = %v, want %q", err, want)
			}
			if hits.Load() != 0 {
				t.Errorf("server received %d requests, want 0", hits.Load())
			}
		})
	}
}

// TestResponsesAdapter_GPT5EffortLevelsOnTheWire drives each accepted
// GPT-5.x level through Stream and pins the sent body: the level as
// reasoning.effort and no temperature. With no effort, gpt-5.5 and gpt-5.6
// still omit temperature (inferred from their medium default). Documented,
// not probed.
func TestResponsesAdapter_GPT5EffortLevelsOnTheWire(t *testing.T) {
	cases := []struct{ model, effort string }{
		{"gpt-5.6-sol", "low"},
		{"gpt-5.6-sol", "medium"},
		{"gpt-5.6-sol", "high"},
		{"gpt-5.6-sol", "xhigh"},
		{"gpt-5.6-sol", "max"},
		{"gpt-5.5", ""},
		{"gpt-5.6-luna", ""},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			script := newResponsesScript(t)
			ch, err := script.adapter(nil).Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			collectEvents(t, ch)
			body := string(script.lastBody(t))
			if strings.Contains(body, `"temperature"`) {
				t.Errorf("body carries temperature: %s", body)
			}
			wantReasoning := tc.effort != ""
			if got := strings.Contains(body, `"reasoning":{"effort":"`+tc.effort+`"}`); got != wantReasoning {
				t.Errorf("reasoning.effort %q present = %v, want %v: %s", tc.effort, got, wantReasoning, body)
			}
		})
	}
}

// TestResponsesAdapter_EffortSuppressesTemperatureWarns pins that a dropped
// caller temperature is logged exactly once per request, whether an effort
// (gpt-5.4, with no sampling-param rule) or the model's rule (gpt-5.6) or
// both cause it.
func TestResponsesAdapter_EffortSuppressesTemperatureWarns(t *testing.T) {
	cases := []struct {
		model, effort string
		wantWarns     int
	}{
		{"gpt-5.4", "high", 1},
		{"gpt-5.4", "", 0},
		{"gpt-5.6-sol", "high", 1},
		{"gpt-5.6-sol", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/effort="+tc.effort, func(t *testing.T) {
			var buf bytes.Buffer
			adapter := NewOpenAIResponsesAdapter(staticBearer("k"), responsesWarnStubServer(t).URL, OpenAIAuthConfig{})
			adapter.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
			ch, err := adapter.Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			collectEvents(t, ch)
			if got := strings.Count(buf.String(), "suppressed caller temperature"); got != tc.wantWarns {
				t.Errorf("warnings = %d, want %d; log: %s", got, tc.wantWarns, buf.String())
			}
		})
	}
}

func TestWarnDroppedReasoningEffort(t *testing.T) {
	cases := []struct {
		name    string
		level   string
		allowed []string
		want    bool
	}{
		{"unset level", "", nil, false},
		{"no effort control", "high", nil, true},
		{"advertised level", "high", []string{"low", "high"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			warnDroppedReasoningEffort(context.Background(), logger, "anthropic", tc.level, "claude-haiku-4-5", tc.allowed)
			if got := strings.Contains(buf.String(), "reasoningEffort ignored"); got != tc.want {
				t.Errorf("warned = %v, want %v; log: %s", got, tc.want, buf.String())
			}
		})
	}
}
