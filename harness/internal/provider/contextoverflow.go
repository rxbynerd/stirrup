package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// openAIContextLengthCode is the error.code OpenAI's Chat Completions and
// Responses APIs send for a request larger than the model's context window.
const openAIContextLengthCode = "context_length_exceeded"

// anthropicContextOverflow reports whether an Anthropic error response says
// the prompt, alone or with max_tokens, exceeds the model's context window.
func anthropicContextOverflow(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Error.Type != "invalid_request_error" {
		return false
	}
	message := strings.ToLower(resp.Error.Message)
	return strings.HasPrefix(message, "prompt is too long") ||
		strings.Contains(message, "exceed context limit")
}

// openAIContextOverflow reports whether an OpenAI-shaped error response
// says the request exceeds the model's context window: by OpenAI's error
// code, or by the "maximum context length" message that compatible servers
// send without one.
func openAIContextOverflow(status int, code, message string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	return code == openAIContextLengthCode ||
		strings.Contains(strings.ToLower(message), "maximum context length")
}

// responsesContextOverflow reports whether an OpenAI Responses error says
// the input exceeds the model's context window.
func responsesContextOverflow(code responsesErrorCode, message string) bool {
	lower := strings.ToLower(message)
	return code == openAIContextLengthCode ||
		strings.Contains(lower, "exceeds the context window") ||
		strings.Contains(lower, "maximum context length")
}

// geminiContextOverflow reports whether a Gemini or Vertex AI error
// response says the input token count exceeds the model's limit.
func geminiContextOverflow(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Error.Status != "INVALID_ARGUMENT" {
		return false
	}
	message := strings.ToLower(resp.Error.Message)
	return strings.Contains(message, "input token count") &&
		strings.Contains(message, "exceeds the maximum")
}

// bedrockContextOverflow reports whether err carries a Bedrock
// ValidationException saying the input is too long for the model.
func bedrockContextOverflow(err error) bool {
	var validation *brtypes.ValidationException
	if !errors.As(err, &validation) {
		return false
	}
	message := strings.ToLower(validation.ErrorMessage())
	return strings.Contains(message, "input is too long") ||
		strings.Contains(message, "prompt is too long") ||
		strings.Contains(message, "too many input tokens")
}
