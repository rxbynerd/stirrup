package prompt

// NewOverridePromptBuilder returns a ComposedPromptBuilder that uses a fixed
// system prompt preamble instead of the mode-based selection, keeping the
// structural fragments (workspace path, turn budget, workspace tree, git
// status, dynamic context).
func NewOverridePromptBuilder(systemPrompt string) *ComposedPromptBuilder {
	return NewComposedPromptBuilder(WithFragments(
		StaticFragment(systemPrompt),
		WorkspacePathFragment(),
		TurnBudgetFragment(),
		WorkspaceTreeFragment(),
		GitStatusFragment(),
		DynamicContextFragment(),
	))
}
