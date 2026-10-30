package provider

import "github.com/rxbynerd/stirrup/harness/internal/provider/quirks"

// openAICompatibleQuirks resolves model's quirks the way a
// default-constructed OpenAICompatibleAdapter does, endpoint profile
// included, so request-builder tests see the production resolution.
func openAICompatibleQuirks(model string) quirks.ProviderQuirks {
	return openAICompatibleQuirksIn(nil, model)
}

// openAICompatibleQuirksIn is openAICompatibleQuirks over registry; a nil
// registry keeps the adapter's default.
func openAICompatibleQuirksIn(registry *quirks.Registry, model string) quirks.ProviderQuirks {
	a := NewOpenAICompatibleAdapter(nil, "", OpenAIAuthConfig{}, RetryPolicy{})
	if registry != nil {
		a.Registry = registry
	}
	return a.Registry.Resolve(a.quirksTarget(model))
}

// openAIResponsesQuirks is openAICompatibleQuirks for the Responses adapter.
func openAIResponsesQuirks(model string) quirks.ProviderQuirks {
	return openAIResponsesQuirksIn(nil, model)
}

// openAIResponsesQuirksIn is openAIResponsesQuirks over registry; a nil
// registry keeps the adapter's default.
func openAIResponsesQuirksIn(registry *quirks.Registry, model string) quirks.ProviderQuirks {
	a := NewOpenAIResponsesAdapter(nil, "", OpenAIAuthConfig{})
	if registry != nil {
		a.Registry = registry
	}
	return a.Registry.Resolve(a.quirksTarget(model))
}
