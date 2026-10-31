package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func verdictOutputSchema() *types.OutputSchema {
	return &types.OutputSchema{
		Name:   "verdict",
		Schema: json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string","enum":["allow","deny"]}},"required":["verdict"],"additionalProperties":false}`),
	}
}

func outputSchemaParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:        model,
		Messages:     contractFixtureMessages(),
		MaxTokens:    256,
		OutputSchema: verdictOutputSchema(),
	}
}

// grantedQuirks resolves the built-in quirks for target and advertises the
// OutputSchema capability on top, for adapters whose registry carries no
// rule yet.
func grantedQuirks(target quirks.Target) quirks.ProviderQuirks {
	q := quirks.DefaultRegistry().Resolve(target)
	q.OutputSchema = quirks.OutputSchemaCapability{Supported: true}
	return q
}

// TestProjectOutputSchema_GatesOnCapabilityAndValidity pins the shared
// gate every adapter projects through: a schema reaches the wire only
// when requested, advertised, and valid.
func TestProjectOutputSchema_GatesOnCapabilityAndValidity(t *testing.T) {
	supported := quirks.OutputSchemaCapability{Supported: true}

	t.Run("nil schema projects nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(projectOutputSchema(nil, supported))
	})

	t.Run("unsupported capability projects nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(projectOutputSchema(verdictOutputSchema(), quirks.OutputSchemaCapability{}))
	})

	t.Run("invalid schema projects nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(projectOutputSchema(&types.OutputSchema{Name: "bad name", Schema: json.RawMessage(`{}`)}, supported))
		must.Nil(projectOutputSchema(&types.OutputSchema{Name: "ok", Schema: json.RawMessage(`[]`)}, supported))
	})

	t.Run("supported and valid projects the schema", func(t *testing.T) {
		must := require.New(t)
		s := verdictOutputSchema()
		must.Same(s, projectOutputSchema(s, supported))
	})
}

// TestLogDroppedOutputSchema_LevelsByCause pins that an unadvertised
// control is a debug line (the portable case) and an invalid schema a
// warning, and that neither logs the schema body.
func TestLogDroppedOutputSchema_LevelsByCause(t *testing.T) {
	capture := func(schema *types.OutputSchema, capability quirks.OutputSchemaCapability) string {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logDroppedOutputSchema(context.Background(), logger, "anthropic", "m", schema, capability)
		return buf.String()
	}

	t.Run("nil schema logs nothing", func(t *testing.T) {
		must := require.New(t)
		must.Empty(capture(nil, quirks.OutputSchemaCapability{}))
	})

	t.Run("unsupported logs at debug", func(t *testing.T) {
		must := require.New(t)
		out := capture(verdictOutputSchema(), quirks.OutputSchemaCapability{})
		must.Contains(out, "level=DEBUG")
		must.Contains(out, "no known structured output control")
		must.NotContains(out, "enum")
	})

	t.Run("invalid logs at warn", func(t *testing.T) {
		must := require.New(t)
		out := capture(&types.OutputSchema{Name: "bad name", Schema: json.RawMessage(`{"secret":1}`)}, quirks.OutputSchemaCapability{Supported: true})
		must.Contains(out, "level=WARN")
		must.Contains(out, "invalid schema")
		must.NotContains(out, "secret")
	})

	t.Run("supported and valid logs nothing", func(t *testing.T) {
		must := require.New(t)
		must.Empty(capture(verdictOutputSchema(), quirks.OutputSchemaCapability{Supported: true}))
	})
}

// TestIsRequestRejected_StatusClasses pins which provider errors count as
// the provider refusing the request as sent.
func TestIsRequestRejected_StatusClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 is a rejection", classifyStatusError(errBase, 400, nil), true},
		{"422 is a rejection", classifyStatusError(errBase, 422, nil), true},
		{"401 is a rejection", classifyStatusError(errBase, 401, nil), true},
		{"429 is a rate limit, not a rejection", classifyStatusError(errBase, 429, nil), false},
		{"500 is not a rejection", classifyStatusError(errBase, 500, nil), false},
		{"transport error carries no status", classifyTransportError(errBase), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, IsRequestRejected(tc.err))
		})
	}
}

var errBase = context.DeadlineExceeded

