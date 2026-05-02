package codescanner

import (
	"bytes"
	"context"
	"regexp"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// patternRule is one rule in the pure-Go pattern pack. Severity is
// per-rule so individual rules can be downgraded or upgraded without
// changing the matcher.
type patternRule struct {
	id       string
	re       *regexp.Regexp
	severity string
	message  string
}

// secretRules returns the canonical hardcoded-secret rules, sourced from
// the LogScrubber pattern set so the two stay in sync. All secret hits
// default to "block": a hardcoded API key landing in a committed file is
// a hard fail.
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
// sinks rather than trying to detect every dynamic-evaluation idiom.
var sinkRules = []patternRule{
	{
		id:       "sink/python_os_system",
		re:       regexp.MustCompile(`\bos\.system\s*\(`),
		severity: SeverityWarn,
		message:  "os.system call: prefer subprocess with shell=False",
	},
	{
		id:       "sink/python_subprocess_shell_true",
		re:       regexp.MustCompile(`subprocess\.[A-Za-z_]+\s*\([^)]*shell\s*=\s*True`),
		severity: SeverityWarn,
		message:  "subprocess call with shell=True: argument injection risk",
	},
	{
		id:       "sink/python_eval",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_.])eval\s*\(`),
		severity: SeverityWarn,
		message:  "eval() use: dynamic code execution risk",
	},
	{
		id:       "sink/python_exec",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_.])exec\s*\(`),
		severity: SeverityWarn,
		message:  "exec() use: dynamic code execution risk",
	},
	{
		// Function constructor (JS/TS) commonly used to evaluate strings.
		// Match `new Function(` or `Function(` at a token boundary.
		id:       "sink/js_function_constructor",
		re:       regexp.MustCompile(`(^|[^A-Za-z0-9_$.])(new\s+)?Function\s*\(`),
		severity: SeverityWarn,
		message:  "Function() constructor: dynamic code execution risk",
	},
	{
		// Backtick command substitution in shell scripts. The pattern
		// matches a `...` pair on a single line containing a shell-like
		// command character. False positives on Markdown code spans are
		// accepted; the warn severity keeps this advisory.
		id:       "sink/shell_backtick",
		re:       regexp.MustCompile("`[^`\n]*[A-Za-z_/][^`\n]*`"),
		severity: SeverityWarn,
		message:  "backtick command substitution: prefer $() and quote inputs",
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

// Scan runs every rule against content and returns the union of matches.
// Findings are emitted in (rule-order, line-order, byte-offset-order) so
// the result is deterministic.
func (s *PatternScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	if len(content) == 0 {
		return &ScanResult{}, nil
	}
	var findings []Finding
	for _, r := range s.rules {
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
