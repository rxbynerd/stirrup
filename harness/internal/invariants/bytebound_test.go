package invariants

import (
	"go/ast"
	"go/token"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// unboundedEmitters are HarnessEvent sinks that do not apply the control
// stream's field caps, each with the reason.
var unboundedEmitters = map[string]string{
	"harness/internal/transport/null.go:NullTransport.Emit":   "discards every event",
	"harness/internal/core/subagent.go:captureTransport.Emit": "keeps a sub-agent's text in memory; it reaches the model as a bounded tool result",
	"harness/internal/transport/stdio.go:StdioTransport.Emit": "stdout has no message-size limit; only tool_result content is bounded upstream, other fields reach stdout unbounded",
}

// selfBoundedTools are the tools allowed to skip the loop's
// tools.maxResultBytes cut because their handler applies it.
var selfBoundedTools = map[string]string{
	"harness/internal/tool/builtins/shell.go:RunCommandToolWithStore": "keeps the head and tail of each stream within maxResultBytes",
	"harness/internal/tool/builtins/shell.go:ReadCommandOutputTool":   "reads a chunk sized to maxResultBytes",
	"harness/internal/tool/builtins/toolsearch.go:ToolSearchTool":     "returns at most maxResults short entries",
}

// toolResultBlockBuilders may build a tool_result content block from an
// already-bounded ToolResult.
var toolResultBlockBuilders = map[string]string{
	"harness/internal/core/types.go:appendToolResults": "copies ToolResults built after boundToolResult",
}

// TestHarnessEvents_PassTheByteBound pins that every HarnessEvent sink
// applies boundEventFields before the event leaves the process, and that
// the gRPC translation and send happen only after it, so a large tool
// payload cannot exceed the control plane's message limit and abort the
// stream.
func TestHarnessEvents_PassTheByteBound(t *testing.T) {
	check := assert.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")
	exempt := newAllowList(unboundedEmitters)

	bounded := 0
	for i := range files {
		sf := &files[i]
		for _, decl := range sf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Name.Name != "Emit" || !takesHarnessEvent(fd) {
				continue
			}
			site := funcDecl{file: sf, decl: fd}.site()
			if callsIn(fd.Body, "boundEventFields") {
				bounded++
				continue
			}
			if exempt.allows(site) {
				continue
			}
			check.Failf("HarnessEvent sink skips the byte bound", "%s: %s emits without boundEventFields", position(fset, fd.Pos()), site)
		}
	}
	check.Positive(bounded, "no Emit applies boundEventFields; the check would be vacuous")
	exempt.assertAllUsed(t)

	conversions := 0
	eachCall(files, func(sf *sourceFile, encl funcDecl, call *ast.CallExpr) {
		name := calleeName(call)
		if name != "harnessEventToProto" && (name != "Send" || !strings.HasPrefix(sf.path, "harness/internal/transport/")) {
			return
		}
		conversions++
		where := position(fset, call.Pos())
		if !check.NotNil(encl.decl, "%s: %s outside a function", where, name) {
			return
		}
		check.True(boundBefore(encl.decl, call.Pos()), "%s: %s runs before boundEventFields in %s", where, name, encl.site())
	})
	check.Positive(conversions, "no harnessEventToProto or Send call found; the check would be vacuous")
}

// TestHarnessEvents_EveryInputCarryingTypeIsBounded pins that every
// HarnessEvent type the harness builds with an Input field has a case in
// boundEventFields, which caps the input or refuses an oversized one, so
// a new request or tool event cannot carry an unbounded input to the
// control plane.
func TestHarnessEvents_EveryInputCarryingTypeIsBounded(t *testing.T) {
	check := assert.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")

	handled := boundEventFieldsInputCases(t, files)
	check.NotEmpty(handled, "boundEventFields has no case bounding Input; the check would be vacuous")
	consts := packageStringConsts(files)

	producers := 0
	for i := range files {
		sf := &files[i]
		typesName := importName(sf.file, "github.com/rxbynerd/stirrup/types")
		ast.Inspect(sf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isPkgSelector(lit.Type, typesName, "HarnessEvent") || fieldValue(lit, "Input") == nil {
				return true
			}
			producers++
			where := position(fset, lit.Pos())
			typ, resolved := stringValue(fieldValue(lit, "Type"), consts[path.Dir(sf.path)])
			if !check.True(resolved, "%s: HarnessEvent with Input has a Type the check cannot resolve to a string constant", where) {
				return true
			}
			check.True(handled[typ], "%s: %q events carry Input but no boundEventFields case bounds it", where, typ)
			return true
		})
	}
	check.Positive(producers, "no HarnessEvent literal sets Input; the check would be vacuous")
}

