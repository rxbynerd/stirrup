package builtins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// grepFilesSchema is the JSON Schema for the grep_files tool input. The
// pattern field is a Go regular expression (RE2 syntax). include/exclude
// accept shell-style globs and are evaluated against each candidate file's
// path. max_results bounds output so a broad pattern cannot flood the
// model's context.
var grepFilesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "A regular expression (RE2 syntax) to match against file contents."
		},
		"path": {
			"type": "string",
			"description": "Directory to search in, relative to the workspace root. Defaults to the workspace root."
		},
		"include": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Optional. Glob patterns (e.g. '*.go') a file path must match to be considered."
		},
		"exclude": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Optional. Glob patterns a file path must NOT match."
		},
		"max_results": {
			"type": "integer",
			"minimum": 1,
			"maximum": 1000,
			"description": "Optional. Maximum number of matches to return. Defaults to 100."
		}
	},
	"required": ["pattern"],
	"additionalProperties": false
}`)

// findFilesSchema is the JSON Schema for the find_files tool input. The
// name field is a shell-style glob (NOT a regex) — this is the explicit
// type difference from grep_files.
var findFilesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {
			"type": "string",
			"description": "A shell-style glob (filepath.Match syntax) matched against each file's basename only — e.g. '*.go', 'handler_*.ts'. Does not support '**'. To narrow by path, use the include field instead."
		},
		"path": {
			"type": "string",
			"description": "Directory to search in, relative to the workspace root. Defaults to the workspace root."
		},
		"include": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Optional. Glob patterns the full file path must additionally match."
		},
		"exclude": {
			"type": "array",
			"items": {"type": "string"},
			"description": "Optional. Glob patterns the full file path must NOT match."
		},
		"max_results": {
			"type": "integer",
			"minimum": 1,
			"maximum": 1000,
			"description": "Optional. Maximum number of paths to return. Defaults to 100."
		}
	},
	"required": ["name"],
	"additionalProperties": false
}`)

const (
	searchTimeout         = 30 * time.Second
	grepDefaultMaxResults = 100
	grepAbsoluteMax       = 1000
	findDefaultMaxResults = 100
	findAbsoluteMax       = 1000
)

// ripgrepDetector caches the result of probing for `rg` so we only run the
// version check once per process. The detection is best-effort: if the probe
// fails for any reason we fall back to the Go-native implementation. Tests
// can override the probe function before any call to detectRipgrep.
type ripgrepDetector struct {
	once    sync.Once
	present atomic.Bool
	probe   func() bool
}

var defaultRipgrepDetector = &ripgrepDetector{
	probe: probeRipgrepLocal,
}

func (d *ripgrepDetector) detect() bool {
	d.once.Do(func() {
		d.present.Store(d.probe())
	})
	return d.present.Load()
}

// probeRipgrepLocal checks whether `rg` is on PATH and answers --version
// cleanly. We deliberately probe at the host level (exec.LookPath) rather
// than through the workspace executor: the rg binary, when present, lives
// on the host. Container/API executors that lack rg will fall through to
// the Go-native search path regardless of host state because their
// CanExec capability path is separate.
func probeRipgrepLocal() bool {
	// We only want to know whether the binary exists; we do not actually
	// run it here. exec.LookPath is sufficient and avoids spawning a
	// process at import time. Callers that want to spawn rg through the
	// workspace executor do so via the executor.Exec path.
	if _, err := lookPath("rg"); err != nil {
		return false
	}
	return true
}

// lookPath is a seam for tests; the real implementation calls os/exec's
// LookPath. Kept in a separate symbol so tests can stub it without touching
// the os package.
var lookPath = func(name string) (string, error) {
	return execLookPath(name)
}

