package quirks

// BuiltinRules returns the first-party rule set baked into the harness.
//
// Step 1 of the Wave 2 implementation deliberately ships an empty
// slice: the scaffolding lands without behaviour change so adapters
// integrating with the registry (Steps 2-5) can do so against the
// final shape, and the supporting tests / CLI surface have something
// concrete to exercise.
//
// Rules are added in subsequent steps in this order:
//
//   - Step 2: openai-compatible reasoning-class rules (`o[1-9]-*`,
//     `gpt-5*` with `gpt-5-chat*` carve-out).
//   - Step 3: gemini default rule pinning StreamArgsOff for "*".
//   - Step 4: openai-responses rules (none planned for v1; the empty
//     output struct-tag fix is not a quirk).
//   - Step 5: ReplayFields rules for deepseek-reasoner*, deepseek-v4*,
//     and gemini-3* parse-side recognition.
//
// Operators who want a non-default rule (e.g. Z.ai compat) inject it
// via NewRegistry — see harness/internal/provider/compat/zai for the
// pattern.
func BuiltinRules() []Rule {
	return nil
}
