package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spanStatusExemptions are SetStatus(codes.Error, ...) sites whose
// description is not traceably scrubbed but carries no secret.
var spanStatusExemptions = map[string]string{
	"harness/internal/trace/otel_live.go:otelAgentSpan.End":  "description is the run outcome, a bounded enum",
	"harness/internal/trace/otel.go:OTelTraceEmitter.Finish": "description defaults to the run outcome, a bounded enum, and is otherwise scrubbed",
}

// slogExemptions are handler constructions that do not wrap a
// ScrubHandler at the call itself, with where the scrubbing happens.
var slogExemptions = map[string]string{
	"harness/internal/observability/logger.go:NewScopedLoggerWithExport": "wraps its JSON leaf and the export leaf in NewScrubHandlerWithSecurity before slog.New",
	"harness/internal/observability/logexport.go:NewLogExporter":         "returns the OTLP bridge as a leaf that NewScopedLoggerWithExport fans out to beneath its ScrubHandler",
}

// TestSpanErrors_AreScrubbed pins that no production code calls
// RecordError on a span (the semconv exception event goes through
// core.recordSpanException, which scrubs it) and that every SetStatus
// description other than for codes.Ok or codes.Unset is a literal or
// traceably scrubbed text: built only from literals and calls to scrub*
// functions or core's span-error helpers, through locals of the same
// function. The log scrubber does not see spans. Span attributes and
// events other than the exception event are not covered.
func TestSpanErrors_AreScrubbed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")
	exempt := newAllowList(spanStatusExemptions)
	scanned := map[string]bool{}

	eachCall(files, func(sf *sourceFile, encl funcDecl, call *ast.CallExpr) {
		scanned[sf.path] = true
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		switch sel.Sel.Name {
		case "RecordError":
			check.Failf("span error bypasses the scrubbing helpers",
				"%s: RecordError records unscrubbed text; use core.recordSpanError or recordSpanException", position(fset, call.Pos()))
		case "SetStatus":
			codesName := importName(sf.file, "go.opentelemetry.io/otel/codes")
			if len(call.Args) != 2 || isPkgSelector(call.Args[0], codesName, "Ok") || isPkgSelector(call.Args[0], codesName, "Unset") {
				return
			}
			if encl.decl != nil && scrubbedExpr(call.Args[1], encl.decl, map[string]bool{}) {
				return
			}
			if encl.decl != nil && exempt.allows(encl.site()) {
				return
			}
			check.Failf("span status description is not scrubbed",
				"%s: SetStatus(codes.Error, ...) needs a literal or scrubbed description", position(fset, call.Pos()))
		}
	})
	for _, path := range []string{"harness/internal/core/loop.go", "harness/internal/core/spanerror.go", "harness/internal/trace/otel.go"} {
		must.True(scanned[path], "scan did not cover %s", path)
	}
	exempt.assertAllUsed(t)
}

// scrubbedExpr reports whether expr is a string literal, the result of a
// scrubbing call, or built only from such values within fn. An identifier
// counts when every assignment to it in fn does; a parameter never does.
func scrubbedExpr(expr ast.Expr, fn *ast.FuncDecl, visiting map[string]bool) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.ParenExpr:
		return scrubbedExpr(e.X, fn, visiting)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && scrubbedExpr(e.X, fn, visiting) && scrubbedExpr(e.Y, fn, visiting)
	case *ast.SelectorExpr:
		return scrubbedExpr(e.X, fn, visiting)
	case *ast.CallExpr:
		name := calleeName(e)
		if isScrubbingFunc(name) {
			return true
		}
		if name != "Sprintf" || len(e.Args) == 0 {
			return false
		}
		for _, arg := range e.Args {
			if !scrubbedExpr(arg, fn, visiting) {
				return false
			}
		}
		return true
	case *ast.Ident:
		if visiting[e.Name] {
			return true
		}
		if isParam(fn, e.Name) {
			return false
		}
		visiting[e.Name] = true
		defer delete(visiting, e.Name)
		values := assignedValues(fn, e.Name)
		if len(values) == 0 {
			return false
		}
		for _, v := range values {
			if !scrubbedExpr(v, fn, visiting) {
				return false
			}
		}
		return true
	}
	return false
}

// isScrubbingFunc reports whether a call to name returns scrubbed text:
// security.Scrub and its variants, the trace package's scrub helpers, and
// core's span-error helpers, which return the text they recorded.
func isScrubbingFunc(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "scrub") ||
		name == "recordSpanError" || name == "recordSpanException"
}

// isParam reports whether name is a parameter or receiver of fn. A
// parameter carries caller-supplied text, so reassigning it to scrubbed
// text on some paths does not make it scrubbed.
func isParam(fn *ast.FuncDecl, name string) bool {
	lists := []*ast.FieldList{fn.Type.Params, fn.Recv}
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, n := range field.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// assignedValues returns every expression assigned to the local name in
// fn. A multi-value assignment from one call yields that call.
func assignedValues(fn *ast.FuncDecl, name string) []ast.Expr {
	var values []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					values = append(values, s.Rhs[i])
				} else if len(s.Rhs) == 1 {
					values = append(values, s.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if id.Name == name && i < len(s.Values) {
					values = append(values, s.Values[i])
				}
			}
		case *ast.RangeStmt:
			if id, ok := s.Value.(*ast.Ident); ok && id.Name == name {
				values = append(values, s.X)
			}
		}
		return true
	})
	return values
}

