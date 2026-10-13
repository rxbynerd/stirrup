package edit

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

const (
	// maxMismatchHintBytes bounds the whole "old_string not found" message.
	maxMismatchHintBytes = 2048
	// similarityThreshold is the minimum window score for reporting a
	// closest region.
	similarityThreshold = 0.6
	// maxScoredOldLines bounds the lines of old_string compared per window,
	// keeping the scan linear in file size for very long old_string values.
	maxScoredOldLines = 32
	// maxQuotedLines and maxQuotedLineBytes bound quoted file text.
	maxQuotedLines     = 3
	maxQuotedLineBytes = 160
)

const notFoundPrefix = "old_string not found in file"

// notFoundMessage builds the error for an old_string with no exact match. It
// diagnoses the mismatch from content, the file text the edit already read,
// and never modifies anything. File text is quoted only for paths that the
// tool guard does not treat as credential material.
func notFoundMessage(path, content, oldString string) string {
	hint := mismatchHint(content, oldString, !security.IsCredentialPath(path))
	return boundMessage(notFoundPrefix + hint)
}

func mismatchHint(content, oldString string, allowQuote bool) string {
	old := strings.TrimSuffix(oldString, "\n")
	if strings.TrimSpace(old) == "" {
		return ""
	}
	oldLines := strings.Split(old, "\n")
	fileLines := strings.Split(content, "\n")

	for _, normalize := range []func(string) string{trimTrailingSpace, collapseSpace} {
		if start, count := findNormalized(fileLines, oldLines, normalize); count > 0 {
			return equivalentMatchHint(fileLines, oldLines, start, count, content, old)
		}
	}
	return closestRegionHint(fileLines, oldLines, allowQuote)
}

func trimTrailingSpace(line string) string {
	return strings.TrimRight(line, " \t\r")
}

