package quirks

// ToolExamplesCapability declares whether a resolved (provider, model) pair
// accepts the JSON-Schema `examples` keyword inside a tool's parameters
// object. Gemini's function-declaration Schema dialect rejects it (see
// GeminiBehaviourFlags.SchemaUnsupportedFeatures); the tool description
// remains the fallback carrier there. The zero value advertises no
// support, so the schema is emitted unchanged.
type ToolExamplesCapability struct {
	// Supported is the master switch; when false the adapter serialises
	// the schema as-is without folding in InputExamples.
	Supported bool `json:"supported"`
}
