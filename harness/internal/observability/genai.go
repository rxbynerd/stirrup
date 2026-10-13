package observability

// GenAIProviderName maps stirrup provider type strings to the OTel GenAI
// `gen_ai.provider.name` enum values. Unknown types (including
// `openai-compatible`, the generic adapter for vLLM/Ollama/Azure
// OpenAI/etc.) fall through to the raw value rather than mislabelling
// telemetry as a specific vendor.
func GenAIProviderName(stirrupType string) string {
	switch stirrupType {
	case "anthropic":
		return "anthropic"
	case "bedrock":
		return "aws.bedrock"
	case "openai-responses":
		return "openai"
	case "gemini":
		return "gcp.vertex_ai"
	default:
		return stirrupType
	}
}
