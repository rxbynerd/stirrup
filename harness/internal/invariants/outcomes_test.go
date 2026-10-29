package invariants

import (
	"go/ast"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestRunOutcomes_EveryOutcomeTheLoopSetsIsClassified pins that every
// string the agentic loop can set as a run outcome is listed in
// types.RunOutcomes, the single table EvalOutcomeFor and RunHealthFor read.
// An unlisted outcome would be scored by the unknown-outcome defaults.
//
// Outcome literals are found by following values from where they meet an
// outcome: assignments to and comparisons with variables named like
// "outcome", parameters and named results named like it, and from there
// through local variables, call arguments and return values within the
// core package until nothing new is reached.
func TestRunOutcomes_EveryOutcomeTheLoopSetsIsClassified(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	fset := token.NewFileSet()
	files := parseProductionFiles(t, fset, "harness/internal/core")
	found := newOutcomeScan(files).literals()

	known := types.RunOutcomes()
	names := make([]string, 0, len(found))
	for lit := range found {
		names = append(names, lit)
	}
	sort.Strings(names)
	t.Logf("outcomes reached: %s", strings.Join(names, ", "))
	must.Contains(found, "success", "the scan no longer reaches the success outcome; it would miss new outcomes")
	for _, lit := range names {
		check.Contains(known, lit, "%s sets outcome %q, which types.RunOutcomes does not list; add it to the table in types/evaloutcome.go", position(fset, found[lit]), lit)
	}
	for _, outcome := range known {
		if _, reached := found[outcome]; reached || providerStopReasonOutcomes[outcome] {
			continue
		}
		check.Failf("classified outcome is never set", "types.RunOutcomes lists %q, which the loop never sets; remove it or list it in providerStopReasonOutcomes", outcome)
	}
}

// providerStopReasonOutcomes are outcomes types.RunOutcomes classifies that
// reach RunTrace.Outcome as a provider stop reason rather than a literal in
// the loop.
var providerStopReasonOutcomes = map[string]bool{
	"max_tokens": true,
}

func isOutcomeName(name string) bool {
	return strings.Contains(strings.ToLower(name), "outcome")
}

// outcomeScan propagates "this value is a run outcome" through the
// functions of one package.
type outcomeScan struct {
	funcs   map[string][]*ast.FuncDecl
	params  map[string]map[int]bool
	results map[string]map[int]bool
	found   map[string]token.Pos
	changed bool
}

func newOutcomeScan(files []sourceFile) *outcomeScan {
	s := &outcomeScan{
		funcs:   map[string][]*ast.FuncDecl{},
		params:  map[string]map[int]bool{},
		results: map[string]map[int]bool{},
		found:   map[string]token.Pos{},
	}
	for _, sf := range files {
		for _, decl := range sf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			s.funcs[fd.Name.Name] = append(s.funcs[fd.Name.Name], fd)
			for i, name := range fieldNames(fd.Type.Params) {
				if isOutcomeName(name) {
					s.mark(s.params, fd.Name.Name, i)
				}
			}
			for i, name := range fieldNames(fd.Type.Results) {
				if isOutcomeName(name) {
					s.mark(s.results, fd.Name.Name, i)
				}
			}
		}
	}
	return s
}

func fieldNames(fields *ast.FieldList) []string {
	if fields == nil {
		return nil
	}
	var names []string
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			names = append(names, "")
			continue
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

func (s *outcomeScan) mark(m map[string]map[int]bool, fn string, i int) {
	if m[fn] == nil {
		m[fn] = map[int]bool{}
	}
	if !m[fn][i] {
		m[fn][i] = true
		s.changed = true
	}
}

// literals runs the propagation to a fixed point and returns every string
// literal reached, with one position where it appears.
func (s *outcomeScan) literals() map[string]token.Pos {
	for first := true; first || s.changed; first = false {
		s.changed = false
		for _, decls := range s.funcs {
			for _, fd := range decls {
				s.scanFunc(fd)
			}
		}
	}
	return s.found
}

func (s *outcomeScan) scanFunc(fd *ast.FuncDecl) {
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			s.scanAssign(fd, node)
		case *ast.BinaryExpr:
			if node.Op == token.EQL || node.Op == token.NEQ {
				if id, ok := node.X.(*ast.Ident); ok && isOutcomeName(id.Name) {
					s.value(fd, node.Y)
				}
			}
		case *ast.SwitchStmt:
			if id, ok := node.Tag.(*ast.Ident); ok && isOutcomeName(id.Name) {
				for _, stmt := range node.Body.List {
					for _, e := range stmt.(*ast.CaseClause).List {
						s.value(fd, e)
					}
				}
			}
		case *ast.CallExpr:
			name := calleeName(node)
			for i, arg := range node.Args {
				if s.params[name][i] {
					s.value(fd, arg)
				}
			}
		}
		return true
	})
	if len(s.results[fd.Name.Name]) == 0 {
		return
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok {
			for i, e := range ret.Results {
				if s.results[fd.Name.Name][i] {
					s.value(fd, e)
				}
			}
		}
		return true
	})
}

// scanAssign treats a value assigned to an outcome-named variable as an
// outcome, and marks the result of a call whose value lands in one.
func (s *outcomeScan) scanAssign(fd *ast.FuncDecl, as *ast.AssignStmt) {
	for i, lhs := range as.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok || !isOutcomeName(id.Name) {
			continue
		}
		switch {
		case len(as.Rhs) == len(as.Lhs):
			s.value(fd, as.Rhs[i])
		case len(as.Rhs) == 1:
			if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
				s.mark(s.results, calleeName(call), i)
			}
		}
	}
}

// value records expr as an outcome: a literal is collected, a call marks
// its callee's first result, a parameter marks that parameter for every
// caller, and a local variable carries the outcome to what is assigned
// to it.
func (s *outcomeScan) value(fd *ast.FuncDecl, expr ast.Expr) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return
		}
		lit, err := strconv.Unquote(e.Value)
		if err != nil || lit == "" {
			return
		}
		if _, seen := s.found[lit]; !seen {
			s.found[lit] = e.Pos()
		}
	case *ast.ParenExpr:
		s.value(fd, e.X)
	case *ast.CallExpr:
		s.mark(s.results, calleeName(e), 0)
	case *ast.Ident:
		if i := slices.Index(fieldNames(fd.Type.Params), e.Name); i >= 0 {
			s.mark(s.params, fd.Name.Name, i)
			return
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != e.Name {
					continue
				}
				switch {
				case len(as.Rhs) == len(as.Lhs):
					if rhs, ok := as.Rhs[i].(*ast.Ident); !ok || rhs.Name != e.Name {
						s.value(fd, as.Rhs[i])
					}
				case len(as.Rhs) == 1:
					if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
						s.mark(s.results, calleeName(call), i)
					}
				}
			}
			return true
		})
	}
}
