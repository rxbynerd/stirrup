package builtins

import (
	"context"
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
			"Example: {\"pattern\": \"func ReadFile\\\\b\", \"include\": [\"*.go\"], \"path\": \"harness/internal\"}",
		InputSchema: grepFilesSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Pattern    string   `json:"pattern"`
				Path       string   `json:"path,omitempty"`
				Include    []string `json:"include,omitempty"`
				Exclude    []string `json:"exclude,omitempty"`
				MaxResults *int     `json:"max_results,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			// Compile the pattern early so a bad regex returns a clean
			// error before the executor is even touched. Models often
			// pass shell globs by mistake; the regexp.Compile error
			// message is the clearest signal we can give them.
			re, err := regexp.Compile(params.Pattern)
			if err != nil {
				return "", fmt.Errorf("invalid regex pattern: %w", err)
			}
			maxResults := grepDefaultMaxResults
			if params.MaxResults != nil {
				if *params.MaxResults < 1 {
					return "", fmt.Errorf("max_results must be >= 1, got %d", *params.MaxResults)
				}
				if *params.MaxResults > grepAbsoluteMax {
					return "", fmt.Errorf("max_results must be <= %d, got %d", grepAbsoluteMax, *params.MaxResults)
				}
				maxResults = *params.MaxResults
			}
			searchDir := "."
			if params.Path != "" {
				searchDir = params.Path
			}
			resolvedDir, err := exec.ResolvePath(searchDir)
			if err != nil {
				return "", fmt.Errorf("resolve search path: %w", err)
			}

			if defaultRipgrepDetector.detect() && exec.Capabilities().CanExec {
				out, ok, err := grepViaRipgrep(ctx, exec, resolvedDir, params.Pattern, params.Include, params.Exclude, maxResults)
				switch {
				case err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
					// Context-cancellation must propagate — the caller asked
					// to stop. Don't paper over it by re-walking natively.
					return "", err
				case err != nil:
					// Executor transport failure (Docker socket timeout,
					// container restart, etc.) — treat the same as an rg
					// exit code >= 2 and fall through to the native walker.
					// A transient transport flake should not be more fatal
					// than a real rg error.
					slog.WarnContext(ctx, "rg invocation failed, falling back to native grep", "err", err)
				case ok:
					return out, nil
				}
				// rg exited with an unexpected error code, or its transport
				// failed; fall through to the Go-native walker so the
				// caller still gets an answer.
			}
			return grepNative(resolvedDir, re, params.Include, params.Exclude, maxResults)
		},
	}
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
		InputSchema: findFilesSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Name       string   `json:"name"`
				Path       string   `json:"path,omitempty"`
				Include    []string `json:"include,omitempty"`
				Exclude    []string `json:"exclude,omitempty"`
				MaxResults *int     `json:"max_results,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Name == "" {
				return "", fmt.Errorf("name is required")
			}
			// Validate the glob early so a malformed pattern returns a
			// clean error before any directory walk begins. filepath.Match
			// returns ErrBadPattern for unbalanced brackets, etc.
			if _, err := filepath.Match(params.Name, ""); err != nil {
				return "", fmt.Errorf("invalid glob pattern %q: %w", params.Name, err)
			}
			maxResults := findDefaultMaxResults
			if params.MaxResults != nil {
				if *params.MaxResults < 1 {
					return "", fmt.Errorf("max_results must be >= 1, got %d", *params.MaxResults)
				}
				if *params.MaxResults > findAbsoluteMax {
					return "", fmt.Errorf("max_results must be <= %d, got %d", findAbsoluteMax, *params.MaxResults)
				}
				maxResults = *params.MaxResults
			}
			searchDir := "."
			if params.Path != "" {
				searchDir = params.Path
			}
			resolvedDir, err := exec.ResolvePath(searchDir)
			if err != nil {
				return "", fmt.Errorf("resolve search path: %w", err)
			}
			return findNative(resolvedDir, params.Name, params.Include, params.Exclude, maxResults)
		},
	}
}

// grepViaRipgrep runs `rg --line-number --no-heading --color never` and
// converts the result. The bool return reports whether rg produced a usable
// answer (true) or hit an unexpected error code that warrants falling back
// to the Go-native walker (false). Error returns are reserved for executor
// failures that the caller surfaces directly.
func grepViaRipgrep(ctx context.Context, exec executor.Executor, dir, pattern string, include, exclude []string, maxResults int) (string, bool, error) {
	var args []string
	args = append(args, "rg", "--line-number", "--no-heading", "--color", "never",
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
		return "", false, fmt.Errorf("rg invocation failed: %w", err)
	}
	// rg exit code 0 == matches found; 1 == no matches (clean); 2+ ==
	// real error (bad regex, IO failure, etc.). We treat 2+ as a soft
	// failure and let the caller fall back to the native walker.
	switch result.ExitCode {
	case 0:
		out := strings.TrimSpace(result.Stdout)
		// Cap output at max_results lines defensively — rg's --max-count
		// is per-file, so a workspace with many files could exceed the
		// global bound the caller asked for.
		lines := strings.Split(out, "\n")
		if len(lines) > maxResults {
			lines = lines[:maxResults]
		}
		if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
			return "No matches found.", true, nil
		}
		return strings.Join(lines, "\n"), true, nil
	case 1:
		return "No matches found.", true, nil
	default:
		return "", false, nil
	}
}

// grepNative walks `dir` and matches `re` against each file's contents. It
// is intentionally simple — no concurrency, no mmap — because correctness
// and predictable behaviour in containers without rg matters more than
// throughput. Binary files are skipped (heuristic: NUL byte in the first
// chunk) to avoid spamming gibberish into the model's context.
func grepNative(dir string, re *regexp.Regexp, include, exclude []string, maxResults int) (string, error) {
	var results []string
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
				results = append(results, fmt.Sprintf("%s:%d:%s", rel, lineNum+1, line))
				if len(results) >= maxResults {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return "", fmt.Errorf("search failed: %w", walkErr)
	}
	if len(results) == 0 {
		return "No matches found.", nil
	}
	return strings.Join(results, "\n"), nil
}

// findNative walks `dir` and collects files whose names match the glob. We
// match against the basename only (consistent with `find -name`), then apply
// the include/exclude glob filters against the relative path.
func findNative(dir, name string, include, exclude []string, maxResults int) (string, error) {
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
		return "", fmt.Errorf("find failed: %w", walkErr)
	}
	if len(results) == 0 {
		return "No matches found.", nil
	}
	return strings.Join(results, "\n"), nil
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
