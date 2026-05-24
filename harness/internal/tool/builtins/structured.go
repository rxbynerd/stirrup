package builtins

// The structured result types in this file are the typed envelopes built-in
// tools marshal into ToolResult.Structured (issue #231). They are deliberately
// concrete Go structs rather than map[string]any: the project's no-`any` rule
// (wave-2 design D13) requires every structured producer to declare its shape
// so the JSON contract is reviewable and stable. The text rendering each tool
// already produced is unchanged and remains the canonical fallback; these
// structs are purely additive.

// commandResult is the structured payload for run_command. timedOut reports
// whether the command was killed by its timeout; in the current executor
// contract a timeout surfaces as a handler error before the structured payload
// is built, so on the success path timedOut is always false and timeoutSeconds
// records the bound that was in effect. The field is retained so a future
// executor that returns partial output on timeout can populate it without a
// schema change.
type commandResult struct {
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exit_code"`
	TimedOut       bool   `json:"timed_out"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// searchMatch is a single hit from grep_files. Column is 1-indexed and present
// only when the search path can report it; the byte-offset is omitted (column
// 0) for the Go-native walker and the rg path, neither of which currently
// emits column information. Text is the full matched line, verbatim.
type searchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column,omitempty"`
	Text   string `json:"text"`
}

// searchResult is the structured payload for grep_files: the ordered list of
// matches and whether the result was capped by max_results. Matches is an
// empty (non-nil) slice when there were no hits so the JSON always carries a
// "matches" array rather than null.
type searchResult struct {
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// findResult is the structured payload for find_files: the ordered list of
// workspace-relative paths and whether the result was capped by max_results.
type findResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// fileExcerpt is the structured payload for read_file. StartLine and EndLine
// are 1-indexed and inclusive, matching the line numbers in the text
// rendering. Truncated reports whether the file had more lines than the
// returned window (start_line+limit did not reach end of file). Lines holds
// the excerpt content without the line-number prefixes the text rendering
// adds. PastEOF is set when start_line was beyond the end of the file, in
// which case Lines is empty and the text rendering is the past-EOF notice.
type fileExcerpt struct {
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Truncated bool     `json:"truncated"`
	PastEOF   bool     `json:"past_eof,omitempty"`
	Lines     []string `json:"lines"`
}
