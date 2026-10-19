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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		name, model, effort, want string // want "" means the body must omit reasoning_effort
	}{
		{"gpt-6 projects high effort", "gpt-6-astra", "high", `"reasoning_effort":"high"`},
		{"gpt-6 lowercases effort", "gpt-6-luna", "LOW", `"reasoning_effort":"low"`},
		{"deepseek flash projects medium effort", "deepseek-flash", "medium", `"reasoning_effort":"medium"`},
		{"deepseek v4 pro projects max effort", "deepseek-v4-pro", "max", `"reasoning_effort":"max"`},
		{"model without allow-list omits effort", "gpt-4o", "high", ""},
		{"gateway-prefixed gpt-6 omits effort", "openai/gpt-6-luna", "high", ""},
		{"empty effort omits key", "gpt-6-astra", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			q := quirks.DefaultRegistry().Resolve("openai-compatible", tc.model)
			req, err := buildOpenAIRequest(params, true, q, nil)
			require.NoError(t, err)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			if tc.want == "" {
				require.NotContains(t, string(body), "reasoning_effort")
				return
			}
			require.Contains(t, string(body), tc.want)
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
	require.NoError(t, err)
	body, _ := json.Marshal(req)
	text := string(body)
	assert.NotContains(t, text, `"temperature"`)
	assert.Contains(t, text, `"max_completion_tokens":1024`)
}

// TestOpenAIRequest_ReasoningEffortRoundTrip pins that reasoning_effort
// decodes back onto its own field rather than into ExtraBodyFields, where
// a re-marshal would reject it as a canonical-field collision.
func TestOpenAIRequest_ReasoningEffortRoundTrip(t *testing.T) {
	in := openaiRequest{Model: "gpt-6-astra", ReasoningEffort: "xhigh", MaxTokens: 8}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	var out openaiRequest
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, "xhigh", out.ReasoningEffort)
	assert.Empty(t, out.ExtraBodyFields)
	_, err = json.Marshal(out)
	assert.NoError(t, err)
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
			require.ErrorContains(t, err, tc.wantErr)
			require.Zero(t, hits.Load())
		})
	}
}

// TestOpenAIAdapter_GatewayGPT6AllowsTools pins that the tools guard is
// first-party only: a gateway-prefixed gpt-6 id passes the pre-send check.
func TestOpenAIAdapter_GatewayGPT6AllowsTools(t *testing.T) {
	params := effortParams("openai/gpt-6-luna", "", true)
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	require.NoError(t, checkOpenAIRequestSupported(params, q))
}

