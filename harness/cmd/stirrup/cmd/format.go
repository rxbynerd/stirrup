package cmd

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// The ANSI escape constants form a closed set — every helper in this
// file emits only these sequences, so stripColor can confidently undo
// the formatting via a fixed series of strings.ReplaceAll calls. A
// single-pass walk over the byte stream would be more general; the
// closed-set approach is preferred here because it keeps the test
// assertions ("no \x1b[ in non-TTY output") exhaustive against a
// concrete list rather than a regex.
const (
	ansiBoldStart  = "\x1b[1m"
	ansiBoldEnd    = "\x1b[22m"
	ansiDimStart   = "\x1b[2m"
	ansiDimEnd     = "\x1b[22m"
	ansiResetStyle = "\x1b[0m"
)

// stderrIsTTY reports whether stderr is connected to a terminal. The
// bare-invocation help surfaces (root.go, harness.go) use this to
// decide whether ANSI formatting is safe to emit. Both surfaces write
// their grouped output to stderr, so checking stderr's fd is correct;
// checking stdout would mis-classify `stirrup harness 2>&1 | cat` as
// a TTY because stdout would still be the parent tty.
//
// Tests replace this seam so the formatted branch is reachable without
// allocating a real PTY.
var stderrIsTTY = func() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// colorEnabled reports whether ANSI formatting should appear in output
// destined for w. The decision combines:
//
//  1. NO_COLOR (per https://no-color.org/): any non-empty value
//     disables colour everywhere, even on a TTY.
//  2. Writer identity: a non-os.Stderr writer (typically a test buffer)
//     is treated as non-TTY so unit tests get deterministic plain text
//     without having to set NO_COLOR.
//  3. The stderrIsTTY seam for the production os.Stderr case.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if w != os.Stderr {
		return false
	}
	return stderrIsTTY()
}

// bold wraps s in ANSI bold-on / bold-off when colour is enabled, or
// returns s unchanged otherwise. Used for section headings in the
// grouped help text. The dual-form bracketing (1m...22m rather than
// 1m...0m) preserves any surrounding dim attribute, which matters when
// a header is nested in a dim-formatted paragraph.
func bold(enabled bool, s string) string {
	if !enabled {
		return s
	}
	return ansiBoldStart + s + ansiBoldEnd
}

// dim wraps s in ANSI faint when colour is enabled, or returns s
// unchanged otherwise. Used for example values in the grouped help so
// the visual emphasis sits on the flag names, not the placeholders.
func dim(enabled bool, s string) string {
	if !enabled {
		return s
	}
	return ansiDimStart + s + ansiDimEnd
}

// stripANSI removes every ANSI sequence this package can emit. The
// helper exists so callers that built an already-formatted string can
// downgrade it to plain text without re-running the format pass — for
// example, when a test captures a literal string and wants to assert
// the plain-text shape regardless of TTY detection.
func stripANSI(s string) string {
	for _, code := range []string{
		ansiBoldStart, ansiBoldEnd, ansiDimStart, ansiDimEnd, ansiResetStyle,
	} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}
