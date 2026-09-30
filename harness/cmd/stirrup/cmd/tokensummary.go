package cmd

import (
	"fmt"
	"strings"
)

// tokenPart is one labelled share of a token count.
type tokenPart struct {
	label string
	n     int
}

// inputTokenDetail renders the cached share of an input count as a
// parenthesised suffix, e.g. " (cache read 1000, cache write 20)", or ""
// when nothing was cached.
func inputTokenDetail(cacheRead, cacheWrite int) string {
	return tokenDetail(tokenPart{"cache read", cacheRead}, tokenPart{"cache write", cacheWrite})
}

// outputTokenDetail renders the reasoning share of an output count, e.g.
// " (reasoning 12)", or "" when none was reported.
func outputTokenDetail(reasoning int) string {
	return tokenDetail(tokenPart{"reasoning", reasoning})
}

func tokenDetail(parts ...tokenPart) string {
	var shown []string
	for _, p := range parts {
		if p.n != 0 {
			shown = append(shown, fmt.Sprintf("%s %d", p.label, p.n))
		}
	}
	if len(shown) == 0 {
		return ""
	}
	return " (" + strings.Join(shown, ", ") + ")"
}
