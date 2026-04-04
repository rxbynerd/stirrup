package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/types"
)

const (
	judgeSystemPrompt = `You are a verification judge. Evaluate the following conversation against the given criteria.

Respond with ONLY a JSON object in this exact format:
{"passed": true, "feedback": "brief explanation"}

- "passed" must be a boolean indicating whether the conversation meets the criteria.
- "feedback" must be a brief explanation of your assessment.

Do not include any text outside the JSON object.`

	judgeMaxTokens   = 1024
	judgeTemperature = 0.0
)

// LLMJudgeVerifier uses an LLM to evaluate whether a conversation meets
// natural-language criteria. This is useful for subjective or complex
// verification that cannot be reduced to a test command exit code.
type LLMJudgeVerifier struct {
	provider provider.ProviderAdapter
	model    string
	criteria string
}

// NewLLMJudgeVerifier creates a verifier that uses the given provider and model
// to judge conversation output against the specified criteria.
func NewLLMJudgeVerifier(prov provider.ProviderAdapter, model string, criteria string) *LLMJudgeVerifier {
	return &LLMJudgeVerifier{
		provider: prov,
		model:    model,
		criteria: criteria,
	}
}

// Verify streams a judging prompt to the LLM and parses the JSON response
// to determine whether the conversation meets the configured criteria.
func (v *LLMJudgeVerifier) Verify(ctx context.Context, vc VerifyContext) (*types.VerificationResult, error) {
	userContent := v.buildUserMessage(vc)

	ch, err := v.provider.Stream(ctx, types.StreamParams{
		Model:       v.model,
		System:      judgeSystemPrompt,
		Messages:    []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: userContent}}}},
		MaxTokens:   judgeMaxTokens,
		Temperature: judgeTemperature,
	})
	if err != nil {
		return nil, fmt.Errorf("llm-judge verifier: stream request failed: %w", err)
	}

	response, err := collectStreamText(ch)
	if err != nil {
		return nil, fmt.Errorf("llm-judge verifier: stream error: %w", err)
	}

	return parseJudgeResponse(response)
}

// buildUserMessage serializes the conversation history and criteria into a
// readable format for the judge model.
func (v *LLMJudgeVerifier) buildUserMessage(vc VerifyContext) string {
	var sb strings.Builder

	sb.WriteString("## Criteria\n\n")
	sb.WriteString(v.criteria)
	sb.WriteString("\n\n## Conversation\n\n")

	for _, msg := range vc.Messages {
		fmt.Fprintf(&sb, "### %s\n\n", msg.Role)
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				sb.WriteString(block.Text)
				sb.WriteString("\n")
			case "tool_use":
				fmt.Fprintf(&sb, "[tool_use: %s]\n", block.Name)
			case "tool_result":
				fmt.Fprintf(&sb, "[tool_result: %s]\n", block.Content)
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// collectStreamText reads all text_delta events from the channel and
// concatenates them into a single response string. Returns an error if
// any error event is received.
func collectStreamText(ch <-chan types.StreamEvent) (string, error) {
	var sb strings.Builder
	for event := range ch {
		switch event.Type {
		case "text_delta":
			sb.WriteString(event.Text)
		case "error":
			if event.Error != nil {
				return "", event.Error
			}
			return "", fmt.Errorf("stream error event with no details")
		}
	}
	return sb.String(), nil
}

// judgeResponse is the expected JSON structure from the judge model.
type judgeResponse struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback"`
}

// parseJudgeResponse attempts to parse the LLM's response as the expected
// JSON format. If parsing fails, it returns a failed result with diagnostic
// feedback rather than an error, since a malformed response is a verification
// outcome (failure) not an infrastructure error.
func parseJudgeResponse(response string) (*types.VerificationResult, error) {
	response = strings.TrimSpace(response)

	var jr judgeResponse
	if err := json.Unmarshal([]byte(response), &jr); err != nil {
		return &types.VerificationResult{
			Passed:   false,
			Feedback: fmt.Sprintf("llm-judge returned malformed response (expected JSON): %s", response),
			Details: map[string]any{
				"rawResponse": response,
				"parseError":  err.Error(),
			},
		}, nil
	}

	return &types.VerificationResult{
		Passed:   jr.Passed,
		Feedback: jr.Feedback,
	}, nil
}
