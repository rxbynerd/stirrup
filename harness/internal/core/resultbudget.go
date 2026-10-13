package core

import (
	"fmt"
	"unicode/utf8"
)

// toolResultTruncationFmt takes the bytes shown, the original byte count,
// the byte offset where the omitted part starts, and a read-more hint.
const toolResultTruncationFmt = "\n[tool result truncated: showing the first %d of %d bytes; the omitted part starts at byte offset %d. %s]"

// boundToolResult cuts output to at most budget bytes, marker included,
// keeping the head on a UTF-8 boundary. It reports whether it cut. Output
// already within budget is returned unchanged, so a bounded result passes
// through a second application untouched.
func boundToolResult(output string, budget int, internalName string) (string, bool) {
	if len(output) <= budget {
		return output, false
	}
	hint := toolResultReadMoreHint(internalName)
	longest := fmt.Sprintf(toolResultTruncationFmt, len(output), len(output), len(output), hint)
	cut := budget - len(longest)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return output[:cut] + fmt.Sprintf(toolResultTruncationFmt, cut, len(output), cut, hint), true
}

func toolResultReadMoreHint(internalName string) string {
	if internalName == "read_file" {
		return "Continue with read_file, setting start_line to the last line shown, or pass a smaller limit"
	}
	return "Narrow the request to see the rest"
}
