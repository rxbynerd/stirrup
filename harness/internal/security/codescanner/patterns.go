package codescanner

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// patternRule is one rule in the pure-Go pattern pack. Severity is
// per-rule so individual rules can be downgraded or upgraded without
// changing the matcher. Scope restricts the rule to the file types its
// pattern was written for.
type patternRule struct {
	id       string
	re       *regexp.Regexp
	severity string
	message  string
	scope    ruleScope
}

// ruleScope decides which files a rule applies to. The zero value
// applies to every file, which is what secret rules use: a hardcoded
// credential is a finding whatever the file is called.
//
// A path whose extension is absent or unrecognised reaches a scoped
// rule only through basenames or interpreters, so an unknown file type
// is scanned by the unscoped rules alone.
type ruleScope struct {
	// exts are lower-cased extensions including the leading dot.
	exts []string
	// basenames are lower-cased file names, matched exactly or as the
	// stem of a variant name ("dockerfile" matches "Dockerfile.dev").
	basenames []string
	// interpreters are program names matched against the `#!` line of
	// a file that has no extension, tolerating a version suffix
	// ("python" matches "python3.12").
	interpreters []string
}

var (
	// shellScope covers files that are shell or embed shell fragments.
	// The YAML entries carry CI `run:` blocks.
	shellScope = ruleScope{
		exts:         []string{".sh", ".bash", ".zsh", ".ksh", ".mk", ".yml", ".yaml", ".dockerfile"},
		basenames:    []string{"dockerfile", "containerfile", "makefile", "gnumakefile"},
		interpreters: []string{"sh", "bash", "zsh", "ksh", "dash"},
	}
	pythonScope = ruleScope{
		exts:         []string{".py", ".pyi"},
		interpreters: []string{"python"},
	}
	jsScope = ruleScope{
		exts:         []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts"},
		interpreters: []string{"node"},
	}
)

// secretRules returns the canonical hardcoded-secret rules, sourced from
// the LogScrubber pattern set so the two stay in sync. All secret hits
// default to "block": a hardcoded API key landing in a committed file is
// a hard fail. They carry no scope, so every file type is checked.
func secretRules() []patternRule {
	patterns := security.SecretPatterns()
	rules := make([]patternRule, 0, len(patterns))
	for _, p := range patterns {
		rules = append(rules, patternRule{
			id:       "secret/" + p.Name,
			re:       p.Re,
			severity: SeverityBlock,
			message:  "hardcoded secret detected (pattern: " + p.Name + ")",
		})
	}
	return rules
}

