package prompt

// NewOverridePromptBuilder returns a ComposedPromptBuilder that uses a fixed
// system prompt preamble instead of the mode-based selection. The remaining
// fragments (workspace path, turn budget, workspace tree, git status, and
// dynamic context with untrusted_context wrapping) are still appended, so
// the harness's structural sections are preserved regardless of the override.
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
