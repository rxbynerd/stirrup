package provider

import (
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
)

// ServerEndpointer is implemented by adapters that send requests to a
// network endpoint. The host and port label the GenAI client metrics as
// server.address and server.port.
type ServerEndpointer interface {
	ServerEndpoint() (host string, port int)
}

// ServerEndpointOf returns the endpoint an adapter sends requests to, or an
// empty host when the adapter has none or does not report one.
func ServerEndpointOf(p ProviderAdapter) (host string, port int) {
	if e, ok := p.(ServerEndpointer); ok {
		return e.ServerEndpoint()
	}
	return "", 0
}

// ServerEndpoint returns the host and port of the API endpoint.
func (a *AnthropicAdapter) ServerEndpoint() (string, int) {
	return observability.ServerEndpointFromURL(a.baseURL)
}

// ServerEndpoint returns the host and port of the API endpoint.
func (o *OpenAICompatibleAdapter) ServerEndpoint() (string, int) {
	return observability.ServerEndpointFromURL(o.baseURL)
}

// ServerEndpoint returns the host and port of the API endpoint.
func (o *OpenAIResponsesAdapter) ServerEndpoint() (string, int) {
	return observability.ServerEndpointFromURL(o.baseURL)
}

// ServerEndpoint returns the host and port of the Vertex AI endpoint.
func (g *GeminiAdapter) ServerEndpoint() (string, int) {
	return observability.ServerEndpointFromURL(g.buildURL(""))
}

// ServerEndpoint returns the AWS SDK's default Bedrock Runtime host for
// the adapter's resolved region, or an empty host when no region resolved.
func (b *BedrockAdapter) ServerEndpoint() (string, int) {
	if b.region == "" {
		return "", 0
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(b.region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return "bedrock-runtime." + b.region + "." + suffix, 443
}

// ServerEndpoint returns the endpoint of the wrapped adapter.
func (a *NormalizingAdapter) ServerEndpoint() (string, int) {
	return ServerEndpointOf(a.inner)
}

// ServerEndpoint returns the endpoint of the wrapped streaming adapter,
// which shares the API host the batch client talks to.
func (a *BatchAdapter) ServerEndpoint() (string, int) {
	return ServerEndpointOf(a.inner)
}

// inferenceAttrs builds the GenAI client metric attributes for a request
// to the given model; baseURL is empty for adapters with no HTTP endpoint.
func inferenceAttrs(providerType, model, baseURL string) observability.InferenceAttrs {
	host, port := observability.ServerEndpointFromURL(baseURL)
	return observability.InferenceAttrs{
		ProviderType:  providerType,
		RequestModel:  model,
		ServerAddress: host,
		ServerPort:    port,
	}
}