// sinkRules covers eval/exec sinks the blueprint calls out explicitly.
// These default to "warn" because legitimate dynamic-evaluation use cases
// exist (test runners, REPLs, plugin loaders); operators wanting strict
// enforcement set BlockOnWarn = true on the config.
//
// The patterns are deliberately conservative — they match clearly-named
// sinks rather than trying to detect every dynamic-evaluation idiom —
// and each is scoped to the language it describes, because the same
// character sequence means something else in another language.
var sinkRules = []patternRule{
	{
		id:       "sink/python_os_system",
		re:       regexp.MustCompile(`\bos\.system\s*\(`),
		severity: SeverityWarn,
		message:  "os.system call: prefer subprocess with shell=False",
		scope:    pythonScope,
	},
	{
		id:       "sink/python_subprocess_shell_true",
		re:       regexp.MustCompile(`subprocess\.[A-Za-z_]+\s*\([^)]*shell\s*=\s*True`),
		severity: SeverityWarn,
		message:  "subprocess call with shell=True: argument injection risk",
		scope:    pythonScope,
	},
	{
		id:       "sink/python_eval",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_.])eval\s*\(`),
		severity: SeverityWarn,
		message:  "eval() use: dynamic code execution risk",
		scope:    pythonScope,
	},
	{
		id:       "sink/python_exec",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_.])exec\s*\(`),
		severity: SeverityWarn,
		message:  "exec() use: dynamic code execution risk",
		scope:    pythonScope,
	},
	{
		// Matches `new Function(` / `Function(`, commonly used to eval strings.
		id:       "sink/js_function_constructor",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_$.])(new\s+)?Function\s*\(`),
		severity: SeverityWarn,
		message:  "Function() constructor: dynamic code execution risk",
		scope:    jsScope,
	},
	{
		// Scoped to shell: the same pattern matches JavaScript template
		// literals and Markdown code spans.
		id:       "sink/shell_backtick",
		re:       regexp.MustCompile("`[^`\n]*[A-Za-z_/][^`\n]*`"),
		severity: SeverityWarn,
		message:  "backtick command substitution: prefer $() and quote inputs",
		scope:    shellScope,
	},
}

// PatternScanner is a pure-Go regex-based CodeScanner. It is always
// available — no external binaries — so it is the default for
// edit-capable run modes.
type PatternScanner struct {
	rules []patternRule
}

// NewPatternScanner returns a PatternScanner pre-loaded with the canonical
// secret + sink rule sets.
func NewPatternScanner() *PatternScanner {
	rules := append([]patternRule{}, secretRules()...)
	rules = append(rules, sinkRules...)
	return &PatternScanner{rules: rules}
}

// Scan runs every rule whose scope covers path against content and
// returns the union of matches. Findings are emitted in (rule-order,
// line-order, byte-offset-order) so the result is deterministic.
func (s *PatternScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	if len(content) == 0 {
		return &ScanResult{}, nil
	}
	file := identifyFile(path, content)
	var findings []Finding
	for _, r := range s.rules {
		if !r.scope.matches(file) {
			continue
		}
		matches := r.re.FindAllIndex(content, -1)
		for _, m := range matches {
			findings = append(findings, Finding{
				Severity: r.severity,
				Rule:     r.id,
				Line:     lineNumber(content, m[0]),
				Message:  r.message,
			})
		}
	}
	return &ScanResult{Findings: findings}, nil
}

// fileIdentity is the per-file information rule scopes are matched
// against. It is derived once per scan.
type fileIdentity struct {
	base   string
	ext    string
	interp string
}

// identifyFile derives the scope inputs for path. The shebang is read
// only when the path carries no extension, which is where it is the
// sole signal of file type.
func identifyFile(path string, content []byte) fileIdentity {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(base))
	if ext == base {
		// A leading dot names the file (".bashrc"); it is not an extension.
		ext = ""
	}
	f := fileIdentity{base: base, ext: ext}
	if ext == "" {
		f.interp = shebangInterpreter(content)
	}
	return f
}

// matches reports whether the scope covers f. An empty scope covers
// every file.
func (s ruleScope) matches(f fileIdentity) bool {
	if len(s.exts) == 0 && len(s.basenames) == 0 && len(s.interpreters) == 0 {
		return true
	}
	if f.ext != "" && slices.Contains(s.exts, f.ext) {
		return true
	}
	for _, n := range s.basenames {
		if f.base == n || strings.HasPrefix(f.base, n+".") {
			return true
		}
	}
	return f.interp != "" && matchesInterpreter(f.interp, s.interpreters)
}

// matchesInterpreter reports whether interp is one of names, allowing a
// numeric version suffix ("python3.12" matches "python").
func matchesInterpreter(interp string, names []string) bool {
	for _, n := range names {
		if rest, ok := strings.CutPrefix(interp, n); ok && strings.Trim(rest, "0123456789.") == "" {
			return true
		}
	}
	return false
}

// shebangInterpreter returns the lower-cased program name from a
// leading `#!` line, resolving `/usr/bin/env prog` to prog. It returns
// "" when content does not open with a shebang.
func shebangInterpreter(content []byte) string {
	if !bytes.HasPrefix(content, []byte("#!")) {
		return ""
	}
	line := content[2:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return ""
	}
	prog := strings.ToLower(filepath.Base(fields[0]))
	if prog != "env" {
		return prog
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
			continue
		}
		return strings.ToLower(filepath.Base(arg))
	}
	return ""
}

// lineNumber returns the 1-indexed line number of the byte offset off
// within content. Offsets past the end map to the last line.
func lineNumber(content []byte, off int) int {
	if off < 0 {
		off = 0
	}
	if off > len(content) {
		off = len(content)
	}
	return 1 + bytes.Count(content[:off], []byte("\n"))
}
