package builtins

import (
	"fmt"
	"unicode/utf8"
)

// maxLineBytes caps a single line returned by read_file or grep_files so a
// minified bundle or single-line data file cannot slip past their line
// limits.
const maxLineBytes = 4 << 10

const lineTruncationFmt = " [line truncated: %d of %d bytes shown]"

// capLine returns line unchanged when it fits maxLineBytes; otherwise it
// keeps the head on a UTF-8 boundary and appends a marker naming the
// original length, the whole staying within maxLineBytes.
func capLine(line string) string {
	if len(line) <= maxLineBytes {
		return line
	}
	longest := fmt.Sprintf(lineTruncationFmt, len(line), len(line))
	cut := maxLineBytes - len(longest)
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + fmt.Sprintf(lineTruncationFmt, cut, len(line))
}