// boundEventFieldsInputCases returns the event types of the case clauses
// in boundEventFields whose body bounds Input: it reads or assigns a
// .Input field, or calls a function whose name mentions Input.
func boundEventFieldsInputCases(t *testing.T, files []sourceFile) map[string]bool {
	t.Helper()
	fd := findFunc(t, files, "harness/internal/transport/eventbound.go", "boundEventFields")
	cases := map[string]bool{}
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok || !boundsInput(clause.Body) {
			return true
		}
		for _, expr := range clause.List {
			if s, ok := stringValue(expr, nil); ok {
				cases[s] = true
			}
		}
		return true
	})
	return cases
}

func boundsInput(body []ast.Stmt) bool {
	found := false
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				found = found || e.Sel.Name == "Input"
			case *ast.CallExpr:
				found = found || strings.Contains(calleeName(e), "Input")
			}
			return !found
		})
	}
	return found
}

// packageStringConsts maps each package directory to its string constants.
func packageStringConsts(files []sourceFile) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, sf := range files {
		dir := path.Dir(sf.path)
		for _, decl := range sf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if s, ok := stringValue(vs.Values[i], nil); ok {
						if out[dir] == nil {
							out[dir] = map[string]string{}
						}
						out[dir][name.Name] = s
					}
				}
			}
		}
	}
	return out
}

// stringValue resolves expr to a string: a string literal, or an
// identifier naming one of consts.
func stringValue(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[e.Name]
		return s, ok
	}
	return "", false
}

// takesHarnessEvent reports whether fd's only parameter is a
// types.HarnessEvent.
func takesHarnessEvent(fd *ast.FuncDecl) bool {
	params := fd.Type.Params.List
	if len(params) != 1 {
		return false
	}
	sel, ok := params[0].Type.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "HarnessEvent"
}

// boundBefore reports whether fd calls boundEventFields before pos.
func boundBefore(fd *ast.FuncDecl, pos token.Pos) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "boundEventFields" && call.Pos() < pos {
			found = true
		}
		return !found
	})
	return found
}

// TestToolResults_PassTheResultBudget pins that every tool result reaching
// the model's history is cut to tools.maxResultBytes: ToolResults are
// built only where boundToolResult runs, tool_result blocks only from
// those ToolResults, and a tool may skip the cut only by bounding itself.
func TestToolResults_PassTheResultBudget(t *testing.T) {
	check := assert.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")
	selfBounded := newAllowList(selfBoundedTools)
	blockBuilders := newAllowList(toolResultBlockBuilders)

	results := 0
	for i := range files {
		sf := &files[i]
		typesName := importName(sf.file, "github.com/rxbynerd/stirrup/types")
		// implied holds the element type of literals written without one
		// inside a slice, array or map literal.
		implied := map[*ast.CompositeLit]ast.Expr{}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			litType := lit.Type
			if litType == nil {
				litType = implied[lit]
			}
			if elt := elementType(litType); elt != nil {
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						e = kv.Value
					}
					if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
						e = u.X
					}
					if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
						implied[inner] = elt
					}
				}
			}
			encl, inFunc := enclosingFunc(sf, lit.Pos())
			where := position(fset, lit.Pos())
			switch {
			case isPkgSelector(litType, typesName, "ToolResult"):
				results++
				check.True(inFunc && callsIn(encl.decl.Body, "boundToolResult"),
					"%s: a ToolResult is built where boundToolResult does not run", where)
			case isPkgSelector(litType, typesName, "ContentBlock") && fieldIsString(lit, "Type", "tool_result"):
				check.True(inFunc && blockBuilders.allows(encl.site()),
					"%s: a tool_result block is built outside the bounded ToolResult path", where)
			case isPkgSelector(litType, importName(sf.file, "github.com/rxbynerd/stirrup/harness/internal/tool"), "Tool") && fieldIsTrue(lit, "SelfBoundedResult"):
				check.True(inFunc && selfBounded.allows(encl.site()),
					"%s: a tool skips the result budget without being listed in selfBoundedTools", where)
			}
			return true
		})
	}
	check.Positive(results, "no ToolResult construction found; the check would be vacuous")
	selfBounded.assertAllUsed(t)
	blockBuilders.assertAllUsed(t)
}

func fieldValue(lit *ast.CompositeLit, field string) ast.Expr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
			return kv.Value
		}
	}
	return nil
}

func fieldIsString(lit *ast.CompositeLit, field, want string) bool {
	v, ok := fieldValue(lit, field).(*ast.BasicLit)
	if !ok || v.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(v.Value)
	return err == nil && s == want
}

func fieldIsTrue(lit *ast.CompositeLit, field string) bool {
	v, ok := fieldValue(lit, field).(*ast.Ident)
	return ok && v.Name == "true"
}

// elementType returns the element type of a slice, array or map type,
// pointer stripped, or nil for any other type.
func elementType(expr ast.Expr) ast.Expr {
	var elt ast.Expr
	switch t := expr.(type) {
	case *ast.ArrayType:
		elt = t.Elt
	case *ast.MapType:
		elt = t.Value
	default:
		return nil
	}
	if star, ok := elt.(*ast.StarExpr); ok {
		return star.X
	}
	return elt
}
