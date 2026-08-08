package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// udiffSchema is the JSON Schema for the unified diff edit tool input.
var udiffSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "File path relative to workspace"
		},
		"diff": {
			"type": "string",
			"description": "Unified diff to apply"
		}
	},
	"required": ["path", "diff"],
	"additionalProperties": false
}`)

// defaultFuzzyThreshold is the default minimum similarity ratio (0.0–1.0) for
// fuzzy matching to accept a hunk location. Below this, the hunk is rejected.
const defaultFuzzyThreshold = 0.80

// UdiffStrategy implements EditStrategy by parsing and applying unified diffs.
// It uses a three-level fallback for hunk matching: exact, whitespace-insensitive,
// and fuzzy (Levenshtein edit distance).
type UdiffStrategy struct {
	// fuzzyThreshold is the minimum similarity ratio (0.0–1.0) for fuzzy
	// matching. Defaults to defaultFuzzyThreshold (0.80).
	fuzzyThreshold float64
}

// NewUdiffStrategy creates a new UdiffStrategy with the given fuzzy matching
// threshold. The threshold controls the minimum similarity ratio (0.0–1.0) for
// the Levenshtein-based fuzzy matching fallback. Use defaultFuzzyThreshold (0.80)
// for standard behaviour.
func NewUdiffStrategy(fuzzyThreshold float64) *UdiffStrategy {
	return &UdiffStrategy{fuzzyThreshold: fuzzyThreshold}
}

// ToolDefinition returns the tool definition for the udiff edit strategy.
func (s *UdiffStrategy) ToolDefinition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "apply_diff",
		Description: "Apply a unified diff to a file. The diff format uses @@ hunk headers and +/- line prefixes. Supports exact, whitespace-insensitive, and fuzzy matching.",
		InputSchema: udiffSchema,
	}
}

// Apply parses a unified diff from the input and applies it to the file via the
// executor. Hunks are applied sequentially with a three-level fallback for
// matching: exact → whitespace-insensitive → fuzzy (Levenshtein ≥ configured threshold).
func (s *UdiffStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	var params struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.Path == "" {
		return &EditResult{
			Applied: false,
			Error:   "path is required",
		}, nil
	}
	if params.Diff == "" {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   "diff is required",
		}, nil
	}

	hunks, err := parseUnifiedDiff(params.Diff)
	if err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   fmt.Sprintf("parse diff: %s", err),
		}, nil
	}
	if len(hunks) == 0 {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   "diff contains no hunks",
		}, nil
	}

	isNewFile := isCreationDiff(params.Diff)
	isDeletion := isDeletionDiff(params.Diff)

	var lines []string
	if isNewFile {
		lines = nil
	} else {
		content, readErr := exec.ReadFile(ctx, params.Path)
		if readErr != nil {
			return &EditResult{
				Path:    params.Path,
				Applied: false,
				Error:   fmt.Sprintf("read file: %s", readErr),
			}, nil
		}
		lines = splitLines(content)
	}

	offset := 0
	for i, hunk := range hunks {
		applied, newLines, newOffset, applyErr := s.applyHunk(lines, hunk, offset)
		if applyErr != nil {
			return &EditResult{
				Path:    params.Path,
				Applied: false,
				Error:   fmt.Sprintf("hunk %d: %s", i+1, applyErr),
			}, nil
		}
		if !applied {
			oldContent := hunk.oldLines()
			targetPos := hunk.oldStart - 1 + offset
			lo, hi := hunkSearchWindow(targetPos, len(oldContent), len(lines))
			return &EditResult{
				Path:    params.Path,
				Applied: false,
				Error: fmt.Sprintf(
					"hunk %d: no match within lines %d-%d of the declared start line %d (tried exact, whitespace-insensitive, and fuzzy matching in that range); the file may have changed since the diff was generated — re-read it and regenerate the diff against its current content",
					i+1, lo+1, hi+len(oldContent), hunk.oldStart,
				),
			}, nil
		}
		lines = newLines
		offset = newOffset
	}

	var newContent string
	if isDeletion && len(lines) == 0 {
		newContent = ""
	} else if len(lines) == 0 {
		newContent = ""
	} else {
		newContent = strings.Join(lines, "\n") + "\n"
	}

	if err := exec.WriteFile(ctx, params.Path, newContent); err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	return &EditResult{
		Path:    params.Path,
		Applied: true,
		Diff:    params.Diff,
	}, nil
}

// hunk represents a single hunk from a unified diff.
type hunk struct {
	oldStart int      // 1-based starting line in the original file
	oldCount int      // number of lines from the original file
	newStart int      // 1-based starting line in the new file
	newCount int      // number of lines in the new file
	context  []string // context lines (lines starting with ' ')
	removed  []string // removed lines (lines starting with '-')
	added    []string // added lines (lines starting with '+')

	lines []diffLine // lines in original hunk order, for reconstructing old/new content
}

// diffLine is a single line within a hunk, tagged with its type.
type diffLine struct {
	kind byte   // ' ' for context, '-' for removed, '+' for added
	text string // line content without the prefix
}

// oldLines returns the lines that should exist in the original file for this
// hunk (context lines and removed lines, in order).
func (h *hunk) oldLines() []string {
	out := make([]string, 0, len(h.context)+len(h.removed))
	for _, dl := range h.lines {
		if dl.kind == ' ' || dl.kind == '-' {
			out = append(out, dl.text)
		}
	}
	return out
}

// newLines returns the lines that should replace the old lines after applying
// this hunk (context lines and added lines, in order).
func (h *hunk) newLines() []string {
	out := make([]string, 0, len(h.context)+len(h.added))
	for _, dl := range h.lines {
		if dl.kind == ' ' || dl.kind == '+' {
			out = append(out, dl.text)
		}
	}
	return out
}

// parseUnifiedDiff extracts hunks from a unified diff string.
func parseUnifiedDiff(diff string) ([]hunk, error) {
	rawLines := strings.Split(diff, "\n")
	// Remove trailing empty line from split.
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}

	var hunks []hunk
	i := 0

	// Skip file headers (--- and +++ lines) and any preamble.
	for i < len(rawLines) {
		if strings.HasPrefix(rawLines[i], "@@") {
			break
		}
		i++
	}

	for i < len(rawLines) {
		if !strings.HasPrefix(rawLines[i], "@@") {
			i++
			continue
		}

		h, err := parseHunkHeader(rawLines[i])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		i++

		// Collect hunk body lines until the next hunk header, a file
		// header (defensive: shouldn't appear mid-hunk in a well-formed
		// single-file diff), or end of input.
		for i < len(rawLines) {
			line := rawLines[i]
			if strings.HasPrefix(line, "@@") {
				break
			}

			if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
				break
			}

			if len(line) == 0 {
				// A blank line is a context line whose space prefix was stripped.
				h.lines = append(h.lines, diffLine{kind: ' ', text: ""})
				h.context = append(h.context, "")
				i++
				continue
			}

			prefix := line[0]
			text := line[1:]
			switch prefix {
			case ' ':
				h.lines = append(h.lines, diffLine{kind: ' ', text: text})
				h.context = append(h.context, text)
			case '-':
				h.lines = append(h.lines, diffLine{kind: '-', text: text})
				h.removed = append(h.removed, text)
			case '+':
				h.lines = append(h.lines, diffLine{kind: '+', text: text})
				h.added = append(h.added, text)
			default:
				// No recognized prefix: some diff generators omit the space
				// prefix on context lines.
				h.lines = append(h.lines, diffLine{kind: ' ', text: line})
				h.context = append(h.context, line)
			}
			i++
		}

		// Context+removed must match oldCount and context+added must match
		// newCount, or the header lied about the hunk's shape.
		actualOld := len(h.context) + len(h.removed)
		actualNew := len(h.context) + len(h.added)
		if h.oldCount != actualOld {
			return nil, fmt.Errorf("hunk @@ -%d,%d: expected %d old lines, got %d",
				h.oldStart, h.oldCount, h.oldCount, actualOld)
		}
		if h.newCount != actualNew {
			return nil, fmt.Errorf("hunk @@ +%d,%d: expected %d new lines, got %d",
				h.newStart, h.newCount, h.newCount, actualNew)
		}

		hunks = append(hunks, h)
	}

	return hunks, nil
}

// parseHunkHeader parses a line like "@@ -1,4 +1,6 @@" or "@@ -1,4 +1,6 @@ optional text".
func parseHunkHeader(line string) (hunk, error) {

	line = strings.TrimPrefix(line, "@@")
	closingIdx := strings.Index(line, "@@")
	if closingIdx < 0 {
		return hunk{}, fmt.Errorf("malformed hunk header: missing closing @@")
	}
	inner := strings.TrimSpace(line[:closingIdx])

	parts := strings.SplitN(inner, " ", 2)
	if len(parts) != 2 {
		return hunk{}, fmt.Errorf("malformed hunk header: expected two ranges, got %q", inner)
	}

	oldStart, oldCount, err := parseRange(parts[0], '-')
	if err != nil {
		return hunk{}, fmt.Errorf("malformed hunk header old range: %w", err)
	}
	newStart, newCount, err := parseRange(parts[1], '+')
	if err != nil {
		return hunk{}, fmt.Errorf("malformed hunk header new range: %w", err)
	}

	return hunk{
		oldStart: oldStart,
		oldCount: oldCount,
		newStart: newStart,
		newCount: newCount,
	}, nil
}

// parseRange parses a range like "-1,4" or "+1,6" or "-1" (implied count of 1).
func parseRange(s string, prefix byte) (start, count int, err error) {
	if len(s) == 0 || s[0] != prefix {
		return 0, 0, fmt.Errorf("expected %c prefix, got %q", prefix, s)
	}
	s = s[1:] // strip prefix

	if idx := strings.Index(s, ","); idx >= 0 {
		start, err = strconv.Atoi(s[:idx])
		if err != nil {
			return 0, 0, fmt.Errorf("parse start: %w", err)
		}
		count, err = strconv.Atoi(s[idx+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("parse count: %w", err)
		}
	} else {
		start, err = strconv.Atoi(s)
		if err != nil {
			return 0, 0, fmt.Errorf("parse start: %w", err)
		}
		count = 1
	}
	return start, count, nil
}

// applyHunk attempts to apply a single hunk to the file lines. It tries three
// strategies in order: exact match, whitespace-insensitive, and fuzzy. Returns
// (applied, newLines, newOffset, error).
func (s *UdiffStrategy) applyHunk(lines []string, h hunk, offset int) (bool, []string, int, error) {
	oldContent := h.oldLines()
	newContent := h.newLines()

	// For pure addition hunks (no old lines to match), insert at the target position.
	if len(oldContent) == 0 {
		pos := h.oldStart - 1 + offset
		if pos < 0 {
			pos = 0
		}
		if pos > len(lines) {
			pos = len(lines)
		}
		result := make([]string, 0, len(lines)+len(newContent))
		result = append(result, lines[:pos]...)
		result = append(result, newContent...)
		result = append(result, lines[pos:]...)
		newOffset := offset + len(newContent) - len(oldContent)
		return true, result, newOffset, nil
	}

	// Strategy 1: exact match at the expected position.
	targetPos := h.oldStart - 1 + offset
	if matchExact(lines, oldContent, targetPos) {
		result := spliceLines(lines, targetPos, len(oldContent), newContent)
		newOffset := offset + len(newContent) - len(oldContent)
		return true, result, newOffset, nil
	}

	// The remaining strategies are inexact scans, bounded to a window
	// around the declared position (hunkSearchWindow): an unbounded scan
	// would take the first coincidentally similar match anywhere in the
	// file, silently corrupting the wrong region while still reporting
	// Applied=true.
	lo, hi := hunkSearchWindow(targetPos, len(oldContent), len(lines))

	// Re-try exact match across the window: an earlier hunk in the same
	// diff may have mis-declared its line count, shifting only the offset.
	for pos := lo; pos <= hi; pos++ {
		if pos == targetPos {
			continue
		}
		if matchExact(lines, oldContent, pos) {
			result := spliceLines(lines, pos, len(oldContent), newContent)
			drift := pos - (h.oldStart - 1)
			newOffset := drift + len(newContent) - len(oldContent)
			return true, result, newOffset, nil
		}
	}

	// Strategy 2: whitespace-insensitive match. Context lines come from
	// the original file (preserving its whitespace); only added lines
	// come from the diff.
	for pos := lo; pos <= hi; pos++ {
		if matchWhitespaceInsensitive(lines, oldContent, pos) {
			replacement := buildReplacement(h, lines, pos)
			result := spliceLines(lines, pos, len(oldContent), replacement)
			drift := pos - (h.oldStart - 1)
			newOffset := drift + len(replacement) - len(oldContent)
			return true, result, newOffset, nil
		}
	}

	// Strategy 3: fuzzy match (Levenshtein-based).
	bestPos, bestSim := findFuzzyMatch(lines, oldContent, lo, hi)
	if bestSim >= s.fuzzyThreshold {
		replacement := buildReplacement(h, lines, bestPos)
		result := spliceLines(lines, bestPos, len(oldContent), replacement)
		drift := bestPos - (h.oldStart - 1)
		newOffset := drift + len(replacement) - len(oldContent)
		return true, result, newOffset, nil
	}

	return false, nil, 0, nil
}

// hunkWindowSlack is the fixed number of lines of tolerance added on either
// side of a hunk's declared position when bounding the inexact fallback
// scans. It absorbs the handful of lines of drift that come from an
// earlier hunk in the same diff miscounting its line range, without being
// wide enough to reach an unrelated block elsewhere in the file.
const hunkWindowSlack = 20

// hunkWindowMultiplier scales the search window with hunk size. A longer
// hunk is a more specific pattern — matching it well requires more lines to
// agree — so it can tolerate proportionally more positional drift without
// materially increasing the risk of matching the wrong region.
const hunkWindowMultiplier = 2

// hunkSearchWindow returns the inclusive range [lo, hi] of starting
// positions the whitespace-insensitive and fuzzy fallbacks (and the
// exact-match re-scan) are allowed to search, centred on the hunk's
// declared position (target) and clamped to valid positions within a file
// of lineCount lines holding a pattern of patternLen lines.
func hunkSearchWindow(target, patternLen, lineCount int) (lo, hi int) {
	radius := hunkWindowSlack + hunkWindowMultiplier*patternLen
	lo = target - radius
	if lo < 0 {
		lo = 0
	}
	hi = target + radius
	if maxPos := lineCount - patternLen; hi > maxPos {
		hi = maxPos
	}
	return lo, hi
}

// buildReplacement constructs the replacement lines for a hunk applied at the
// given position. Context lines are taken from the original file (preserving
// the file's actual whitespace/content), while added lines come from the diff.
func buildReplacement(h hunk, fileLines []string, pos int) []string {
	var result []string

	fileIdx := pos
	for _, dl := range h.lines {
		switch dl.kind {
		case ' ':
			// Use the original file's line to preserve exact whitespace.
			if fileIdx < len(fileLines) {
				result = append(result, fileLines[fileIdx])
			}
			fileIdx++
		case '-':

			fileIdx++
		case '+':

			result = append(result, dl.text)
		}
	}
	return result
}

// matchExact checks whether lines[pos:pos+len(pattern)] exactly equals pattern.
func matchExact(lines, pattern []string, pos int) bool {
	if pos < 0 || pos+len(pattern) > len(lines) {
		return false
	}
	for i, p := range pattern {
		if lines[pos+i] != p {
			return false
		}
	}
	return true
}

// matchWhitespaceInsensitive checks whether lines match after trimming
// leading/trailing whitespace from each line.
func matchWhitespaceInsensitive(lines, pattern []string, pos int) bool {
	if pos < 0 || pos+len(pattern) > len(lines) {
		return false
	}
	for i, p := range pattern {
		if strings.TrimSpace(lines[pos+i]) != strings.TrimSpace(p) {
			return false
		}
	}
	return true
}

// findFuzzyMatch slides the pattern over lines[lo:hi+len(pattern)] and
// returns the position with the highest average line-by-line similarity
// ratio. lo and hi bound the search to the hunk's declared region (see
// hunkSearchWindow). If lo > hi (no valid position in range), it reports
// no match.
func findFuzzyMatch(lines, pattern []string, lo, hi int) (bestPos int, bestSim float64) {
	if len(pattern) == 0 || len(lines) < len(pattern) {
		return -1, 0
	}

	bestPos = -1
	bestSim = 0

	for pos := lo; pos <= hi; pos++ {
		sim := blockSimilarity(lines[pos:pos+len(pattern)], pattern)
		if sim > bestSim {
			bestSim = sim
			bestPos = pos
		}
	}

	return bestPos, bestSim
}

// blockSimilarity computes the average per-line similarity between two equally
// sized slices of strings using Levenshtein distance.
func blockSimilarity(a, b []string) float64 {
	if len(a) != len(b) {
		return 0
	}
	total := 0.0
	for i := range a {
		total += lineSimilarity(a[i], b[i])
	}
	return total / float64(len(a))
}

// lineSimilarity computes the similarity ratio between two strings as
// 1 - (levenshteinDistance / max(len(a), len(b))). Returns 1.0 for identical
// strings and 0.0 for completely different strings of equal length.
func lineSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1.0
	}
	dist := levenshtein(a, b)
	return 1.0 - float64(dist)/float64(maxLen)
}

// levenshtein computes the Levenshtein edit distance between two strings.
// Uses the standard two-row dynamic programming approach for O(min(m,n)) space.
func levenshtein(a, b string) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(
				curr[j-1]+1,    // insertion
				prev[j]+1,      // deletion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}

	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// spliceLines replaces lines[pos:pos+count] with replacement.
func spliceLines(lines []string, pos, count int, replacement []string) []string {
	result := make([]string, 0, len(lines)-count+len(replacement))
	result = append(result, lines[:pos]...)
	result = append(result, replacement...)
	result = append(result, lines[pos+count:]...)
	return result
}

// isCreationDiff checks if the diff represents a new file (old file is /dev/null).
func isCreationDiff(diff string) bool {
	for _, line := range strings.SplitN(diff, "\n", 5) {
		if strings.HasPrefix(line, "--- /dev/null") || strings.HasPrefix(line, "--- a/dev/null") {
			return true
		}
	}
	return false
}

// isDeletionDiff checks if the diff represents a file deletion (new file is /dev/null).
func isDeletionDiff(diff string) bool {
	for _, line := range strings.SplitN(diff, "\n", 5) {
		if strings.HasPrefix(line, "+++ /dev/null") || strings.HasPrefix(line, "+++ b/dev/null") {
			return true
		}
	}
	return false
}