// TestBuildAnthropicRequest_OutputSchemaFormat pins the output_config.format
// shape and that an unruled model sends no output_config at all.
func TestBuildAnthropicRequest_OutputSchemaFormat(t *testing.T) {
	t.Run("granted model emits output_config.format json_schema", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("claude-haiku-5-5")
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})
		body, err := json.Marshal(buildAnthropicRequest(params, true, q))
		must.NoError(err)
		var got struct {
			OutputConfig struct {
				Effort string `json:"effort"`
				Format struct {
					Type   string          `json:"type"`
					Schema json.RawMessage `json:"schema"`
				} `json:"format"`
			} `json:"output_config"`
		}
		must.NoError(json.Unmarshal(body, &got))
		must.Equal("json_schema", got.OutputConfig.Format.Type)
		must.JSONEq(string(verdictOutputSchema().Schema), string(got.OutputConfig.Format.Schema))
		must.Empty(got.OutputConfig.Effort, "effort key must not be emitted empty")
		must.NotContains(string(body), `"effort"`)
	})

	t.Run("ungranted model sends no output_config", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("claude-haiku-4-5")
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})
		body, err := json.Marshal(buildAnthropicRequest(params, true, q))
		must.NoError(err)
		must.NotContains(string(body), "output_config")
	})
}

// TestBuildOpenAIRequest_OutputSchemaResponseFormat pins the Chat
// Completions response_format shape, its round trip, and the ungranted
// no-op.
func TestBuildOpenAIRequest_OutputSchemaResponseFormat(t *testing.T) {
	t.Run("granted model emits strict json_schema response_format", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-5-nano")
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: params.Model})
		req, err := buildOpenAIRequest(params, true, q, nil)
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		var got struct {
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		must.NoError(json.Unmarshal(body, &got))
		must.Equal("json_schema", got.ResponseFormat.Type)
		must.Equal("verdict", got.ResponseFormat.JSONSchema.Name)
		must.True(got.ResponseFormat.JSONSchema.Strict)
		must.JSONEq(string(verdictOutputSchema().Schema), string(got.ResponseFormat.JSONSchema.Schema))

		var back openaiRequest
		must.NoError(json.Unmarshal(body, &back))
		must.NotNil(back.ResponseFormat)
		must.Equal("verdict", back.ResponseFormat.JSONSchema.Name)
		must.NotContains(back.ExtraBodyFields, "response_format")
	})

	t.Run("ungranted model sends no response_format", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-4o")
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: params.Model})
		req, err := buildOpenAIRequest(params, true, q, nil)
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		must.NotContains(string(body), "response_format")
	})

	t.Run("extra body field cannot smuggle response_format", func(t *testing.T) {
		must := require.New(t)
		req := openaiRequest{Model: "m", ExtraBodyFields: map[string]any{"response_format": map[string]any{"type": "text"}}}
		_, err := json.Marshal(req)
		must.ErrorContains(err, "response_format")
	})

	t.Run("malformed response_format fails to decode", func(t *testing.T) {
		must := require.New(t)
		var back openaiRequest
		must.ErrorContains(json.Unmarshal([]byte(`{"model":"m","messages":[],"stream":false,"max_completion_tokens":1,"response_format":"x"}`), &back), "response_format")
	})
}

