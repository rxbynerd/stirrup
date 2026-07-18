package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// defaultGeminiSafetyThresholds is the BLOCK_NONE safety configuration
// applied when the operator has not supplied an explicit override.
// See docs/providers.md for the rationale.
var defaultGeminiSafetyThresholds = []geminiSafetySetting{
	{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
}

// BuildGenerateContentRequest serialises StreamParams into the Vertex AI
// GenerateContentRequest body. Returns the body bytes and a per-call
// id→name map for tool-use IDs, used to look up the function name when
// emitting tool_result blocks back to Gemini since Gemini matches by
// name and never echoes the original tool_use_id.
//
// safety is the configured GeminiSafetySettings slice; empty defaults
// all five HARM_CATEGORY_* to BLOCK_NONE.
//
// q carries the resolved per-(provider, model) quirks: it selects the
// wire value for functionCallingConfig.streamFunctionCallArguments and
// gates whether StreamParams.ToolChoice is projected onto
// functionCallingConfig.mode / allowedFunctionNames. A zero-value q
// reproduces the no-quirk default (stream args off, mode AUTO with no
// allow-list).
//
// Errors are tool schema conversion failures, role-mapping invariant
// violations, or JSON marshalling errors. Safety-setting validation is
// the types layer's responsibility.
func BuildGenerateContentRequest(
	params types.StreamParams,
	safety []types.GeminiSafetySetting,
	q quirks.ProviderQuirks,
) (body []byte, toolNameByID map[string]string, err error) {
	contents, toolNameByID, err := translateMessagesGemini(params.System, params.Messages, q.StructuredToolResults)
	if err != nil {
		return nil, nil, err
	}

	req := generateContentRequest{
		Contents: contents,
	}

	if sys := geminiSystemFromMessages(params.System, params.Messages); sys != "" {
		req.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: sys}},
		}
	}

	// Lint runs before ConvertSchema so the operator sees the
	// model-scoped policy rejection rather than a structural-rewrite
	// error for the same shape.
	if len(params.Tools) > 0 {
		unsupported := q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures
		decls := make([]geminiFunctionDeclaration, 0, len(params.Tools))
		for _, t := range params.Tools {
			if err := LintGeminiSchema(t.Name, t.InputSchema, unsupported); err != nil {
				return nil, nil, err
			}
			converted, err := ConvertSchema(t.InputSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("tool %q: %w", t.Name, err)
			}
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  converted,
			})
		}
		mode, allowed := geminiToolChoiceFromParams(params, q.ToolChoice)
		req.Tools = []geminiTools{{FunctionDeclarations: decls}}
		req.ToolConfig = &geminiToolConfig{
			FunctionCallingConfig: geminiFunctionCallingConfig{
				Mode:                        mode,
				AllowedFunctionNames:        allowed,
				StreamFunctionCallArguments: streamFunctionCallArgsFromQuirks(q),
			},
		}
	}

	// Always send a non-empty safety-settings list so the run is not at
	// the mercy of Vertex's server-side default (blocks medium-and-above
	// on several categories).
	req.SafetySettings = buildGeminiSafetySettings(safety)

	// MaxOutputTokens is only emitted when > 0: Vertex treats an explicit
	// 0 as either a validation error or a hard zero-output cap, neither
	// of which the caller wants.
	if params.MaxTokens > 0 || params.Temperature != nil {
		gc := &geminiGenerationConfig{}
		if params.MaxTokens > 0 {
			gc.MaxOutputTokens = params.MaxTokens
		}
		if params.Temperature != nil {
			t := *params.Temperature
			gc.Temperature = &t
		}
		req.GenerationConfig = gc
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	return bodyBytes, toolNameByID, nil
}

// translateMessagesGemini converts stirrup's message history into the
// Contents array that Vertex AI expects. Returns the contents plus a map
// from tool_use ID → tool name so the caller can correlate later
// tool_result blocks back to their originating call. Role-mapping rules
// are documented in docs/providers.md.
func translateMessagesGemini(system string, messages []types.Message, cap quirks.StructuredToolResultCapability) ([]geminiContent, map[string]string, error) {
	_ = system // consumed by geminiSystemFromMessages, not here
	out := make([]geminiContent, 0, len(messages))
	toolNameByID := make(map[string]string)

	for i, msg := range messages {
		switch msg.Role {
		case "system":

			continue // handled in geminiSystemFromMessages

		case "assistant":
			parts := make([]geminiPart, 0, len(msg.Content))
			for j, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text == "" {
						continue
					}

					parts = append(parts, geminiPart{
						Text:             block.Text,
						ThoughtSignature: block.ThoughtSignature,
					})
				case "tool_use":
					args := normaliseToolArgs(block.Input)

					parts = append(parts, geminiPart{
						FunctionCall: &geminiFunctionCall{
							Name: block.Name,
							Args: args,
						},
						ThoughtSignature: block.ThoughtSignature,
					})
					if block.ID != "" {
						toolNameByID[block.ID] = block.Name
					}
				case "tool_result":
					return nil, nil, fmt.Errorf("messages[%d].content[%d]: tool_result is not valid on an assistant message", i, j)
				default:

					continue
				}
			}
			if len(parts) == 0 {

				continue // Vertex rejects empty parts arrays
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})

		case "user":
			// Emit function-response Contents first, then a trailing
			// user-text Content, per docs/providers.md.
			textParts := make([]geminiPart, 0)
			for j, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text == "" {
						continue
					}
					textParts = append(textParts, geminiPart{Text: block.Text})
				case "tool_result":
					name, ok := toolNameByID[block.ToolUseID]
					if !ok {
						return nil, nil, fmt.Errorf("messages[%d].content[%d]: tool_result references unknown tool_use_id %q", i, j, block.ToolUseID)
					}
					response, err := geminiToolResultResponse(block, cap)
					if err != nil {
						return nil, nil, fmt.Errorf("messages[%d].content[%d]: %w", i, j, err)
					}
					out = append(out, geminiContent{
						Role: "function",
						Parts: []geminiPart{{
							FunctionResponse: &geminiFunctionResponse{
								Name:     name,
								Response: response,
							},
						}},
					})
				case "tool_use":
					return nil, nil, fmt.Errorf("messages[%d].content[%d]: tool_use is not valid on a user message", i, j)
				default:
					continue
				}
			}
			if len(textParts) > 0 {
				out = append(out, geminiContent{Role: "user", Parts: textParts})
			}

		default:

			continue
		}
	}
	return out, toolNameByID, nil
}

