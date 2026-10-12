package observability

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OpenTelemetry GenAI client metric names and attribute keys (semantic
// conventions v1.41). Durations are in seconds, unlike the stirrup.*
// histograms, which are in milliseconds.
const (
	metricGenAITokenUsage        = "gen_ai.client.token.usage"
	metricGenAIOperationDuration = "gen_ai.client.operation.duration"
	metricGenAITimeToFirstChunk  = "gen_ai.client.operation.time_to_first_chunk"

	attrGenAIOperationName = "gen_ai.operation.name"
	attrGenAIProviderName  = "gen_ai.provider.name"
	attrGenAIRequestModel  = "gen_ai.request.model"
	attrGenAIResponseModel = "gen_ai.response.model"
	attrGenAITokenType     = "gen_ai.token.type"
	attrServerAddress      = "server.address"
	attrServerPort         = "server.port"
	attrErrorType          = "error.type"

	genAIOperationChat = "chat"
	genAITokenInput    = "input"
	genAITokenOutput   = "output"
)

// Advisory bucket boundaries from the GenAI metrics specification.
var (
	genAITokenBuckets = []float64{
		1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144,
		1048576, 4194304, 16777216, 67108864,
	}
	genAIDurationBuckets = []float64{
		0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12,
		10.24, 20.48, 40.96, 81.92,
	}
)

// InferenceAttrs identifies one model inference for the GenAI client
// metrics. Every field is bounded by configuration or provider response
// shape; run IDs, conversation IDs and tool names are deliberately absent
// so the series count stays proportional to configured providers and models.
type InferenceAttrs struct {
	// ProviderType is the stirrup provider type; it is mapped to the
	// semconv gen_ai.provider.name value.
	ProviderType string
	// RequestModel is the model the harness asked for.
	RequestModel string
	// ResponseModel is the model the provider reported; empty when unknown.
	ResponseModel string
	// ServerAddress and ServerPort identify the provider endpoint; they are
	// omitted when ServerAddress is empty (for example, Bedrock).
	ServerAddress string
	ServerPort    int
}

func (a InferenceAttrs) attributes(extra ...attribute.KeyValue) metric.MeasurementOption {
	attrs := make([]attribute.KeyValue, 0, 6+len(extra))
	attrs = append(attrs,
		attribute.String(attrGenAIOperationName, genAIOperationChat),
		attribute.String(attrGenAIProviderName, GenAIProviderName(a.ProviderType)),
	)
	if a.RequestModel != "" {
		attrs = append(attrs, attribute.String(attrGenAIRequestModel, a.RequestModel))
	}
	if a.ResponseModel != "" {
		attrs = append(attrs, attribute.String(attrGenAIResponseModel, a.ResponseModel))
	}
	if a.ServerAddress != "" {
		attrs = append(attrs, attribute.String(attrServerAddress, a.ServerAddress))
		if a.ServerPort > 0 {
			attrs = append(attrs, attribute.Int(attrServerPort, a.ServerPort))
		}
	}
	attrs = append(attrs, extra...)
	return metric.WithAttributes(attrs...)
}

// ServerEndpointFromURL returns the host and port of rawURL for the
// server.address and server.port attributes. The port defaults from the
// scheme when the URL names none. An unparsable or host-less URL yields an
// empty host.
func ServerEndpointFromURL(rawURL string) (host string, port int) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", 0
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return u.Hostname(), n
		}
		return u.Hostname(), 0
	}
	switch u.Scheme {
	case "https":
		return u.Hostname(), 443
	case "http":
		return u.Hostname(), 80
	}
	return u.Hostname(), 0
}

// RecordInferenceTokens records the gen_ai.client.token.usage points for
// one provider request: input is the total input including cached tokens.
// Both counts are what the provider reported for that request; a count the
// provider did not report (zero) is not recorded.
func (m *Metrics) RecordInferenceTokens(ctx context.Context, a InferenceAttrs, input, output int) {
	if input > 0 {
		m.GenAITokenUsage.Record(ctx, int64(input), a.attributes(attribute.String(attrGenAITokenType, genAITokenInput)))
	}
	if output > 0 {
		m.GenAITokenUsage.Record(ctx, int64(output), a.attributes(attribute.String(attrGenAITokenType, genAITokenOutput)))
	}
}

// RecordInferenceDuration records gen_ai.client.operation.duration for one
// provider request. A non-empty errorType marks a failed request.
func (m *Metrics) RecordInferenceDuration(ctx context.Context, a InferenceAttrs, d time.Duration, errorType string) {
	if errorType == "" {
		m.GenAIOperationDuration.Record(ctx, d.Seconds(), a.attributes())
		return
	}
	m.GenAIOperationDuration.Record(ctx, d.Seconds(), a.attributes(attribute.String(attrErrorType, errorType)))
}

// RecordFirstChunk records the time to the first streamed chunk of one
// provider request on both stirrup.harness.provider_ttfb (milliseconds,
// legacy attributes) and gen_ai.client.operation.time_to_first_chunk
// (seconds, GenAI attributes). Adapters call it from one place so every
// provider reports the measurement identically.
func (m *Metrics) RecordFirstChunk(ctx context.Context, legacy metric.MeasurementOption, a InferenceAttrs, d time.Duration) {
	m.ProviderTTFB.Record(ctx, float64(d.Milliseconds()), legacy)
	m.GenAITimeToFirstChunk.Record(ctx, d.Seconds(), a.attributes())
}
