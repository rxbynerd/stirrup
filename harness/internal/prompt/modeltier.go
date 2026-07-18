package prompt

import "path"

// Prompt tiers group models by how much prompting they need. Frontier
// models degrade under over-prescriptive prompts and get lean additions;
// open-weight models benefit from explicit process scaffolding; everything
// else renders the base prompt only, so an unrecognised model is always
// safe.
const (
	TierFrontier   = "frontier"
	TierOpenWeight = "open-weight"
	TierDefault    = "default"
)

// Tier membership is defined here and nowhere else: adding a new model to
// a tier is a one-line change to one of these tables. Globs use path.Match
// with the same semantics as the provider quirks registry — "*" does not
// cross "/", so gateway-prefixed IDs (e.g. "openrouter/gemma-4") need the
// "*/" variants that withPrefixedVariants adds.
var (
	frontierModelGlobs = withPrefixedVariants(
		"claude-fable-5*",
		"claude-mythos-5*",
		"claude-sonnet-5*",
		"claude-opus-4-8*",
		"gpt-5.5*",
		"gpt-5.6*",
	)

	openWeightModelGlobs = withPrefixedVariants(
		"gemma*",
		"glm-*",
		"deepseek*",
		"qwen*",
	)
)

func withPrefixedVariants(globs ...string) []string {
	out := make([]string, 0, len(globs)*2)
	for _, g := range globs {
		out = append(out, g, "*/"+g)
	}
	return out
}

// TemplateData is the data surface system prompt templates render against.
// Both the shipped mode prompts and operator-supplied templates
// (promptBuilder.template) see the same surface. Matching is exposed as
// methods rather than a FuncMap so templates parse once and render
// concurrently, and so a bare text/template.Parse in ValidateRunConfig is
// a faithful syntax check.
type TemplateData struct {
	// Model is the resolved prompt model (see PromptContext.Model).
	Model string
	// Mode is the run mode ("execution", "planning", ...).
	Mode string
}

// ModelIs reports whether the prompt model matches any of the given
// path.Match globs. A malformed glob counts as a non-match rather than an
// error so a typo in one branch cannot take down prompt rendering.
func (d TemplateData) ModelIs(globs ...string) bool {
	return modelMatchesAny(d.Model, globs)
}

// Tier returns the prompt tier for the model: TierFrontier,
// TierOpenWeight, or TierDefault when the model matches neither table.
func (d TemplateData) Tier() string {
	return TierFor(d.Model)
}

// TierFor resolves a model identity to its prompt tier. Exported for the
// trace emitter, which records the tier alongside the prompt model.
func TierFor(model string) string {
	switch {
	case modelMatchesAny(model, frontierModelGlobs):
		return TierFrontier
	case modelMatchesAny(model, openWeightModelGlobs):
		return TierOpenWeight
	default:
		return TierDefault
	}
}

func modelMatchesAny(model string, globs []string) bool {
	if model == "" {
		return false
	}
	for _, g := range globs {
		if ok, err := path.Match(g, model); err == nil && ok {
			return true
		}
	}
	return false
}
