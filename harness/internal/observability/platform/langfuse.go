package platform

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
)

// langfuseDefinition sends traces over OTLP/HTTP, which is all Langfuse
// ingests, and adds the langfuse.* attributes behind its trace name, tags,
// release, filterable metadata and observation types. Option: host (the
// Langfuse base URL; unset uses traceEmitter.endpoint). Authorization
// stays in traceEmitter.headers.
var langfuseDefinition = definition{
	options: map[string]bool{"host": false},
	build:   buildLangfuse,
}

const (
	langfuseOTLPPath               = "/api/public/otel"
	langfuseIngestionVersionHeader = "x-langfuse-ingestion-version"
	langfuseIngestionVersion       = "4"

	langfuseTraceNameKey          = "langfuse.trace.name"
	langfuseTraceTagsKey          = "langfuse.trace.tags"
	langfuseReleaseKey            = "langfuse.release"
	langfuseObservationTypeKey    = "langfuse.observation.type"
	langfuseTraceMetadataPrefix   = "langfuse.trace.metadata."
	langfuseObservationMetaPrefix = "langfuse.observation.metadata."
)

// stirrupTag names the run span's agent and leads the trace name and tags.
const stirrupTag = "stirrup"

// langfuseTraceMetadata maps each stirrup span attribute to the trace
// metadata key it fills.
var langfuseTraceMetadata = []struct{ from, key string }{
	{"stirrup.control_plane.url", "control_plane_url"},
	{"stirrup.task.id", "task_id"},
	{"stirrup.attempt.id", "attempt_id"},
}

func buildLangfuse(_ context.Context, opts map[string]string, _ Deps) (Profile, error) {
	traces := SignalDefaults{
		Headers: map[string]string{langfuseIngestionVersionHeader: langfuseIngestionVersion},
	}
	if host := opts["host"]; host != "" {
		if err := validateLangfuseHost(host); err != nil {
			return Profile{}, err
		}
		traces.Endpoint = strings.TrimRight(host, "/") + langfuseOTLPPath
	}
	return Profile{
		RequiredProtocol: otlp.ProtocolHTTPProtobuf,
		Signals:          map[otlp.Signal]SignalDefaults{otlp.SignalTraces: traces},
		WrapSpanExporter: func(next sdktrace.SpanExporter) sdktrace.SpanExporter {
			return langfuseExporter{SpanExporter: next}
		},
	}, nil
}

// validateLangfuseHost accepts an http(s) base URL, optionally with a path
// prefix. Errors never echo the value, which could carry userinfo.
func validateLangfuseHost(host string) error {
	u, err := url.Parse(host)
	switch {
	case err != nil:
		return errors.New("host is not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("host must be an http:// or https:// URL")
	case u.Host == "":
		return errors.New("host has no host name")
	case u.User != nil:
		return errors.New("host must not carry userinfo; send credentials in traceEmitter.headers")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("host must not carry a query or fragment")
	case strings.Contains(u.Path, langfuseOTLPPath):
		return errors.New("host must be the Langfuse base URL, without " + langfuseOTLPPath)
	}
	return nil
}

// langfuseExporter adds langfuse.* attributes to each span at export time.
type langfuseExporter struct {
	sdktrace.SpanExporter
}

func (e langfuseExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	decorated := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		decorated[i] = withLangfuse(s)
	}
	return e.SpanExporter.ExportSpans(ctx, decorated)
}

// langfuseSpan overrides Attributes on an exported span, keeping every
// other accessor of the embedded span.
type langfuseSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s langfuseSpan) Attributes() []attribute.KeyValue { return s.attrs }

// withLangfuse returns s with the langfuse.* attributes it lacks. An
// attribute already on the span is never replaced, and Stirrup's own
// metadata keys take precedence over a label of the same name.
func withLangfuse(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	original := s.Attributes()
	values := make(map[attribute.Key]attribute.Value, len(original))
	for _, kv := range original {
		values[kv.Key] = kv.Value
	}

	var added []attribute.KeyValue
	add := func(kv attribute.KeyValue) {
		if _, present := values[kv.Key]; present {
			return
		}
		values[kv.Key] = kv.Value
		added = append(added, kv)
	}
	addString := func(key, value string) {
		if value != "" {
			add(attribute.String(key, value))
		}
	}

	mode := resourceString(s, "harness.run.mode")
	if mode == "" {
		mode = values["run.mode"].AsString()
	}
	tags := []string{stirrupTag}
	if mode != "" {
		tags = append(tags, mode)
	}

	if isRunSpan(values) {
		addString(langfuseTraceNameKey, strings.Join(tags, " "))
	}
	for _, m := range langfuseTraceMetadata {
		addString(langfuseTraceMetadataPrefix+m.key, values[attribute.Key(m.from)].AsString())
	}
	addString(langfuseTraceMetadataPrefix+"run_mode", mode)
	for _, kv := range original {
		label, ok := strings.CutPrefix(string(kv.Key), "stirrup.label.")
		if !ok || label == "" {
			continue
		}
		addString(langfuseTraceMetadataPrefix+label, kv.Value.AsString())
		addString(langfuseObservationMetaPrefix+label, kv.Value.AsString())
	}
	addString(langfuseReleaseKey, resourceString(s, "service.version"))
	add(attribute.StringSlice(langfuseTraceTagsKey, tags))
	addString(langfuseObservationTypeKey, langfuseObservationType(s.Name()))

	if len(added) == 0 {
		return s
	}
	attrs := make([]attribute.KeyValue, 0, len(original)+len(added))
	attrs = append(attrs, original...)
	attrs = append(attrs, added...)
	return langfuseSpan{ReadOnlySpan: s, attrs: attrs}
}

// isRunSpan identifies the run's own invoke_agent span by its agent name,
// so it is found whether or not the span has an upstream parent.
// Sub-agent spans carry a different agent name.
func isRunSpan(values map[attribute.Key]attribute.Value) bool {
	return values["gen_ai.operation.name"].AsString() == "invoke_agent" &&
		values["gen_ai.agent.name"].AsString() == stirrupTag
}

// langfuseObservationType types the component spans Langfuse cannot type
// from gen_ai.operation.name. Other spans keep Langfuse's own typing.
func langfuseObservationType(name string) string {
	switch {
	case strings.HasPrefix(name, "guard."):
		return "guardrail"
	case name == "verifier.verify":
		return "evaluator"
	case name == "context.prepare",
		strings.HasPrefix(name, "hooks."),
		strings.HasPrefix(name, "git."),
		strings.HasPrefix(name, "workspace."):
		return "chain"
	default:
		return ""
	}
}

func resourceString(s sdktrace.ReadOnlySpan, key attribute.Key) string {
	r := s.Resource()
	if r == nil {
		return ""
	}
	v, ok := r.Set().Value(key)
	if !ok {
		return ""
	}
	return v.AsString()
}
