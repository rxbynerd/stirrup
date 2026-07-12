package provider

import "github.com/rxbynerd/stirrup/harness/internal/provider/quirks"

// ruleDescriptions returns the Description field of each rule in the
// supplied slice, preserving order. Returned as a non-nil empty slice
// when the input is empty so the slog.Any attribute renders as `[]`
// rather than `null` — easier to grep and parse downstream.
//
// Shared by every adapter's quirks-application log line (openai.go,
// openai_responses.go, gemini.go, anthropic.go) — it lived only in
// openai.go historically, which meant three other adapters depended on
// an openai-local helper. Consolidated here so it has exactly one
// definition with no adapter-specific home.
func ruleDescriptions(rules []quirks.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Description)
	}
	return out
}