// GrepFilesTool returns a tool that searches file contents for a regular
// expression. It prefers `rg` (ripgrep) when available — both for speed and
// for its richer ignore-file handling — and falls back to a Go-native
// implementation when rg is not on PATH. Output is structured as
// "path:line:match" lines so a future strict-mode pass (#231) can parse it
// without re-running the search.
func GrepFilesTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "grep_files",
		Description: "Search file contents in the workspace for a regular expression (Go RE2 syntax). Returns one 'path:line:match' line per hit. " +
			"Use this when looking for a string, symbol, or pattern inside files; use find_files instead when searching by filename or extension. " +
			"The pattern is a regex, not a glob — pass '\\bMyFunc\\b' to find the symbol MyFunc, not '*MyFunc*'. " +
			"include and exclude take shell-style globs (e.g. '*.go') applied to candidate paths. Results are capped by max_results (default 100, max 1000); binary files are skipped. " +
			// The `\\\\b` in the example renders as `\b` on the wire (a RE2
			// word boundary). It is double-escaped because the Go string
			// literal first consumes one layer of backslashes, leaving the
			// JSON example with `\\b`, which JSON parsers then decode to
			// `\b` for the regex engine.
			"Example: {\"pattern\": \"func ReadFile\\\\b\", \"include\": [\"*.go\"], \"path\": \"harness/internal\"}",
		// #222 structured example. The `\\b` here matches the runtime description
		// substring (the Go source double-escaped it once); pinned by
		// TestBuiltinInputExamples_MatchDescription.
		InputExamples:     []json.RawMessage{json.RawMessage(`{"pattern": "func ReadFile\\b", "include": ["*.go"], "path": "harness/internal"}`)},
		InputSchema:       grepFilesSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Pattern    string   `json:"pattern"`
				Path       string   `json:"path,omitempty"`
				Include    []string `json:"include,omitempty"`
				Exclude    []string `json:"exclude,omitempty"`
				MaxResults *int     `json:"max_results,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
			}
			if params.Pattern == "" {
				return tool.StructuredResult{}, fmt.Errorf("pattern is required")
			}
			// Compile the pattern early so a bad regex returns a clean
			// error before the executor is even touched. Models often
			// pass shell globs by mistake; the regexp.Compile error
			// message is the clearest signal we can give them.
			re, err := regexp.Compile(params.Pattern)
			if err != nil {
				return tool.StructuredResult{}, fmt.Errorf("invalid regex pattern: %w", err)
			}
			maxResults := grepDefaultMaxResults
			if params.MaxResults != nil {
				if *params.MaxResults < 1 {
					return tool.StructuredResult{}, fmt.Errorf("max_results must be >= 1, got %d", *params.MaxResults)
				}
				if *params.MaxResults > grepAbsoluteMax {
					return tool.StructuredResult{}, fmt.Errorf("max_results must be <= %d, got %d", grepAbsoluteMax, *params.MaxResults)
				}
				maxResults = *params.MaxResults
			}
			searchDir := "."
			if params.Path != "" {
				searchDir = params.Path
			}
			resolvedDir, err := exec.ResolvePath(searchDir)
			if err != nil {
				return tool.StructuredResult{}, fmt.Errorf("resolve search path: %w", err)
			}

			// matches is built directly from source data by whichever search
			// path runs — never re-parsed from rendered text — so a colon in a
			// path or a matched line can never corrupt or silently drop a
			// structured match (issue #231). The text rendering is derived from
			// the same matches afterwards and stays byte-identical to the
			// historical "path:line:text" format.
			// Collect one element past maxResults so genuine truncation is
			// distinguishable from a result count that lands exactly on the
			// cap: with a probe element, len(matches) > maxResults proves more
			// matches existed, whereas == maxResults is a clean fit. The probe
			// is trimmed before serialization so the caller never sees it
			// (issue #341).
			probeMax := maxResults + 1
			var matches []searchMatch
			gotResult := false
			if defaultRipgrepDetector.detect() && exec.Capabilities().CanExec {
				rgMatches, ok, rgErr := grepViaRipgrep(ctx, exec, resolvedDir, params.Pattern, params.Include, params.Exclude, probeMax)
				switch {
				case rgErr != nil && (errors.Is(rgErr, context.Canceled) || errors.Is(rgErr, context.DeadlineExceeded)):
					// Context-cancellation must propagate — the caller asked
					// to stop. Don't paper over it by re-walking natively.
					return tool.StructuredResult{}, rgErr
				case rgErr != nil:
					// Executor transport failure (Docker socket timeout,
					// container restart, etc.) — treat the same as an rg
					// exit code >= 2 and fall through to the native walker.
					// A transient transport flake should not be more fatal
					// than a real rg error.
					slog.WarnContext(ctx, "rg invocation failed, falling back to native grep", "err", rgErr)
				case ok:
					matches = rgMatches
					gotResult = true
				}
				// rg exited with an unexpected error code, or its transport
				// failed; fall through to the Go-native walker so the
				// caller still gets an answer.
			}
			if !gotResult {
				nativeMatches, nativeErr := grepNative(resolvedDir, re, params.Include, params.Exclude, probeMax)
				if nativeErr != nil {
					return tool.StructuredResult{}, nativeErr
				}
				matches = nativeMatches
			}

			truncated := len(matches) > maxResults
			if truncated {
				matches = matches[:maxResults]
			}

			structured, marshalErr := json.Marshal(searchResult{
				Matches:   matchesOrEmpty(matches),
				Truncated: truncated,
			})
			if marshalErr != nil {
				return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
			}
			return tool.StructuredResult{
				Text:       renderGrepText(matches),
				Structured: structured,
				Kind:       kindSearchResult,
			}, nil
		},
	}
}

// noMatchesText is the sentinel both search tools render when nothing matched.
const noMatchesText = "No matches found."

// matchesOrEmpty guarantees a non-nil slice so the marshalled searchResult
// always carries a "matches" array rather than null.
func matchesOrEmpty(m []searchMatch) []searchMatch {
	if m == nil {
		return []searchMatch{}
	}
	return m
}

// renderGrepText produces the canonical "path:line:text" grep rendering from
// typed matches. It is the inverse of how grepNative/grepViaRipgrep build their
// matches and reproduces the historical text byte-for-byte: one
// "path:line:text" line per match joined by newlines, or the no-matches
// sentinel when empty.
func renderGrepText(matches []searchMatch) string {
	if len(matches) == 0 {
		return noMatchesText
	}
	lines := make([]string, len(matches))
	for i, m := range matches {
		lines[i] = fmt.Sprintf("%s:%d:%s", m.Path, m.Line, m.Text)
	}
	return strings.Join(lines, "\n")
}

// FindFilesTool returns a tool that searches for file names matching a
// shell-style glob. It does NOT shell out — name matching is cheap enough
// that walking the workspace tree directly is simpler and works in every
// executor regardless of CanExec.
func FindFilesTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "find_files",
		Description: "Locate files in the workspace whose basenames match a shell-style glob. Returns one workspace-relative path per line. " +
			"Use this when searching by filename or extension; use grep_files instead when searching for a pattern inside file contents. " +
			"The name field is a glob (filepath.Match syntax) matched against each file's basename only — '*.go', 'handler_*.ts' — not a regex and not a path. The '**' segment is not supported in name; use the include filter to narrow by path. " +
			"Results are capped by max_results (default 100, max 1000). " +
			"Example: {\"name\": \"*_test.go\", \"path\": \"harness/internal\", \"exclude\": [\"*/testdata/*\"]}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"name": "*_test.go", "path": "harness/internal", "exclude": ["*/testdata/*"]}`)},
		InputSchema:       findFilesSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Name       string   `json:"name"`
				Path       string   `json:"path,omitempty"`
				Include    []string `json:"include,omitempty"`
				Exclude    []string `json:"exclude,omitempty"`
				MaxResults *int     `json:"max_results,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
			}
			if params.Name == "" {
				return tool.StructuredResult{}, fmt.Errorf("name is required")
			}
			// Validate the glob early so a malformed pattern returns a
			// clean error before any directory walk begins. filepath.Match
			// returns ErrBadPattern for unbalanced brackets, etc.
			if _, err := filepath.Match(params.Name, ""); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("invalid glob pattern %q: %w", params.Name, err)
			}
			maxResults := findDefaultMaxResults
			if params.MaxResults != nil {
				if *params.MaxResults < 1 {
					return tool.StructuredResult{}, fmt.Errorf("max_results must be >= 1, got %d", *params.MaxResults)
				}
				if *params.MaxResults > findAbsoluteMax {
					return tool.StructuredResult{}, fmt.Errorf("max_results must be <= %d, got %d", findAbsoluteMax, *params.MaxResults)
				}
				maxResults = *params.MaxResults
			}
			searchDir := "."
			if params.Path != "" {
				searchDir = params.Path
			}
			resolvedDir, err := exec.ResolvePath(searchDir)
			if err != nil {
				return tool.StructuredResult{}, fmt.Errorf("resolve search path: %w", err)
			}

			// findNative returns the typed path list directly; the text is
			// derived from it so a path containing a newline-free colon (or any
			// other char) cannot diverge the two representations (issue #231).
			// Collect one path past maxResults so a count landing exactly on
			// the cap is not misreported as truncated; the probe path is
			// trimmed before serialization (issue #341).
			paths, err := findNative(resolvedDir, params.Name, params.Include, params.Exclude, maxResults+1)
			if err != nil {
				return tool.StructuredResult{}, err
			}

			truncated := len(paths) > maxResults
			if truncated {
				paths = paths[:maxResults]
			}

			structured, marshalErr := json.Marshal(findResult{
				Paths:     pathsOrEmpty(paths),
				Truncated: truncated,
			})
			if marshalErr != nil {
				return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
			}
			return tool.StructuredResult{
				Text:       renderFindText(paths),
				Structured: structured,
				Kind:       kindFindResult,
			}, nil
		},
	}
}

// pathsOrEmpty guarantees a non-nil slice so the marshalled findResult always
// carries a "paths" array rather than null.
func pathsOrEmpty(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

// renderFindText produces the canonical newline-joined find_files rendering:
// one workspace-relative path per line, or the no-matches sentinel when empty.
func renderFindText(paths []string) string {
	if len(paths) == 0 {
		return noMatchesText
	}
	return strings.Join(paths, "\n")
}

// rgJSONEvent is the subset of ripgrep's `--json` event stream we consume. rg
// emits one JSON object per line; "match" events carry the path, line number,
// and matched line text as structured fields, so a colon in the path or the
// matched text can never be misattributed the way splitting rendered
// "path:line:text" text would. Non-UTF-8 paths/text arrive base64-encoded under
// "bytes" instead of "text"; we decode that so the structured match still
// carries the real bytes.
type rgJSONEvent struct {
	Type string `json:"type"`
	Data struct {
		Path  rgJSONText `json:"path"`
		Lines rgJSONText `json:"lines"`
		Line  int        `json:"line_number"`
	} `json:"data"`
}

// rgJSONText is ripgrep's tagged string: valid UTF-8 under "text", otherwise
// base64-encoded raw bytes under "bytes".
type rgJSONText struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

// value returns the decoded string, preferring the UTF-8 "text" form and
// falling back to base64-decoded "bytes". A bytes value that fails to decode is
// returned verbatim, which only happens on malformed rg output.
func (t rgJSONText) value() string {
	if t.Text != "" {
		return t.Text
	}
	if t.Bytes != "" {
		if raw, err := base64.StdEncoding.DecodeString(t.Bytes); err == nil {
			return string(raw)
		}
		return t.Bytes
	}
	return ""
}

// grepViaRipgrep runs `rg --json` and builds searchMatch structs directly from
// the structured events rather than parsing rendered text. The bool return
// reports whether rg produced a usable answer (true) or hit an unexpected error
// code that warrants falling back to the Go-native walker (false). Error
// returns are reserved for executor failures that the caller surfaces directly.
//
// Reconstructing "path:line:text" from these fields (renderGrepText) is
// byte-identical to rg's `--no-heading --line-number` text output for the
// directory-target invocation used here.
func grepViaRipgrep(ctx context.Context, exec executor.Executor, dir, pattern string, include, exclude []string, maxResults int) ([]searchMatch, bool, error) {
	var args []string
	args = append(args, "rg", "--json", "--color", "never",
		"--max-count", fmt.Sprintf("%d", maxResults),
		"-e", shellQuote(pattern))
	for _, g := range include {
		args = append(args, "--glob", shellQuote(g))
	}
	for _, g := range exclude {
		args = append(args, "--glob", shellQuote("!"+g))
	}
	args = append(args, shellQuote(dir))
	cmd := strings.Join(args, " ")

	result, err := exec.Exec(ctx, cmd, searchTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("rg invocation failed: %w", err)
	}
	// rg exit code 0 == matches found; 1 == no matches (clean); 2+ ==
	// real error (bad regex, IO failure, etc.). We treat 2+ as a soft
	// failure and let the caller fall back to the native walker.
	switch result.ExitCode {
	case 0:
		matches := parseRipgrepJSON(result.Stdout, maxResults)
		return matches, true, nil
	case 1:
		return nil, true, nil
	default:
		return nil, false, nil
	}
}

// parseRipgrepJSON walks rg's newline-delimited JSON event stream and collects
// the "match" events into searchMatch structs, capped at maxResults. rg's
// --max-count is per-file, so a workspace with many files can exceed the global
// bound; the cap here enforces it. Non-"match" events (begin/end/summary) and
// any line that does not parse as a JSON object are skipped — rg only emits
// well-formed objects, so a parse miss is a defensive no-op, never a dropped
// match for a colon-bearing path.
func parseRipgrepJSON(stdout string, maxResults int) []searchMatch {
	var matches []searchMatch
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		var ev rgJSONEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "match" {
			continue
		}
		// rg's matched-line text carries the file's trailing newline; strip a
		// single trailing "\n" so the text matches the per-line rendering the
		// native walker and rg's own text mode produce. A CRLF line keeps its
		// "\r" (the native walker splits on "\n" only), so we do NOT strip it.
		text := strings.TrimSuffix(ev.Data.Lines.value(), "\n")
		matches = append(matches, searchMatch{
			Path: ev.Data.Path.value(),
			Line: ev.Data.Line,
			Text: text,
		})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches
}

// grepNative walks `dir` and matches `re` against each file's contents. It
// is intentionally simple — no concurrency, no mmap — because correctness
// and predictable behaviour in containers without rg matters more than
// throughput. Binary files are skipped (heuristic: NUL byte in the first
// chunk) to avoid spamming gibberish into the model's context.
func grepNative(dir string, re *regexp.Regexp, include, exclude []string, maxResults int) ([]searchMatch, error) {
	var results []searchMatch
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip entries we cannot stat (permissions, race) rather
			// than aborting the whole walk; the model would otherwise
			// lose every other match because of one bad sibling.
			if errors.Is(err, fs.ErrPermission) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// CWE-59: os.ReadFile follows symlinks unconditionally. Skipping
		// symlink entries here closes a workspace-escape: a symlink inside
		// the workspace pointing at /etc/shadow (or any host-readable file)
		// would otherwise have its content surfaced to the model when it
		// matches the search pattern. exec.ResolvePath validates the search
		// root, not individual file reads during the walk; this is the gap
		// it cannot cover.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !pathMatchesFilters(path, dir, include, exclude) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			// Same rationale as above: a single unreadable file should
			// not blank the whole result. We just skip it.
			return nil //nolint:nilerr // intentional skip-on-error to keep partial results
		}
		if looksBinary(data) {
			return nil
		}
		for lineNum, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				rel, relErr := filepath.Rel(dir, path)
				if relErr != nil {
					rel = path
				}
				// Build the typed match directly; the text rendering is
				// derived from these fields by renderGrepText, so a colon in
				// rel or line never confuses path/line/text (issue #231).
				results = append(results, searchMatch{Path: rel, Line: lineNum + 1, Text: line})
				if len(results) >= maxResults {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return nil, fmt.Errorf("search failed: %w", walkErr)
	}
	return results, nil
}

// findNative walks `dir` and collects files whose names match the glob. We
// match against the basename only (consistent with `find -name`), then apply
// the include/exclude glob filters against the relative path. It returns the
// workspace-relative path list directly; the text rendering is derived from it
// by renderFindText.
func findNative(dir, name string, include, exclude []string, maxResults int) ([]string, error) {
	var results []string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		matched, matchErr := filepath.Match(name, d.Name())
		if matchErr != nil {
			return fmt.Errorf("match name pattern: %w", matchErr)
		}
		if !matched {
			return nil
		}
		if !pathMatchesFilters(path, dir, include, exclude) {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		results = append(results, rel)
		if len(results) >= maxResults {
			return errStopWalk
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return nil, fmt.Errorf("find failed: %w", walkErr)
	}
	return results, nil
}

// errStopWalk is the sentinel filepath.WalkDir callbacks return to stop the
// walk early without surfacing as an error. errors.Is comparison against
// this sentinel keeps the contract clear at the call site.
var errStopWalk = errors.New("walk stopped: max results reached")

// pathMatchesFilters applies include/exclude globs against the path. include
// is permissive (empty = match all); exclude is restrictive (any match
// vetoes). Both globs are matched against both the basename and the path
// relative to `root` so callers can write either "*.go" or "internal/**.go".
func pathMatchesFilters(path, root string, include, exclude []string) bool {
	base := filepath.Base(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	for _, g := range exclude {
		if globHit(g, base, rel) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, g := range include {
		if globHit(g, base, rel) {
			return true
		}
	}
	return false
}

// globHit reports whether glob g matches either the basename or the relative
// path. filepath.Match is shell-style (no `**`); a pattern containing `**`
// is converted to a regex-like equivalent that treats `**` as "zero or more
// path segments".
func globHit(g, base, rel string) bool {
	if ok, _ := filepath.Match(g, base); ok {
		return true
	}
	if ok, _ := filepath.Match(g, rel); ok {
		return true
	}
	if strings.Contains(g, "**") {
		if doubleStarMatch(g, rel) {
			return true
		}
	}
	return false
}

// doubleStarMatch translates `**` into a regex `.*` over a path-segment-aware
// matcher. A short helper rather than a dependency on doublestar keeps the
// behaviour explicit at the call site and avoids pulling another module.
//
// Non-wildcard runes are passed through regexp.QuoteMeta so glob patterns
// containing regex metacharacters ([, ], (, ), +, {, }, |, \, ^, $) translate
// to a regex that matches them literally. The previous implementation
// escaped only `.`; any other metacharacter produced either a compile error
// (silently swallowed, so the filter failed open) or a regex with unintended
// semantics (capturing groups, quantifiers). Iteration is by rune so
// multi-byte UTF-8 path segments (e.g. café/**) are not split across the
// pass-through path.
func doubleStarMatch(pattern, path string) bool {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// looksBinary reports whether the first 8 KB of data contains a NUL byte.
// This is the same heuristic git, grep, and ripgrep use; it is not
// foolproof but is enough to keep wide-character text files and machine
// code out of the model's context.
func looksBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// shellQuote wraps a string in single quotes, escaping any embedded single
// quotes. This is sufficient for passing arguments to sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
