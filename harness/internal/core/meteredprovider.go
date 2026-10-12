package core

import (
	"context"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/types"
)

// meteredProvider records the GenAI client operation duration and token
// usage for model calls made outside the agentic loop (context
// summarisation, the llm-judge verifier, the cloud-judge guard). The loop
// records the same instruments itself per attempt, so it must not be given
// a meteredProvider. The adapters already record time to first chunk, which
// keeps all three instruments describing the same calls.
type meteredProvider struct {
	inner        provider.ProviderAdapter
	providerType string
	// metrics is installed once the run's metrics exist; until then calls
	// pass through unrecorded.
	metrics *observability.Metrics
}

var _ provider.ProviderAdapter = (*meteredProvider)(nil)

func newMeteredProvider(inner provider.ProviderAdapter, providerType string) *meteredProvider {
	return &meteredProvider{inner: inner, providerType: providerType}
}

// ServerEndpoint returns the endpoint of the wrapped adapter.
func (m *meteredProvider) ServerEndpoint() (string, int) {
	return provider.ServerEndpointOf(m.inner)
}

// Stream forwards the request and records the call's duration and token
// usage when its stream ends.
func (m *meteredProvider) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	if m.metrics == nil {
		return m.inner.Stream(ctx, params)
	}
	host, port := provider.ServerEndpointOf(m.inner)
	attrs := observability.InferenceAttrs{
		ProviderType:  m.providerType,
		RequestModel:  params.Model,
		ServerAddress: host,
		ServerPort:    port,
	}
	start := time.Now()
	in, err := m.inner.Stream(ctx, params)
	if err != nil {
		m.metrics.RecordInferenceDuration(ctx, attrs, time.Since(start), observability.ToolFailureProviderRequest.String())
		return nil, err
	}

	out := make(chan types.StreamEvent)
	go func() {
		defer close(out)
		var usage types.TokenUsage
		errorType := ""
		forwarding := true
		for ev := range in {
			switch ev.Type {
			case "message_complete":
				usage.MergeEvent(ev)
				if ev.ResponseModel != "" {
					attrs.ResponseModel = boundedResponseModel(ev.ResponseModel)
				}
			case "error":
				errorType = observability.ToolFailureProviderStream.String()
			}
			if !forwarding {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Keep draining so the adapter's producer is not blocked.
				forwarding = false
				errorType = observability.ToolFailureProviderStream.String()
			}
		}
		m.metrics.RecordInferenceDuration(ctx, attrs, time.Since(start), errorType)
		m.metrics.RecordInferenceTokens(ctx, attrs, usage.Input, usage.Output)
	}()
	return out, nil
}
