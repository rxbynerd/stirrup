package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	decisionDefaultBaseURL = "https://api.typesafe.ai"
	decisionDefaultHost    = "api.typesafe.ai"
)

// decisionClient speaks the /v1/systemone decision-model protocol, served by
// TypeSafe, OpenRouter and the open-weight Kev and Decis servers. It shares
// postJSON's retry policy, so 429 and 5xx responses are retried within the
// call's timeout.
type decisionClient struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string

	// sleep waits between retries; nil waits on a timer.
	sleep func(context.Context, time.Duration) error
}

func newDecisionClient(httpClient *http.Client, baseURL, apiKey, model string) (*decisionClient, error) {
	if baseURL == "" {
		baseURL = decisionDefaultBaseURL
	}
	endpoint, err := joinEndpoint(baseURL, "/v1/systemone")
	if err != nil {
		return nil, err
	}
	return &decisionClient{http: httpClient, endpoint: endpoint, apiKey: apiKey, model: model}, nil
}

// decisionQuestion is one typed question. Instructions is a string or an
// object whose fields the question names in backticks.
type decisionQuestion struct {
	Type         string            `json:"type"`
	Instructions any               `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// decisionRequest is the content of one /v1/systemone call; the client adds
// the model.
type decisionRequest struct {
	State     string
	Questions map[string]decisionQuestion
}

type decisionWireRequest struct {
	State     string                      `json:"state"`
	Model     string                      `json:"model"`
	Questions map[string]decisionQuestion `json:"questions"`
}

type decisionWireResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// decisionResponse carries each answer undecoded, keyed by question, for
// the caller to validate against the question it asked.
type decisionResponse struct {
	Model        string
	Answers      map[string]json.RawMessage
	InputTokens  int
	OutputTokens int
}

// decisionRequestBody is the JSON body Decide sends for req.
func decisionRequestBody(model string, req decisionRequest) decisionWireRequest {
	return decisionWireRequest{State: req.State, Model: model, Questions: req.Questions}
}

// Decide sends req and returns the provider's answers.
func (c *decisionClient) Decide(ctx context.Context, req decisionRequest) (decisionResponse, error) {
	headers := map[string]string{}
	if c.apiKey != "" {
		headers["Authorization"] = "Bearer " + c.apiKey
	}
	raw, err := postJSON(ctx, jsonRequest{client: c.http, endpoint: c.endpoint, headers: headers, secret: c.apiKey, sleep: c.sleep},
		decisionRequestBody(c.model, req))
	if err != nil {
		return decisionResponse{}, err
	}
	var wire decisionWireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return decisionResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return decisionResponse{
		Model:        wire.Model,
		Answers:      wire.Answers,
		InputTokens:  wire.Usage.InputTokens,
		OutputTokens: wire.Usage.OutputTokens,
	}, nil
}