func collapseSpace(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

// findNormalized searches for oldLines inside fileLines after applying
// normalize to every line. The first and last old lines may match only part
// of a file line, as in an exact substring search. It returns the zero-based
// index of the first line of the first match and the number of matches.
func findNormalized(fileLines, oldLines []string, normalize func(string) string) (start, count int) {
	normFile := make([]string, len(fileLines))
	for i, l := range fileLines {
		normFile[i] = normalize(l)
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = normalize(l)
	}
	haystack := strings.Join(normFile, "\n")
	needle := strings.Join(normOld, "\n")
	if strings.TrimSpace(needle) == "" {
		return 0, 0
	}

	start = -1
	for from := 0; from <= len(haystack); {
		idx := strings.Index(haystack[from:], needle)
		if idx < 0 {
			break
		}
		if start < 0 {
			start = strings.Count(haystack[:from+idx], "\n")
		}
		count++
		from += idx + len(needle)
	}
	return start, count
}

func equivalentMatchHint(fileLines, oldLines []string, start, count int, content, old string) string {
	kinds := differenceKinds(fileLines[start:min(start+len(oldLines), len(fileLines))], oldLines, content, old)
	var b strings.Builder
	fmt.Fprintf(&b, "; a match exists at %s that differs only in %s", lineRange(start, len(oldLines)), strings.Join(kinds, ", "))
	if count > 1 {
		fmt.Fprintf(&b, " (%d such locations; this is the first)", count)
	}
	b.WriteString(". Copy the exact text from the file and resend; nothing was changed.")
	return b.String()
}

// differenceKinds names how the file lines differ from the old_string lines.
func differenceKinds(fileWindow, oldLines []string, content, old string) []string {
	var kinds []string
	add := func(k string) {
		for _, existing := range kinds {
			if existing == k {
				return
			}
		}
		kinds = append(kinds, k)
	}

	if len(oldLines) > 1 {
		fileCRLF := strings.Contains(content, "\r\n")
		oldCRLF := strings.Contains(old, "\r\n")
		switch {
		case fileCRLF && !oldCRLF:
			add("line endings (the file uses CRLF, old_string uses LF)")
		case oldCRLF && !fileCRLF:
			add("line endings (the file uses LF, old_string uses CRLF)")
		}
	}

	for i, fl := range fileWindow {
		if i >= len(oldLines) {
			break
		}
		a := strings.TrimSuffix(fl, "\r")
		b := strings.TrimSuffix(oldLines[i], "\r")
		if a == b {
			continue
		}
		// The first old line may match the end of its file line and the
		// last old line its start, so edges of a partial line are compared
		// only where the line's text actually ends.
		text := strings.TrimSpace(b)
		atStart := strings.HasPrefix(strings.TrimLeft(a, " \t"), text)
		atEnd := strings.HasSuffix(strings.TrimRight(a, " \t"), text)
		first, last := i == 0, i == len(oldLines)-1
		aligned := strings.TrimSpace(a) == text ||
			(first && atEnd) || (last && atStart) ||
			(first && last && strings.Contains(a, text))
		if !aligned {
			add("spacing within lines")
			continue
		}
		if la, lb := leadingSpace(a), leadingSpace(b); atStart && la != lb {
			add(indentationKind(la, lb))
		}
		if atEnd && trailingSpace(a) != trailingSpace(b) {
			add("trailing whitespace")
		}
	}
	if len(kinds) == 0 {
		kinds = append(kinds, "whitespace")
	}
	return kinds
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

func trailingSpace(s string) string {
	return s[len(strings.TrimRight(s, " \t")):]
}

func indentationKind(fileIndent, oldIndent string) string {
	fileTabs := strings.Contains(fileIndent, "\t")
	oldTabs := strings.Contains(oldIndent, "\t")
	switch {
	case fileTabs && !oldTabs:
		return "indentation (the file indents with tabs, old_string uses spaces)"
	case oldTabs && !fileTabs:
		return "indentation (the file indents with spaces, old_string uses tabs)"
	default:
		return "indentation width"
	}
}

func lineRange(start, n int) string {
	if n <= 1 {
		return fmt.Sprintf("line %d", start+1)
	}
	return fmt.Sprintf("lines %d-%d", start+1, start+n)
}

// closestRegionHint scores every window of the file against the leading
// lines of old_string and reports the best one at or above the similarity
// threshold. The window length is the line count of old_string, so a
// replacement that adds or removes lines scores low.
func closestRegionHint(fileLines, oldLines []string, allowQuote bool) string {
	scored := oldLines
	if len(scored) > maxScoredOldLines {
		scored = scored[:maxScoredOldLines]
	}
	ids := make(map[string]int32)
	oldTokens := make([][]int32, len(scored))
	for i, l := range scored {
		oldTokens[i] = tokenIDs(ids, l)
	}
	fileTokens := make([][]int32, len(fileLines))
	for i, l := range fileLines {
		fileTokens[i] = tokenIDs(ids, l)
	}

	bestStart, bestScore := -1, 0.0
	for i := 0; i+len(scored) <= len(fileLines); i++ {
		if s := windowScore(fileTokens[i:i+len(scored)], oldTokens); s > bestScore {
			bestStart, bestScore = i, s
		}
	}
	if bestStart < 0 || bestScore < similarityThreshold {
		return "; no similar region exists in the file. Read the file again before retrying."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "; the closest region is %s (about %d%% similar) but its content differs from old_string.", lineRange(bestStart, len(oldLines)), int(bestScore*100))
	if allowQuote {
		first := firstDifferingLine(fileLines, oldLines, bestStart)
		end := min(first+maxQuotedLines, bestStart+len(oldLines), len(fileLines))
		b.WriteString(" The file has:")
		for i := first; i < end; i++ {
			fmt.Fprintf(&b, "\n%d: %s", i+1, quoteLine(fileLines[i]))
		}
		b.WriteString("\n")
	} else {
		b.WriteString(" ")
	}
	b.WriteString("Read the file again and resend old_string with the exact text; nothing was changed.")
	return b.String()
}

func firstDifferingLine(fileLines, oldLines []string, start int) int {
	for i := 0; i < len(oldLines) && start+i < len(fileLines); i++ {
		if collapseSpace(fileLines[start+i]) != collapseSpace(oldLines[i]) {
			return start + i
		}
	}
	return start
}

func quoteLine(line string) string {
	line = strings.TrimRight(line, "\r")
	if len(line) <= maxQuotedLineBytes {
		return line
	}
	cut := maxQuotedLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "..."
}

// tokenize splits a line into runs of letters, digits and underscores and
// single punctuation runes, dropping whitespace.
func tokenize(line string) []string {
	var tokens []string
	start := -1
	for i, r := range line {
		word := r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
		if word {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			tokens = append(tokens, line[start:i])
			start = -1
		}
		if !unicode.IsSpace(r) {
			tokens = append(tokens, line[i:i+utf8.RuneLen(r)])
		}
	}
	if start >= 0 {
		tokens = append(tokens, line[start:])
	}
	return tokens
}

// tokenIDs tokenizes a line into interned token ids, sorted ascending.
func tokenIDs(ids map[string]int32, line string) []int32 {
	tokens := tokenize(line)
	out := make([]int32, len(tokens))
	for i, t := range tokens {
		id, ok := ids[t]
		if !ok {
			id = int32(len(ids))
			ids[t] = id
		}
		out[i] = id
	}
	slices.Sort(out)
	return out
}

// windowScore returns the token-weighted Dice similarity between aligned
// file and old_string lines. Lines empty on both sides carry no weight.
func windowScore(fileWindow, oldTokens [][]int32) float64 {
	common, total := 0, 0
	for i, ot := range oldTokens {
		ft := fileWindow[i]
		total += len(ft) + len(ot)
		common += 2 * sortedOverlap(ft, ot)
	}
	if total == 0 {
		return 0
	}
	return float64(common) / float64(total)
}

// sortedOverlap returns the multiset intersection size of two ascending
// slices.
func sortedOverlap(a, b []int32) int {
	overlap := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			overlap++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return overlap
}

// boundMessage caps msg at maxMismatchHintBytes on a rune boundary.
func boundMessage(msg string) string {
	if len(msg) <= maxMismatchHintBytes {
		return msg
	}
	const marker = "..."
	cut := maxMismatchHintBytes - len(marker)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + marker
}
