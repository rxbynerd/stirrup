package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

var traceGrepCmd = &cobra.Command{
	Use:   "grep [pattern] <file>",
	Short: "Filter JSONL trace records by substring or JSON-path predicate",
	Long: `Filter JSONL trace records, printing only the matching lines.

Match modes:
  pattern   Substring match against the raw JSON line. Empty pattern
            matches every line (so --jq alone is enough on its own).
  --jq      A small JSON-path predicate of the form '<path> <op> <value>'.
            Supported ops: == != contains. The value may be a quoted
            string or a bare number. Examples:
              --jq '.id == "run-42"'
              --jq '.turns != 0'
              --jq '.outcome contains "fail"'
              --jq '.toolCalls.0.name == "edit_file"'
            This is deliberately a thin predicate, not a full jq
            interpreter — the goal is to drop the operational
            dependency on having jq installed.

Pass ` + "`-`" + ` as the file argument to read from stdin. Matching
records are emitted verbatim, one JSON document per line.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runTraceGrep,
}

func init() {
	traceCmd.AddCommand(traceGrepCmd)
	f := traceGrepCmd.Flags()
	f.String("jq", "", "JSON-path predicate (e.g. '.outcome == \"success\"'). Combine with the pattern arg for AND semantics.")
	f.Bool("invert-match", false, "Invert the match — print records that do NOT satisfy the predicate.")
}

func runTraceGrep(cmd *cobra.Command, args []string) error {
	var pattern, path string
	switch len(args) {
	case 1:
		path = args[0]
	case 2:
		pattern = args[0]
		path = args[1]
	}
	jq, _ := cmd.Flags().GetString("jq")
	invert, _ := cmd.Flags().GetBool("invert-match")

	pred, err := compileJQ(jq)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runTraceGrepWith(ctx, path, cmd.OutOrStdout(), pattern, pred, invert)
}

func runTraceGrepWith(ctx context.Context, path string, out io.Writer, pattern string, pred jqPredicate, invert bool) error {
	var src io.Reader
	if path == "-" {
		src = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening trace file %q: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		src = f
	}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		matched, err := lineMatches(line, pattern, pred)
		if err != nil {
			// A line that fails predicate evaluation is treated as a
			// non-match (e.g. it isn't valid JSON, or the path
			// doesn't exist). Predicate compile errors are surfaced
			// up-front; runtime evaluation errors must not abort
			// `grep` mid-stream.
			matched = false
		}
		if invert {
			matched = !matched
		}
		if !matched {
			continue
		}
		if _, err := out.Write(append([]byte(nil), line...)); err != nil {
			return err
		}
		if _, err := io.WriteString(out, "\n"); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("reading trace file: %w", err)
	}
	return nil
}

func lineMatches(line []byte, pattern string, pred jqPredicate) (bool, error) {
	if pattern != "" && !strings.Contains(string(line), pattern) {
		return false, nil
	}
	if pred.empty() {
		return true, nil
	}
	var doc any
	if err := json.Unmarshal(line, &doc); err != nil {
		return false, err
	}
	return pred.eval(doc)
}

// jqPredicate is a deliberately tiny predicate evaluator over a JSON
// document. It supports three operators: ==, !=, and `contains`. The
// path is a dot-separated walk of object keys and numeric array
// indices (e.g. `.toolCalls.0.name`). This is enough to cover the
// acceptance-criteria examples in issue #244 without taking on a full
// jq vendor dependency.
type jqPredicate struct {
	path []pathSeg
	op   string
	want any // string, float64, or nil (for null)
	raw  bool
}

type pathSeg struct {
	key string
	idx int
	num bool
}

func (p jqPredicate) empty() bool { return !p.raw }

func compileJQ(expr string) (jqPredicate, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return jqPredicate{}, nil
	}

	// Tokenise as: <path> <op> <value>. The op is one of ==, !=, contains.
	// A value may be a "double-quoted string", a bare number, or the
	// literal null/true/false. The path begins with `.` and may
	// reference an empty path with just `.` (meaning the whole doc).
	rest := expr
	pathToken, rest, err := lexJQPath(rest)
	if err != nil {
		return jqPredicate{}, fmt.Errorf("--jq: %w", err)
	}
	rest = strings.TrimSpace(rest)

	var op string
	switch {
	case strings.HasPrefix(rest, "=="):
		op = "=="
		rest = rest[2:]
	case strings.HasPrefix(rest, "!="):
		op = "!="
		rest = rest[2:]
	case strings.HasPrefix(rest, "contains"):
		op = "contains"
		rest = rest[len("contains"):]
	default:
		return jqPredicate{}, fmt.Errorf("--jq: expected one of ==, !=, contains after path, got %q", rest)
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return jqPredicate{}, fmt.Errorf("--jq: missing value after operator %q", op)
	}

	want, err := parseJQValue(rest)
	if err != nil {
		return jqPredicate{}, fmt.Errorf("--jq: %w", err)
	}

	segs, err := compileJQPath(pathToken)
	if err != nil {
		return jqPredicate{}, fmt.Errorf("--jq: %w", err)
	}

	return jqPredicate{path: segs, op: op, want: want, raw: true}, nil
}

// lexJQPath returns the leading path token (e.g. `.foo.bar.0`) and the
// remainder of the expression. A path is a `.`-prefixed run of word
// characters, digits, and underscores separated by `.`.
func lexJQPath(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, ".") {
		return "", s, fmt.Errorf("path must start with '.'")
	}
	i := 1
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' {
			break
		}
		if c == '=' || c == '!' {
			break
		}
		// "contains" begins with 'c', which is a valid identifier
		// character. Stop only at the boundary signalled by
		// whitespace; the caller separates `contains` from the path
		// using whitespace, which the issue's example respects.
		i++
	}
	return s[:i], s[i:], nil
}

func compileJQPath(token string) ([]pathSeg, error) {
	token = strings.TrimPrefix(token, ".")
	if token == "" {
		return nil, nil
	}
	parts := strings.Split(token, ".")
	out := make([]pathSeg, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("empty segment in path")
		}
		seg := pathSeg{key: p}
		if n, ok := parseIndex(p); ok {
			seg.idx = n
			seg.num = true
		}
		out = append(out, seg)
	}
	return out, nil
}

func parseIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// parseJQValue parses a quoted string, a bare number, or a true/false/null
// literal. Returns string, float64, bool, or nil.
func parseJQValue(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty value")
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("invalid quoted string: %w", err)
		}
		return out, nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	var n float64
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		return nil, fmt.Errorf("expected quoted string or number, got %q", s)
	}
	return n, nil
}

func (p jqPredicate) eval(doc any) (bool, error) {
	got, ok := walkPath(doc, p.path)
	if !ok {
		// Path didn't resolve. != against any literal value is true;
		// == is false. contains is false.
		switch p.op {
		case "!=":
			return true, nil
		default:
			return false, nil
		}
	}
	switch p.op {
	case "==":
		return jsonEqual(got, p.want), nil
	case "!=":
		return !jsonEqual(got, p.want), nil
	case "contains":
		gs, gok := got.(string)
		ws, wok := p.want.(string)
		if !gok || !wok {
			return false, nil
		}
		return strings.Contains(gs, ws), nil
	default:
		return false, fmt.Errorf("unknown op %q", p.op)
	}
}

func walkPath(doc any, path []pathSeg) (any, bool) {
	cur := doc
	for _, seg := range path {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg.key]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			if !seg.num {
				return nil, false
			}
			if seg.idx < 0 || seg.idx >= len(v) {
				return nil, false
			}
			cur = v[seg.idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonEqual compares two JSON-decoded values for equality with the
// number-coercion semantics jq operators expect: int constants in the
// query end up as float64 after parseJQValue, and JSON numbers in the
// document are float64 too, so direct == works on those. Strings and
// bools compare verbatim; null is `nil`.
func jsonEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	default:
		return false
	}
}
