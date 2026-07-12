package builtins

// The structured result types in this file are the typed envelopes built-in
// tools marshal into ToolResult.Structured (issue #231). They are deliberately
// concrete Go structs rather than map[string]any: the project's no-`any` rule
// (wave-2 design D13) requires every structured producer to declare its shape
// so the JSON contract is reviewable and stable. The text rendering each tool
// already produced is unchanged and remains the canonical fallback; these
// structs are purely additive.

// Kind discriminators identify which struct a ToolResult.Structured payload
// carries (issue #231). B2's MCP bridge and provider adapters route by these
// stable values rather than JSON-sniffing the payload, which would violate the
// typed-not-`any` rule. Each StructuredHandler returns the matching kind.
const (
	kindCommandResult   = "command_result"
	kindSearchResult    = "search_result"
	kindFindResult      = "find_result"
	kindFileExcerpt     = "file_excerpt"
	kindGitStatus       = "git_status"
	kindGitChangedFiles = "git_changed_files"
	kindGitDiff         = "git_diff"
	kindGitShow         = "git_show"
)

// commandResult is the structured payload for run_command. timedOut reports
// whether the command was killed by its timeout: every executor wraps
// executor.ErrTimeout into the error it returns on a genuine deadline expiry
// while still returning whatever partial stdout/stderr it captured (#489), so
// RunCommandTool classifies that case via errors.Is and reports it as a soft
// outcome — timedOut true, no handler error — rather than discarding the
// output. exitCode is executor-dependent when timedOut is true (local.go
// leaves it 0; container.go and k8s_execcore.go set -1), so it carries no
// meaningful status in that case — callers must gate on timedOut and never
// read exitCode as a real exit code when it is true. timeoutSeconds records
// the bound that was in effect either way.
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

// gitStatusEntry is a single working-tree change from git_status, parsed from
// `git status --porcelain=v1`. Staged and Unstaged hold the two porcelain XY
// status letters (a space means unmodified in that column); the raw two-rune
// code is preserved verbatim in Code so consumers that recognise less common
// states (copied, type-changed) are not lossy. OrigPath is set only for
// renames/copies and names the source path.
type gitStatusEntry struct {
	Code     string `json:"code"`
	Staged   string `json:"staged"`
	Unstaged string `json:"unstaged"`
	Path     string `json:"path"`
	OrigPath string `json:"orig_path,omitempty"`
}

// gitStatusResult is the structured payload for git_status. Branch is the
// current branch (or a detached-HEAD marker); Entries is the ordered list of
// working-tree changes, capped at the entry bound. Truncated reports the cap
// was hit. Clean is true when the working tree had no changes. Entries is a
// non-nil slice so the JSON always carries an array.
type gitStatusResult struct {
	Branch    string           `json:"branch"`
	Clean     bool             `json:"clean"`
	Entries   []gitStatusEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}

// gitChangedFile is a single path from git_changed_files with its name-status
// letter (A/M/D/R/C/T). OrigPath is the rename/copy source when Status is R/C.
type gitChangedFile struct {
	Status   string `json:"status"`
	Path     string `json:"path"`
	OrigPath string `json:"orig_path,omitempty"`
}

// gitChangedFilesResult is the structured payload for git_changed_files: the
// ordered list of changed paths and whether the result was capped. Staged
// records whether the staged (index vs HEAD) or unstaged (worktree vs index)
// view was requested. Files is a non-nil slice so the JSON always carries an
// array.
type gitChangedFilesResult struct {
	Staged    bool             `json:"staged"`
	Files     []gitChangedFile `json:"files"`
	Truncated bool             `json:"truncated"`
}

// gitDiffResult is the structured payload for git_diff: the unified diff text,
// bounded by byte and line caps, and the bounds that produced it. Staged
// records whether the staged view (--cached) was requested; Path echoes the
// single-path filter when one was supplied. Truncated reports the diff was cut
// at a bound.
type gitDiffResult struct {
	Diff      string `json:"diff"`
	Staged    bool   `json:"staged"`
	Path      string `json:"path,omitempty"`
	Truncated bool   `json:"truncated"`
}

// gitShowResult is the structured payload for git_show: the bounded output of
// `git show <ref>` (optionally restricted to a single path). Ref echoes the
// requested revision; Path echoes the single-path filter when one was
// supplied. Truncated reports the output was cut at a bound.
type gitShowResult struct {
	Ref       string `json:"ref"`
	Path      string `json:"path,omitempty"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}
