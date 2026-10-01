package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// verdictSchemaName names the response_format schema on the wire.
const verdictSchemaName = "verdict"

// openaiClient speaks Chat Completions to OpenAI and OpenAI-compatible
// gateways (OpenRouter, vLLM, LM Studio).
type openaiClient struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string
}

func newOpenAIClient(httpClient *http.Client, baseURL, apiKey, model string) (*openaiClient, error) {
	if baseURL == "" {
		return nil, errors.New("openai-compatible judge requires a base URL")
	}
	endpoint, err := joinEndpoint(baseURL, "/chat/completions")
	if err != nil {
		return nil, err
	}
	return &openaiClient{http: httpClient, endpoint: endpoint, apiKey: apiKey, model: model}, nil
}

// openaiRequest sends max_completion_tokens, the key current OpenAI models
// accept; reasoning models reject the legacy max_tokens.
type openaiRequest struct {
	Model               string                `json:"model"`
	Messages            []openaiMessage       `json:"messages"`
	MaxCompletionTokens int                   `json:"max_completion_tokens"`
	Temperature         *float64              `json:"temperature,omitempty"`
	ResponseFormat      *openaiResponseFormat `json:"response_format,omitempty"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema openaiJSONSchema `json:"json_schema"`
}

type openaiJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type openaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements JudgeClient.
func (c *openaiClient) Complete(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
	body := openaiRequest{
		Model: c.model,
		Messages: []openaiMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         req.Temperature,
	}
	if len(req.Schema) > 0 {
		body.ResponseFormat = &openaiResponseFormat{
			Type:       "json_schema",
			JSONSchema: openaiJSONSchema{Name: verdictSchemaName, Strict: true, Schema: req.Schema},
		}
	}

	headers := map[string]string{}
	if c.apiKey != "" {
		headers["Authorization"] = "Bearer " + c.apiKey
	}
	raw, err := postJSON(ctx, c.http, c.endpoint, headers, body, c.apiKey)
	if err != nil {
		return JudgeResponse{}, err
	}

	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return JudgeResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if or.Error != nil {
		return JudgeResponse{}, fmt.Errorf("provider returned an error: %s", providerText(or.Error.Message, c.apiKey))
	}
	if len(or.Choices) == 0 {
		return JudgeResponse{}, errors.New("provider response contained no choices")
	}

	choice := or.Choices[0]
	stop := mapOpenAIFinishReason(choice.FinishReason)
	if choice.Message.Refusal != "" {
		stop = stopRefusal
	}
	return JudgeResponse{
		Text:         choice.Message.Content,
		Model:        or.Model,
		StopReason:   stop,
		InputTokens:  or.Usage.PromptTokens,
		OutputTokens: or.Usage.CompletionTokens,
	}, nil
}

func mapOpenAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return stopEndTurn
	case "length":
		return stopMaxTokens
	case "content_filter":
		return stopRefusal
	default:
		return reason
	}
}