// TestResponsesRequest_GPT6ReasoningAndSampling pins the Responses wire
// shape for the GPT-5.4+ and GPT-6 families: reasoning.effort is projected
// where the model documents it, and temperature is omitted whenever an
// effort is sent or the model rejects sampling params. gpt-5.4 with no
// effort keeps its temperature (its default effort is none). The GPT-5.x
// rows are documented, not probed.
func TestResponsesRequest_GPT6ReasoningAndSampling(t *testing.T) {
	cases := []struct {
		name, model, effort string
		contains            []string
		notContains         []string
	}{
		{"gpt-6.1 sends xhigh effort and omits temperature", "gpt-6.1-sol", "xhigh",
			[]string{`"reasoning":{"effort":"xhigh"}`}, []string{`"temperature"`}},
		{"gpt-6 without effort omits temperature and reasoning", "gpt-6-astra", "",
			nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-5.4 without effort keeps temperature", "gpt-5.4", "",
			[]string{`"temperature":0.1`}, []string{`"reasoning"`}},
		{"gpt-5.4 high sends effort and omits temperature", "gpt-5.4", "high",
			[]string{`"reasoning":{"effort":"high"}`}, []string{`"temperature"`}},
		// gpt-5.4-mini inherits the gpt-5.4 rule; only gpt-5.4 is documented,
		// so this row is inferred.
		{"gpt-5.4-mini sends xhigh effort and omits temperature", "gpt-5.4-mini", "xhigh",
			[]string{`"reasoning":{"effort":"xhigh"}`}, []string{`"temperature"`}},
		{"gpt-5.5 without effort omits temperature and reasoning", "gpt-5.5", "",
			nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-5.5 low sends effort and omits temperature", "gpt-5.5", "low",
			[]string{`"reasoning":{"effort":"low"}`}, []string{`"temperature"`}},
		{"gpt-5.6-sol max sends effort and omits temperature", "gpt-5.6-sol", "max",
			[]string{`"reasoning":{"effort":"max"}`}, []string{`"temperature"`}},
		{"gpt-5.6-luna without effort omits temperature and reasoning", "gpt-5.6-luna", "",
			nil, []string{`"temperature"`, `"reasoning"`}},
		{"gpt-4.1 keeps temperature and omits reasoning", "gpt-4.1", "high",
			[]string{`"temperature":0.1`}, []string{`"reasoning"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(params, q, nil, "")
			require.NoError(t, err)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			text := string(body)
			for _, want := range tc.contains {
				require.Contains(t, text, want)
			}
			for _, absent := range tc.notContains {
				require.NotContains(t, text, absent)
			}
			var back responsesRequest
			require.NoError(t, json.Unmarshal(body, &back))
			require.Equal(t, req.ReasoningEffort, back.ReasoningEffort)
		})
	}
}

// TestResponsesAdapter_RejectsUnsupportedEffortBeforeSend mirrors the Chat
// Completions guard on the Responses surface.
func TestResponsesAdapter_RejectsUnsupportedEffortBeforeSend(t *testing.T) {
	srv, hits := countingServer(t)
	adapter := NewOpenAIResponsesAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{})
	_, err := adapter.Stream(context.Background(), effortParams("gpt-6-astra", "minimal", true))
	require.ErrorContains(t, err, `reasoningEffort "minimal" is not supported`)
	require.Zero(t, hits.Load())
}

// TestResponsesAdapter_GPT5EffortAllowListsBeforeSend pins the GPT-5.x
// Responses allow-lists at the pre-send guard: minimal is documented for
// none of them, and max only for gpt-5.6. The documented none level is
// outside the provider-neutral enum, so it is refused on every model.
func TestResponsesAdapter_GPT5EffortAllowListsBeforeSend(t *testing.T) {
	cases := []struct {
		name, model, effort string
	}{
		{"gpt-5.6-terra rejects minimal", "gpt-5.6-terra", "minimal"},
		{"gpt-5.5 rejects minimal", "gpt-5.5", "minimal"},
		{"gpt-5.5 rejects max", "gpt-5.5", "max"},
		{"gpt-5.4 rejects max", "gpt-5.4", "max"},
		{"gpt-5.4 rejects minimal", "gpt-5.4", "minimal"},
		{"gpt-5.4 rejects none", "gpt-5.4", "none"},
		{"gpt-5.5 rejects none", "gpt-5.5", "none"},
		{"gpt-5.6-sol rejects none", "gpt-5.6-sol", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := countingServer(t)
			adapter := NewOpenAIResponsesAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{})
			_, err := adapter.Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			require.ErrorContains(t, err, `reasoningEffort "`+tc.effort+`" is not supported`)
			require.Zero(t, hits.Load())
		})
	}
}

// TestResponsesAdapter_GPT5EffortLevelsOnTheWire drives each accepted
// GPT-5.x level through Stream and pins the sent body: the level as
// reasoning.effort and no temperature. With no effort, gpt-5.5 and gpt-5.6
// still omit temperature (inferred from their medium default). Documented,
// not probed.
func TestResponsesAdapter_GPT5EffortLevelsOnTheWire(t *testing.T) {
	cases := []struct {
		name, model, effort string
	}{
		{"gpt-5.6-sol sends low effort", "gpt-5.6-sol", "low"},
		{"gpt-5.6-sol sends medium effort", "gpt-5.6-sol", "medium"},
		{"gpt-5.6-sol sends high effort", "gpt-5.6-sol", "high"},
		{"gpt-5.6-sol sends xhigh effort", "gpt-5.6-sol", "xhigh"},
		{"gpt-5.6-sol sends max effort", "gpt-5.6-sol", "max"},
		{"gpt-5.5 without effort omits temperature and reasoning", "gpt-5.5", ""},
		{"gpt-5.6-luna without effort omits temperature and reasoning", "gpt-5.6-luna", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := newResponsesScript(t)
			ch, err := script.adapter(nil).Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			require.NoError(t, err)
			collectEvents(t, ch)
			body := string(script.lastBody(t))
			require.NotContains(t, body, `"temperature"`)
			wantReasoning := tc.effort != ""
			gotReasoning := strings.Contains(body, `"reasoning":{"effort":"`+tc.effort+`"}`)
			require.Equal(t, wantReasoning, gotReasoning, "reasoning.effort %q in body: %s", tc.effort, body)
		})
	}
}

// TestResponsesAdapter_EffortSuppressesTemperatureWarns pins that a dropped
// caller temperature is logged exactly once per request, whether an effort
// (gpt-5.4, with no sampling-param rule) or the model's rule (gpt-5.6) or
// both cause it.
func TestResponsesAdapter_EffortSuppressesTemperatureWarns(t *testing.T) {
	cases := []struct {
		name, model, effort string
		wantWarns           int
	}{
		{"gpt-5.4 effort warns once", "gpt-5.4", "high", 1},
		{"gpt-5.4 without effort does not warn", "gpt-5.4", "", 0},
		{"gpt-5.6 effort warns once", "gpt-5.6-sol", "high", 1},
		{"gpt-5.6 without effort warns once from model rule", "gpt-5.6-sol", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			adapter := NewOpenAIResponsesAdapter(staticBearer("k"), responsesWarnStubServer(t).URL, OpenAIAuthConfig{})
			adapter.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
			ch, err := adapter.Stream(context.Background(), effortParams(tc.model, tc.effort, false))
			require.NoError(t, err)
			collectEvents(t, ch)
			gotWarns := strings.Count(buf.String(), "suppressed caller temperature")
			require.Equal(t, tc.wantWarns, gotWarns, "log: %s", buf.String())
		})
	}
}

func TestWarnDroppedReasoningEffort_LogsOnlyWhenLevelDropped(t *testing.T) {
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
			got := strings.Contains(buf.String(), "reasoningEffort ignored")
			require.Equal(t, tc.want, got, "log: %s", buf.String())
		})
	}
}
