package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// defaultGeminiSafetyThresholds is the BLOCK_NONE safety configuration
// applied when the operator has not supplied an explicit override. A
// coding harness producing security tooling cannot tolerate false
// positives on legitimate code samples; setting BLOCK_NONE on every
// HARM_CATEGORY_* category is the only sane default. Operators who
// require stricter behaviour pass GeminiSafetySettings on the
// ProviderConfig.
var defaultGeminiSafetyThresholds = []geminiSafetySetting{
	{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_NONE"},
	{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
}

// BuildGenerateContentRequest serialises StreamParams into the Vertex AI
// GenerateContentRequest body. Returns the body bytes and a per-call
// id→name map for tool-use IDs (used to look up the function name when
// emitting tool_result blocks back to Gemini, since Gemini matches by
// name and never echoes the original tool_use_id).
//
// safety is the configured GeminiSafetySettings slice from
// ProviderConfig. When empty, all five HARM_CATEGORY_* default to
// BLOCK_NONE — the secure default for a coding harness. See
// defaultGeminiSafetyThresholds for the full list.
//
// Errors fall into three categories:
//
//   - tool schema conversion failures (propagated from ConvertSchema)
//   - role-mapping invariants (e.g. an assistant message containing a
//     tool_result block, or a tool_result whose ToolUseID has not been
//     seen as a prior tool_use)
//   - JSON marshalling errors (only on programmer error in this package)
//
// The function does not perform safety-setting validation — that is the
// responsibility of the types layer (Wave 1).
func BuildGenerateContentRequest(
	params types.StreamParams,
	safety []types.GeminiSafetySetting,
) (body []byte, toolNameByID map[string]string, err error) {
	contents, toolNameByID, err := translateMessagesGemini(params.System, params.Messages)
	if err != nil {
		return nil, nil, err
	}

	req := generateContentRequest{
		Contents: contents,
	}

	// System instruction: lifted from the leading system text. The first
	// system message in params.System is normalised onto a Content with no
	// role; subsequent role:"system" entries (rare, but supported) are
	// concatenated by translateMessagesGemini.
	if sys := geminiSystemFromMessages(params.System, params.Messages); sys != "" {
		req.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: sys}},
		}
	}

	// Tools: convert each schema; emit a single Tools entry containing all
	// declarations. ConvertSchema errors include the tool name in the
	// returned message so failures point at the right declaration.
	if len(params.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(params.Tools))
		for _, t := range params.Tools {
			params, err := ConvertSchema(t.InputSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("tool %q: %w", t.Name, err)
			}
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
		req.Tools = []geminiTools{{FunctionDeclarations: decls}}
		req.ToolConfig = &geminiToolConfig{
			FunctionCallingConfig: geminiFunctionCallingConfig{
				Mode:                        "AUTO",
				StreamFunctionCallArguments: false,
			},
		}
	}

	// Safety settings: pass-through when configured; otherwise emit the
	// BLOCK_NONE defaults. We always send a non-empty list so the run is
	// not at the mercy of Vertex's server-side default (which is to block
	// medium-and-above on several categories).
	req.SafetySettings = buildGeminiSafetySettings(safety)

	// Generation config: only emit fields that were actually set. A nil
	// StreamParams.Temperature means "use Gemini's default" and is omitted
	// from the wire; a non-nil pointer (including an explicit 0.0 for
	// greedy decoding) is transmitted verbatim. MaxOutputTokens is
	// guarded separately: the *float64 migration on Temperature made the
	// MaxTokens=0 + Temperature=Float64Ptr(0.0) combination newly
	// reachable, and Vertex AI's documented behaviour on an explicit
	// maxOutputTokens:0 is either a validation error or a hard
	// zero-output cap — neither what the caller wants.
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
// tool_result blocks back to their originating call.
//
// Role mapping rules:
//   - A "user" message with one or more text blocks becomes a single
//     {role:"user"} content with all text concatenated (no separator) into
//     a single Part. This matches Vertex's expectation that one Content
//     represents one logical turn.
//   - A "user" message with tool_result blocks emits a SEPARATE
//     {role:"function"} content per result. Vertex requires this role
//     specifically for functionResponse parts, and the wire format does
//     not allow user-text and function-response parts to share a Content.
//   - An "assistant" message is collapsed into a single {role:"model"}
//     content whose parts preserve the original block ordering. Mixed
//     text and tool_use blocks therefore live in one Content together.
//   - "system" messages are stripped from the message stream and instead
//     contribute to the SystemInstruction (handled in
//     geminiSystemFromMessages).
//
// A user message containing both text and tool_result blocks emits the
// function-response Contents first (mirroring the OpenAI Responses adapter's
// pinned ordering, where function_call_output items precede the next user
// turn's instructions). Without that ordering, Vertex receives a user-text
// turn before a function-response, which violates its own expectations.
func translateMessagesGemini(system string, messages []types.Message) ([]geminiContent, map[string]string, error) {
	_ = system // consumed by geminiSystemFromMessages, not here
	out := make([]geminiContent, 0, len(messages))
	toolNameByID := make(map[string]string)

	for i, msg := range messages {
		switch msg.Role {
		case "system":
			// Handled in geminiSystemFromMessages; skip silently.
			continue

		case "assistant":
			parts := make([]geminiPart, 0, len(msg.Content))
			for j, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text == "" {
						continue
					}
					// ThoughtSignature is round-tripped on assistant text
					// parts when the previous turn captured one (#194).
					// Vertex 2.x never emits the field, so it stays empty
					// in that case and `omitempty` drops it from the
					// serialised request.
					parts = append(parts, geminiPart{
						Text:             block.Text,
						ThoughtSignature: block.ThoughtSignature,
					})
				case "tool_use":
					args := normaliseToolArgs(block.Input)
					// ThoughtSignature is round-tripped on the part
					// carrying the functionCall — the load-bearing case
					// for multi-turn tool exchanges on Gemini 3.x where
					// dropping the blob breaks the model's chain-of-
					// thought continuity (#194).
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
					// Unknown block types on assistant side are ignored —
					// the harness's message construction would never emit
					// them, but a permissive read of the contract is
					// preferable to a hard failure here.
					continue
				}
			}
			if len(parts) == 0 {
				// Empty assistant message: skip rather than emit an empty
				// Content (Vertex rejects empty parts arrays).
				continue
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})

		case "user":
			// Two passes: emit function-response Contents first, then a
			// trailing user-text Content (if any). See the docstring for
			// the rationale.
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
					response := map[string]interface{}{
						"content": block.Content,
					}
					if block.IsError {
						response["error"] = true
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
			// Unknown role: skip rather than fail. Future message roles
			// (e.g. for multi-agent transcripts) should not break the
			// adapter, and the agentic loop already restricts the role
			// values it produces.
			continue
		}
	}
	return out, toolNameByID, nil
}

// geminiSystemFromMessages composes the systemInstruction text. The
// authoritative source is params.System (PromptBuilder output), but for
// safety we also concatenate any role:"system" messages onto it — some
// callers construct a Messages slice that includes the system prompt as
// the first entry instead of using params.System.
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
