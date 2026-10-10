package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// maxStructuredOutputCorrections is the number of corrective attempts
	// a run gets, in total, after final answers that fail
	// RunConfig.OutputSchema.
	maxStructuredOutputCorrections = 2

	// maxStructuredOutputBytes caps the structured output below any result
	// sink limit an operator sets, so the "done" event that carries it
	// stays well within gRPC message limits.
	maxStructuredOutputBytes = 1 << 20

	// maxStructuredOutputViolationBytes bounds the validation error
	// returned to the model in a corrective attempt.
	maxStructuredOutputViolationBytes = 2048
)

// ValidateOutputSchema compiles RunConfig.OutputSchema, which
// types.ValidateRunConfig can only check structurally.
func ValidateOutputSchema(config *types.RunConfig) error {
	if len(config.OutputSchema) == 0 {
		return nil
	}
	if err := security.CompileJSONSchema(config.OutputSchema); err != nil {
		return fmt.Errorf("outputSchema: %w", err)
	}
	return nil
}

// structuredOutputLimit is the largest structured output a run may
// return: the result sink's final-text limit, capped at
// maxStructuredOutputBytes.
func structuredOutputLimit(config *types.RunConfig) int {
	return min(config.ResultSink.ResolvedMaxFinalAssistantTextBytes(), maxStructuredOutputBytes)
}

// finalAnswerText is the text of the assistant message that ended the
// inner loop; empty when the history does not end with one.
func finalAnswerText(messages []types.Message) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" {
		return ""
	}
	return lastAssistantText(last.Content)
}

// checkStructuredOutput turns a final answer into the document the run
// returns: extracted, compacted, stripped of prototype-pollution keys,
// scrubbed, size-checked, and validated against schema. On failure the
// error is the reason to give the model.
func checkStructuredOutput(text string, schema json.RawMessage, maxBytes int) (json.RawMessage, error) {
	doc, err := extractStructuredOutput(text)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, doc); err != nil {
		return nil, fmt.Errorf("the final message is not a single JSON document: %w", err)
	}
	stripped, _, err := security.StripDangerousKeysFromInput(compact.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the final message is not a single JSON document: %w", err)
	}
	scrubbed := scrubJSONStrings(stripped)
	if len(scrubbed) > maxBytes {
		return nil, fmt.Errorf("the JSON document is %d bytes, over the %d-byte limit; return a smaller document", len(scrubbed), maxBytes)
	}
	if err := security.ValidateJSONSchema(scrubbed, schema); err != nil {
		return nil, errors.New(schemaViolationText(err))
	}
	return scrubbed, nil
}

// extractStructuredOutput returns the JSON document in text, which must
// be either exactly one JSON value or exactly one fenced code block
// (untagged or tagged "json") with nothing around it.
func extractStructuredOutput(text string) ([]byte, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, errors.New("the final message has no text; it must be a single JSON document")
	}
	if strings.HasPrefix(t, "```") {
		newline := strings.IndexByte(t, '\n')
		if newline < 0 {
			return nil, errors.New("the fenced block has no content")
		}
		if lang := strings.TrimSpace(t[3:newline]); lang != "" && !strings.EqualFold(lang, "json") {
			return nil, fmt.Errorf("the fenced block is tagged %q; tag it json or send the bare document", lang)
		}
		body, closed := strings.CutSuffix(t[newline+1:], "```")
		if !closed {
			return nil, errors.New("the final message must be only the fenced JSON block, with nothing after its closing fence")
		}
		t = strings.TrimSpace(body)
	}
	if !json.Valid([]byte(t)) {
		var v any
		err := json.Unmarshal([]byte(t), &v)
		hint := ""
		if strings.Contains(t, "```") || (!strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[")) {
			hint = "; send only the JSON document, or a single ```json fenced block with nothing around it"
		}
		return nil, fmt.Errorf("the final message is not a single JSON document: %v%s", err, hint)
	}
	return []byte(t), nil
}

// schemaViolationText is a validation error without the validator's
// header line, leaving one "- at '<pointer>': <reason>" line per
// violation.
func schemaViolationText(err error) string {
	msg := err.Error()
	if _, details, ok := strings.Cut(msg, "\n"); ok && strings.TrimSpace(details) != "" {
		return "the JSON document does not satisfy the schema:\n" + details
	}
	return msg
}

// structuredOutputViolation bounds and scrubs a rejection reason before
// it is returned to the model or recorded as the run error.
func structuredOutputViolation(err error) string {
	capped, _ := types.CapFinalAssistantText(security.Scrub(err.Error()), maxStructuredOutputViolationBytes)
	return capped
}

// structuredOutputCorrection is the user turn that asks the model to
// replace a rejected final answer.
func structuredOutputCorrection(violation string) types.Message {
	return types.Message{
		Role:      "user",
		Synthetic: true,
		Content: []types.ContentBlock{{
			Type: "text",
			Text: "Your final message did not satisfy the required output schema: " + violation +
				"\n\nReply with only a JSON document that conforms to the schema in <output_schema> (a single ```json fenced block is also accepted).",
		}},
	}
}

// scrubJSONStrings applies security.Scrub to every string literal (keys
// and values) of a compact, valid JSON document, re-encoding only the
// literals it changes, so the result stays valid JSON with the original
// key order and number spelling.
func scrubJSONStrings(doc []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(doc))
	for i := 0; i < len(doc); {
		if doc[i] != '"' {
			out.WriteByte(doc[i])
			i++
			continue
		}
		end := i + 1
		for end < len(doc) && doc[end] != '"' {
			if doc[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(doc) {
			out.Write(doc[i:])
			break
		}
		literal := doc[i : end+1]
		i = end + 1

		var s string
		if err := json.Unmarshal(literal, &s); err != nil {
			out.Write(literal)
			continue
		}
		scrubbed := security.Scrub(s)
		if scrubbed == s {
			out.Write(literal)
			continue
		}
		var enc bytes.Buffer
		e := json.NewEncoder(&enc)
		e.SetEscapeHTML(false)
		if err := e.Encode(scrubbed); err != nil {
			out.Write(literal)
			continue
		}
		out.Write(bytes.TrimSuffix(enc.Bytes(), []byte("\n")))
	}
	return out.Bytes()
}