// geminiSystemFromMessages composes the systemInstruction text from
// params.System plus any role:"system" messages, for callers that put
// the system prompt in Messages instead of params.System.
func geminiSystemFromMessages(system string, messages []types.Message) string {
	parts := make([]string, 0, 2)
	if system != "" {
		parts = append(parts, system)
	}
	for _, msg := range messages {
		if msg.Role != "system" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// geminiToolResultResponse marshals one tool_result block into the JSON
// object Vertex expects in functionResponse.response. The canonical text
// always populates "content" (the fallback the model can read regardless
// of structured support). When the resolved capability accepts the
// object-response shape and the block carries a structured envelope, the
// envelope is embedded verbatim under "structured" with its
// discriminator under "kind".
func geminiToolResultResponse(block types.ContentBlock, cap quirks.StructuredToolResultCapability) (json.RawMessage, error) {
	body := geminiFunctionResponseBody{
		Content: block.Content,
		Error:   block.IsError,
	}
	if cap.Supported && cap.ObjectResponse && len(block.Structured) > 0 {
		body.Structured = block.Structured
		body.Kind = block.Kind
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal functionResponse body: %w", err)
	}
	return raw, nil
}

// normaliseToolArgs returns an argument JSON object suitable for Gemini's
// functionCall.args field. Empty or nil input maps to {} so Vertex sees a
// well-formed (if vacuous) argument object rather than a missing field —
// the API rejects functionCall items with no args at all.
func normaliseToolArgs(in json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(in))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}")
	}
	return in
}

// geminiToolChoiceFromParams projects the provider-neutral
// StreamParams.ToolChoice onto Gemini's functionCallingConfig.mode and,
// for the named-tool form, allowedFunctionNames. Gated on the resolved
// capability; falls back to "AUTO" with no allow-list for auto mode,
// unsupported modes, and a named-tool choice with no name.
func geminiToolChoiceFromParams(params types.StreamParams, capability quirks.ToolChoiceCapability) (mode string, allowedFunctionNames []string) {
	if !capability.Supported {
		return "AUTO", nil
	}
	switch params.ToolChoice {
	case types.ToolChoiceRequired:
		if !capability.Required {
			return "AUTO", nil
		}
		return "ANY", nil
	case types.ToolChoiceNone:
		if !capability.None {
			return "AUTO", nil
		}
		return "NONE", nil
	case types.ToolChoiceTool:
		// Gemini expresses "force this one tool" as ANY mode restricted to
		// a single allowed function name. A choice with no name is not
		// expressible, so fall back to AUTO.
		if !capability.NamedTool || params.ToolChoiceName == "" {
			return "AUTO", nil
		}

		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("gemini", params.Model, len(params.ToolChoiceName))
			return "AUTO", nil
		}
		return "ANY", []string{params.ToolChoiceName}
	default:
		return "AUTO", nil
	}
}

// streamFunctionCallArgsFromQuirks projects the resolved
// GeminiStreamArgsShape onto the boolean the wire schema uses for
// functionCallingConfig.streamFunctionCallArguments. Unknown enum
// values conservatively map to false rather than panicking, so an
// older harness build does not crash on a newer rule.
func streamFunctionCallArgsFromQuirks(q quirks.ProviderQuirks) bool {
	switch q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape {
	case quirks.StreamArgsOff:
		return false
	case quirks.StreamArgsV2Snapshot, quirks.StreamArgsV3Deltas:
		return true
	default:
		return false
	}
}

// buildGeminiSafetySettings returns the safety settings list to send on
// the request. When the operator-supplied list is empty we emit
// BLOCK_NONE for every category; otherwise the configured list is used
// verbatim (it has already been validated by the types layer).
func buildGeminiSafetySettings(safety []types.GeminiSafetySetting) []geminiSafetySetting {
	if len(safety) == 0 {
		out := make([]geminiSafetySetting, len(defaultGeminiSafetyThresholds))
		copy(out, defaultGeminiSafetyThresholds)
		return out
	}
	out := make([]geminiSafetySetting, len(safety))
	for i, s := range safety {
		out[i] = geminiSafetySetting{
			Category:  s.Category,
			Threshold: s.Threshold,
		}
	}
	return out
}
