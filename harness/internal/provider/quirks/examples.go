package quirks

// ToolExamplesCapability declares whether a resolved (provider, model) pair
// accepts worked tool-input examples, and where. Gemini's
// function-declaration Schema dialect rejects the `examples` keyword (see
// GeminiBehaviourFlags.SchemaUnsupportedFeatures); the tool description
// remains the fallback carrier there. The zero value advertises no
// support, so the schema is emitted unchanged.
type ToolExamplesCapability struct {
	// Supported is the master switch; when false the adapter serialises
	// the schema as-is without folding in InputExamples.
	Supported bool `json:"supported"`

	// Native selects a dedicated wire field for the examples (Anthropic's
	// tool input_examples) instead of folding them into the schema's
	// `examples` keyword, leaving the schema untouched. Meaningful only
	// when Supported is true.
	Native bool `json:"native"`
}