// TestLogHandlers_AreScrubbed pins that every slog handler the harness
// builds sits beneath a ScrubHandler: a JSON, text or OTLP-bridge leaf is
// constructed as the argument of NewScrubHandler, every slog.New wraps a
// ScrubHandler or discards, and the process default is set only to a
// scrubbed logger. Exemptions name where the scrubbing happens instead.
func TestLogHandlers_AreScrubbed(t *testing.T) {
	check := assert.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")
	exempt := newAllowList(slogExemptions)
	scrubWrapped := map[*ast.CallExpr]bool{}

	eachCall(files, func(_ *sourceFile, _ funcDecl, call *ast.CallExpr) {
		if strings.HasPrefix(calleeName(call), "NewScrubHandler") && len(call.Args) > 0 {
			if inner, ok := call.Args[0].(*ast.CallExpr); ok {
				scrubWrapped[inner] = true
			}
		}
	})

	eachCall(files, func(sf *sourceFile, encl funcDecl, call *ast.CallExpr) {
		slogName := importName(sf.file, "log/slog")
		bridgeName := importName(sf.file, "go.opentelemetry.io/contrib/bridges/otelslog")
		violation := ""
		switch {
		case isPkgSelector(call.Fun, slogName, "NewJSONHandler"),
			isPkgSelector(call.Fun, slogName, "NewTextHandler"),
			isPkgSelector(call.Fun, bridgeName, "NewHandler"):
			if !scrubWrapped[call] {
				violation = "log handler is not built inside NewScrubHandler"
			}
		case isPkgSelector(call.Fun, slogName, "New"):
			if len(call.Args) == 1 && !scrubbedHandler(call.Args[0], slogName) {
				violation = "slog.New wraps a handler that is not a ScrubHandler"
			}
		case isPkgSelector(call.Fun, slogName, "SetDefault"):
			if len(call.Args) != 1 || !returnsScrubbedLogger(sf.file, call.Args[0], slogName) {
				violation = "slog.SetDefault installs a logger the scan cannot trace to a ScrubHandler"
			}
		}
		if violation == "" || (encl.decl != nil && exempt.allows(encl.site())) {
			return
		}
		check.Failf("log path bypasses the scrubber", "%s: %s", position(fset, call.Pos()), violation)
	})
	exempt.assertAllUsed(t)
}

// returnsScrubbedLogger reports whether expr calls a function declared in
// f whose every return is slog.New over a ScrubHandler.
func returnsScrubbedLogger(f *ast.File, expr ast.Expr, slogName string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != id.Name || fd.Body == nil {
			continue
		}
		returns, scrubbed := 0, 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			returns++
			if len(ret.Results) == 1 {
				if inner, ok := ret.Results[0].(*ast.CallExpr); ok && isPkgSelector(inner.Fun, slogName, "New") &&
					len(inner.Args) == 1 && scrubbedHandler(inner.Args[0], slogName) {
					scrubbed++
				}
			}
			return true
		})
		return returns > 0 && returns == scrubbed
	}
	return false
}

func scrubbedHandler(expr ast.Expr, slogName string) bool {
	if isPkgSelector(expr, slogName, "DiscardHandler") {
		return true
	}
	call, ok := expr.(*ast.CallExpr)
	return ok && strings.HasPrefix(calleeName(call), "NewScrubHandler")
}

// TestScrubbedExpr_AcceptsOnlyTraceablyScrubbedText pins the description
// heuristic of TestSpanErrors_AreScrubbed on small functions.
func TestScrubbedExpr_AcceptsOnlyTraceablyScrubbedText(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"literal", `desc := "failed"`, true},
		{"scrub call", `desc := security.Scrub(err.Error())`, true},
		{"literal then scrub", `desc := "failed"; desc = scrubOrPass(msg, false)`, true},
		{"sprintf of scrubbed", `s := scrubHook(x); desc := fmt.Sprintf("exit %d: %s", s.Code, s.Error)`, true},
		{"raw error", `desc := err.Error()`, false},
		{"parameter", `desc := msg`, false},
		{"parameter scrubbed on one path", `if cond { msg = security.Scrub(msg) }; desc := msg`, false},
		{"unscrubbed helper", `desc := unscrubbed(msg)`, false},
		{"concatenation with raw text", `desc := "failed: " + err.Error()`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			src := "package p\nfunc f(msg string, err error, cond bool, x any) { " + tc.body + "; use(desc) }\n"
			f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, parser.SkipObjectResolution)
			must.NoError(err)
			fn, ok := f.Decls[0].(*ast.FuncDecl)
			must.True(ok)
			must.Equal(tc.want, scrubbedExpr(ast.NewIdent("desc"), fn, map[string]bool{}))
		})
	}
}
