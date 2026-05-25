package quirks

// ToolExamplesCapability declares whether a resolved (provider, model) pair
// accepts the JSON-Schema `examples` keyword inside a tool's parameters
// object (issue #222). It is the cross-provider capability the adapters
// consult before folding types.ToolPresentation.InputExamples into the
// emitted schema.
//
// `examples` is a standard JSON-Schema 2020-12 keyword. OpenAI Chat/Responses,
// Anthropic, and Bedrock pass through arbitrary schema keywords in the
// parameters object, so the structured examples reach the model context
// alongside the schema. Gemini is the exception: its function-declaration
// Schema dialect rejects `examples` (it is listed among
// GeminiBehaviourFlags.SchemaUnsupportedFeatures), so the Gemini rule leaves
// this capability at its zero value and the adapter never folds examples in —
// the #227 description text remains the carrier there.
//
// The zero value advertises NO support: Supported is false. An adapter
// resolving a provider with no examples rule therefore emits the schema
// unchanged, byte-identical to the pre-#222 shape. The example is never lost
// for those providers — it still rides in the tool description, which is sent
// to every provider unconditionally.
type ToolExamplesCapability struct {
	// Supported is the master switch. When false, the adapter MUST NOT fold
	// InputExamples into the emitted parameters object; it serialises the
	// schema as-is.
	Supported bool `json:"supported"`
}
