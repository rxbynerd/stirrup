package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	compatibilityDoc = "../docs/compatibility.md"
	compatRepoRoot   = ".."
	compatRulesStart = "<!-- compat-rules:start -->"
	compatRulesEnd   = "<!-- compat-rules:end -->"
)

var (
	compatFootnoteRef = regexp.MustCompile(`\[\^([A-Za-z0-9_-]+)\]`)
	compatFootnoteDef = regexp.MustCompile(`(?m)^\[\^([A-Za-z0-9_-]+)\]:`)
)

// compatRule is one line of the rule list in docs/compatibility.md.
type compatRule struct {
	id          string
	path        string
	declaration string
	substring   string
	test        string
}

func readCompatibilityDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(compatibilityDoc))
	require.New(t).NoError(err)
	return string(raw)
}

func parseCompatRules(t *testing.T, doc string) []compatRule {
	t.Helper()
	must := require.New(t)
	start := strings.Index(doc, compatRulesStart)
	end := strings.Index(doc, compatRulesEnd)
	must.True(start >= 0 && end > start, "rule list markers missing from %s", compatibilityDoc)

	var rules []compatRule
	for _, line := range strings.Split(doc[start+len(compatRulesStart):end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "id |") {
			continue
		}
		fields := strings.Split(line, "|")
		must.Len(fields, 5, "rule list line needs 5 fields: %q", line)
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		rules = append(rules, compatRule{fields[0], fields[1], fields[2], fields[3], fields[4]})
	}
	must.NotEmpty(rules)
	return rules
}

// declarationName returns the lookup key of a top-level declaration:
// "Func" for a function, "Recv.Method" for a method, and the name itself
// for a type, variable or constant.
func declarationName(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return []string{d.Name.Name}
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if ident, ok := recv.(*ast.Ident); ok {
			return []string{ident.Name + "." + d.Name.Name}
		}
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			}
		}
		return names
	}
	return nil
}

// stringLiterals concatenates the unquoted string literals under node, so a
// message built by "a" + "b" matches a substring that spans the join.
func stringLiterals(node ast.Node) string {
	var b strings.Builder
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			b.WriteString(s)
		}
		return true
	})
	return b.String()
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.New(t).NoError(err, "parse %s", path)
	return file
}

func declaresTest(t *testing.T, dir, name string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	require.New(t).NoError(err)
	for _, path := range matches {
		for _, decl := range parseGoFile(t, path).Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// TestCompatibilityDoc_CitedRulesExist fails when a rule that
// docs/compatibility.md cites no longer exists: the declaration, the
// message substring inside it, or the test that pins it.
func TestCompatibilityDoc_CitedRulesExist(t *testing.T) {
	rules := parseCompatRules(t, readCompatibilityDoc(t))

	for _, rule := range rules {
		t.Run(rule.id, func(t *testing.T) {
			must := require.New(t)
			path := filepath.Join(compatRepoRoot, filepath.FromSlash(rule.path))
			file := parseGoFile(t, path)

			var found ast.Decl
			for _, decl := range file.Decls {
				if slices.Contains(declarationName(decl), rule.declaration) {
					found = decl
					break
				}
			}
			must.NotNil(found, "%s does not declare %s", rule.path, rule.declaration)

			if rule.substring != "" {
				must.Contains(stringLiterals(found), rule.substring,
					"%s in %s no longer produces the cited message", rule.declaration, rule.path)
			}
			if rule.test != "" {
				must.True(declaresTest(t, filepath.Dir(path), rule.test),
					"no test %s beside %s", rule.test, rule.path)
			}
		})
	}
}

// TestCompatibilityDoc_FootnotesMatchRuleList fails when a footnote
// reference, a footnote definition and a rule list entry do not pair up,
// so a row cannot cite a rule the list does not check.
func TestCompatibilityDoc_FootnotesMatchRuleList(t *testing.T) {
	must := require.New(t)
	doc := readCompatibilityDoc(t)

	var listed []string
	seen := map[string]bool{}
	for _, rule := range parseCompatRules(t, doc) {
		must.False(seen[rule.id], "duplicate rule id %s", rule.id)
		seen[rule.id] = true
		listed = append(listed, rule.id)
	}

	var defined []string
	for _, m := range compatFootnoteDef.FindAllStringSubmatch(doc, -1) {
		defined = append(defined, m[1])
	}

	body := compatFootnoteDef.ReplaceAllString(doc, "")
	referenced := map[string]bool{}
	for _, m := range compatFootnoteRef.FindAllStringSubmatch(body, -1) {
		referenced[m[1]] = true
	}
	var referencedIDs []string
	for id := range referenced {
		referencedIDs = append(referencedIDs, id)
	}

	slices.Sort(listed)
	slices.Sort(defined)
	slices.Sort(referencedIDs)
	must.Equal(listed, defined, "footnote definitions differ from the rule list")
	must.Equal(listed, referencedIDs, "footnote references differ from the rule list")
}
