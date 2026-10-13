package platform

import (
	"context"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// phoenixDefinition adds the OpenInference attributes Arize Phoenix reads
// beyond what it derives from the GenAI semantic conventions: a span kind
// on every span, the run's input and output on the root span, and the
// project. Option: project (the Phoenix project; unset uses Phoenix's
// "default" project).
//
// Phoenix accepts OTLP over gRPC and HTTP, so no protocol is required, but
// only OTLP/HTTP can carry an Authorization header to an authenticated
// Phoenix.
var phoenixDefinition = definition{
	options: map[string]bool{"project": false},
	build:   buildPhoenix,
}

const (
	openInferenceSpanKindKey    = "openinference.span.kind"
	openInferenceProjectNameKey = "openinference.project.name"
	openInferenceInputValueKey  = "input.value"
	openInferenceOutputValueKey = "output.value"
	openInferenceMetadataKey    = "metadata"
)

func buildPhoenix(_ context.Context, opts map[string]string, _ Deps) (Profile, error) {
	p := Profile{
		WrapSpanExporter: func(next sdktrace.SpanExporter) sdktrace.SpanExporter {
			return openInferenceExporter{SpanExporter: next}
		},
	}
	if project := opts["project"]; project != "" {
		p.ResourceAttrs = []attribute.KeyValue{attribute.String(openInferenceProjectNameKey, project)}
	}
	return p, nil
}

// openInferenceExporter adds OpenInference attributes to each span at
// export time, when the root span's late-set content attributes are
// present.
type openInferenceExporter struct {
	sdktrace.SpanExporter
}

func (e openInferenceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	decorated := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		decorated[i] = withOpenInference(s)
	}
	return e.SpanExporter.ExportSpans(ctx, decorated)
}

// openInferenceSpan overrides Attributes on an exported span. Embedding
// the interface keeps every other accessor, including the SDK's
// unexported marker method.
type openInferenceSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s openInferenceSpan) Attributes() []attribute.KeyValue { return s.attrs }

func withOpenInference(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	original := s.Attributes()
	values := make(map[attribute.Key]attribute.Value, len(original))
	for _, kv := range original {
		values[kv.Key] = kv.Value
	}

	var added []attribute.KeyValue
	add := func(key, value string) {
		if _, present := values[attribute.Key(key)]; !present && value != "" {
			added = append(added, attribute.String(key, value))
		}
	}
	add(openInferenceSpanKindKey, openInferenceSpanKind(s.Name(), values["gen_ai.operation.name"].AsString()))
	if !s.Parent().IsValid() {
		add(openInferenceInputValueKey, firstUserText(values["gen_ai.input.messages"].AsString()))
		add(openInferenceOutputValueKey, lastAssistantText(values["gen_ai.output.messages"].AsString()))
		add(openInferenceMetadataKey, runMetadata(values["run.id"].AsString(), values["run.mode"].AsString()))
	}
	if len(added) == 0 {
		return s
	}
	attrs := make([]attribute.KeyValue, 0, len(original)+len(added))
	attrs = append(attrs, original...)
	attrs = append(attrs, added...)
	return openInferenceSpan{ReadOnlySpan: s, attrs: attrs}
}

// openInferenceSpanKind maps a span to its OpenInference kind: GenAI
// operations by name, guard and verifier spans by their span names, and
// every other span as a CHAIN step.
func openInferenceSpanKind(name, operation string) string {
	switch operation {
	case "invoke_agent":
		return "AGENT"
	case "chat":
		return "LLM"
	case "execute_tool":
		return "TOOL"
	}
	switch {
	case strings.HasPrefix(name, "guard."):
		return "GUARDRAIL"
	case name == "verifier.verify":
		return "EVALUATOR"
	default:
		return "CHAIN"
	}
}

// genAIMessage is the subset of the GenAI semconv message schema the
// root input and output need.
type genAIMessage struct {
	Role  string `json:"role"`
	Parts []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	} `json:"parts"`
}

func parseMessages(raw string) []genAIMessage {
	if raw == "" {
		return nil
	}
	var msgs []genAIMessage
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil
	}
	return msgs
}

// firstUserText returns the first text part of the first user message.
func firstUserText(raw string) string {
	for _, m := range parseMessages(raw) {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" && p.Content != "" {
				return p.Content
			}
		}
	}
	return ""
}

// lastAssistantText returns the text parts of the last assistant message
// that has any, joined by newlines.
func lastAssistantText(raw string) string {
	msgs := parseMessages(raw)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var texts []string
		for _, p := range msgs[i].Parts {
			if p.Type == "text" && p.Content != "" {
				texts = append(texts, p.Content)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

// runMetadata renders the run identity as the JSON object Phoenix shows in
// a span's metadata panel.
func runMetadata(runID, mode string) string {
	if runID == "" && mode == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{"run_id": runID, "run_mode": mode})
	if err != nil {
		return ""
	}
	return string(b)
}
