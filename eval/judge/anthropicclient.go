package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicAPIVersion     = "2023-06-01"
)

// anthropicClient speaks the Messages API directly so eval stays independent
// of harness/internal and carries no vendor SDK.
type anthropicClient struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string
}

func newAnthropicClient(httpClient *http.Client, baseURL, apiKey, model string) (*anthropicClient, error) {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	endpoint, err := joinEndpoint(baseURL, "/v1/messages")
	if err != nil {
		return nil, err
	}
	return &anthropicClient{http: httpClient, endpoint: endpoint, apiKey: apiKey, model: model}, nil
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	MaxTokens    int                    `json:"max_tokens"`
	Temperature  *float64               `json:"temperature,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicOutputConfig struct {
	Format anthropicOutputFormat `json:"format"`
}

type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete implements JudgeClient.
func (c *anthropicClient) Complete(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
	body := anthropicRequest{
		Model:       c.model,
		System:      req.System,
		Messages:    []anthropicMessage{{Role: "user", Content: req.User}},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if len(req.Schema) > 0 {
		body.OutputConfig = &anthropicOutputConfig{
			Format: anthropicOutputFormat{Type: "json_schema", Schema: req.Schema},
		}
	}

	raw, err := postJSON(ctx, c.http, c.endpoint, map[string]string{
		"x-api-key":         c.apiKey,
		"anthropic-version": anthropicAPIVersion,
	}, body, c.apiKey)
	if err != nil {
		return JudgeResponse{}, err
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return JudgeResponse{}, fmt.Errorf("decode response: %w", err)
	}

	var text strings.Builder
	for _, blk := range ar.Content {
		if blk.Type == "text" {
			text.WriteString(blk.Text)
		}
	}
	return JudgeResponse{
		Text:         text.String(),
		Model:        ar.Model,
		StopReason:   ar.StopReason,
		InputTokens:  ar.Usage.InputTokens,
		OutputTokens: ar.Usage.OutputTokens,
	}, nil
}
