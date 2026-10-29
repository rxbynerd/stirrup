package quirks

// OutputSchemaCapability declares whether a resolved (provider, model)
// pair accepts a JSON Schema constraint on the response text; adapters
// consult it before projecting a types.StreamParams.OutputSchema onto
// their native field (Anthropic `output_config.format`, Chat Completions
// `response_format`, Responses `text.format`, Gemini
// `generationConfig.responseFormat.text`). The zero value advertises no
// support, so an unresolved provider sends no schema and the caller
// falls back to parsing the prompt-only JSON contract.
type OutputSchemaCapability struct {
	// Supported reports that the provider enforces a JSON Schema on the
	// response text for this model.
	Supported bool `json:"supported"`
}
