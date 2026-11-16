package observability

import "strings"

// openAICompatibleHosts maps the API hosts of vendors that serve the
// OpenAI Chat Completions wire to their gen_ai.provider.name value. A
// host matches exactly or as a subdomain.
var openAICompatibleHosts = []struct {
	suffix   string
	provider string
}{
	{"api.deepseek.com", "deepseek"},
	{"api.groq.com", "groq"},
	{"api.mistral.ai", "mistral"},
	{"api.x.ai", "x_ai"},
	{"openai.azure.com", "azure.ai.openai"},
	{"cognitiveservices.azure.com", "azure.ai.openai"},
}

// GenAIProviderName maps a stirrup provider type to the OTel GenAI
// `gen_ai.provider.name` enum value. serverAddress is the host the
// adapter sends requests to; for `openai-compatible`, the generic adapter
// for vLLM/Ollama/Azure OpenAI/etc., a well-known vendor host selects that
// vendor's value. An unknown type or host falls through to the raw type
// rather than mislabelling telemetry as a specific vendor.
func GenAIProviderName(stirrupType, serverAddress string) string {
	switch stirrupType {
	case "anthropic":
		return "anthropic"
	case "bedrock":
		return "aws.bedrock"
	case "openai-responses":
		return "openai"
	case "gemini":
		return "gcp.vertex_ai"
	case "openai-compatible":
		if provider := openAICompatibleProviderForHost(serverAddress); provider != "" {
			return provider
		}
	}
	return stirrupType
}

func openAICompatibleProviderForHost(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range openAICompatibleHosts {
		if host == h.suffix || strings.HasSuffix(host, "."+h.suffix) {
			return h.provider
		}
	}
	return ""
}