// TestBuildResponsesRequest_OutputSchemaTextFormat pins the Responses
// text.format shape, its position before prompt_cache_key, and the round
// trip.
func TestBuildResponsesRequest_OutputSchemaTextFormat(t *testing.T) {
	t.Run("granted model emits text.format json_schema", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-5.5")
		params.CacheKey = "k"
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-responses", Model: params.Model})
		req, err := buildResponsesRequest(params, q, nil, "")
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		var got struct {
			Text struct {
				Format struct {
					Type   string          `json:"type"`
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"format"`
			} `json:"text"`
		}
		must.NoError(json.Unmarshal(body, &got))
		must.Equal("json_schema", got.Text.Format.Type)
		must.Equal("verdict", got.Text.Format.Name)
		must.True(got.Text.Format.Strict)
		must.JSONEq(string(verdictOutputSchema().Schema), string(got.Text.Format.Schema))
		textAt := bytes.Index(body, []byte(`"text":{"format"`))
		cacheAt := bytes.Index(body, []byte(`"prompt_cache_key"`))
		must.Positive(textAt)
		must.Positive(cacheAt)
		must.Less(textAt, cacheAt, "prompt_cache_key stays last")

		var back responsesRequest
		must.NoError(json.Unmarshal(body, &back))
		must.NotNil(back.TextFormat)
		must.Equal("verdict", back.TextFormat.Name)
	})

	t.Run("ungranted model sends no text object", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-4o")
		q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-responses", Model: params.Model})
		req, err := buildResponsesRequest(params, q, nil, "")
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		must.NotContains(string(body), `"format"`)
	})

	t.Run("malformed text object fails to decode", func(t *testing.T) {
		must := require.New(t)
		var back responsesRequest
		must.ErrorContains(json.Unmarshal([]byte(`{"model":"m","input":[],"store":false,"text":1}`), &back), "text")
	})
}

// TestBuildGenerateContentRequest_OutputSchemaResponseFormat pins the
// Vertex AI responseFormat array shape, that the deprecated
// responseMimeType / responseSchema pair is not sent, that a schema alone
// is enough to emit generationConfig, and that the built-in registry
// grants no Gemini model.
func TestBuildGenerateContentRequest_OutputSchemaResponseFormat(t *testing.T) {
	type responseFormat struct {
		Text struct {
			MimeType string          `json:"mimeType"`
			Schema   json.RawMessage `json:"schema"`
		} `json:"text"`
	}
	type body struct {
		GenerationConfig struct {
			ResponseFormat []responseFormat `json:"responseFormat"`
			ThinkingConfig json.RawMessage  `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}

	t.Run("granted capability emits a one-element responseFormat array", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gemini-3.8-flash")
		raw, _, err := BuildGenerateContentRequest(params, nil, grantedQuirks(quirks.Target{ProviderType: "gemini", Model: params.Model}))
		must.NoError(err)
		var got body
		must.NoError(json.Unmarshal(raw, &got))
		must.Len(got.GenerationConfig.ResponseFormat, 1)
		must.Equal("APPLICATION_JSON", got.GenerationConfig.ResponseFormat[0].Text.MimeType)
		must.JSONEq(string(verdictOutputSchema().Schema), string(got.GenerationConfig.ResponseFormat[0].Text.Schema))
		must.NotContains(string(raw), "responseMimeType")
		must.NotContains(string(raw), "responseSchema")
	})

	t.Run("schema alone emits generationConfig", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gemini-3.8-flash")
		params.MaxTokens = 0
		params.ReasoningSummary = types.ReasoningSummaryOff
		raw, _, err := BuildGenerateContentRequest(params, nil, grantedQuirks(quirks.Target{ProviderType: "gemini", Model: params.Model}))
		must.NoError(err)
		var got body
		must.NoError(json.Unmarshal(raw, &got))
		must.Len(got.GenerationConfig.ResponseFormat, 1)
		must.Nil(got.GenerationConfig.ThinkingConfig)
		must.NotContains(string(raw), "maxOutputTokens")
	})

	t.Run("built-in registry grants no Gemini model, so nothing is sent", func(t *testing.T) {
		for _, model := range []string{"gemini-3.8-flash", "gemini-2.5-pro"} {
			t.Run(model, func(t *testing.T) {
				must := require.New(t)
				params := outputSchemaParams(model)
				q := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: model})
				raw, _, err := BuildGenerateContentRequest(params, nil, q)
				must.NoError(err)
				must.NotContains(string(raw), "responseFormat")
			})
		}
	})
}

// TestBuildRequest_InvalidOutputSchemaIsDropped pins that an invalid schema
// on a granted model leaves every adapter's body free of a schema field
// rather than failing the request.
func TestBuildRequest_InvalidOutputSchemaIsDropped(t *testing.T) {
	invalid := &types.OutputSchema{Name: "bad name", Schema: json.RawMessage(`{"type":"object"}`)}

	t.Run("anthropic", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("claude-haiku-5-5")
		params.OutputSchema = invalid
		body, err := json.Marshal(buildAnthropicRequest(params, true, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: params.Model})))
		must.NoError(err)
		must.NotContains(string(body), "output_config")
	})

	t.Run("openai-compatible", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-5-nano")
		params.OutputSchema = invalid
		req, err := buildOpenAIRequest(params, true, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-compatible", Model: params.Model}), nil)
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		must.NotContains(string(body), "response_format")
	})

	t.Run("openai-responses", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gpt-5.5")
		params.OutputSchema = invalid
		req, err := buildResponsesRequest(params, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "openai-responses", Model: params.Model}), nil, "")
		must.NoError(err)
		body, err := json.Marshal(req)
		must.NoError(err)
		must.NotContains(string(body), `"format"`)
	})

	t.Run("gemini", func(t *testing.T) {
		must := require.New(t)
		params := outputSchemaParams("gemini-3.8-flash")
		params.OutputSchema = invalid
		raw, _, err := BuildGenerateContentRequest(params, nil, grantedQuirks(quirks.Target{ProviderType: "gemini", Model: params.Model}))
		must.NoError(err)
		must.NotContains(string(raw), "responseFormat")
	})
}
