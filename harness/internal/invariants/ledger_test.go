package invariants

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamCallSites are the production functions allowed to call a
// provider adapter's Stream, each with how its call reaches the run-wide
// token ledger.
var streamCallSites = map[string]string{
	"harness/internal/core/ledger.go:chargingProvider.Stream":                 "the ledger charge itself",
	"harness/internal/core/meteredprovider.go:meteredProvider.Stream":         "metrics decorator the factory wraps in chargingProvider",
	"harness/internal/core/turnrequest.go:AgenticLoop.requestProvider":        "the agentic turn; charges the ledger it is passed (checked below)",
	"harness/internal/provider/batch.go:BatchAdapter.pumpInner":               "adapter decorator beneath the loop's or auxProv's charge",
	"harness/internal/provider/normalizer.go:NormalizingAdapter.Stream":       "adapter decorator beneath the loop's or auxProv's charge",
	"harness/internal/context/summarise.go:SummariseStrategy.generateSummary": "auxiliary call; the factory hands it auxProv (checked below)",
	"harness/internal/verifier/llmjudge.go:LLMJudgeVerifier.Verify":           "auxiliary call; the factory hands it auxProv (checked below)",
	"harness/internal/guard/cloudjudge.go:CloudJudge.check":                   "auxiliary call; the factory hands it auxProv (checked below)",
}

// auxiliaryConstructors maps each constructor of an auxiliary model
// caller to the factory builder that must be its only production caller.
var auxiliaryConstructors = map[string]string{
	"NewSummariseStrategy": "buildContextStrategy",
	"NewLLMJudgeVerifier":  "buildVerifier",
	"NewCloudJudge":        "buildGuardRailNode",
}

// TestProviderStreams_AreChargedToTheRunLedger pins that every provider
// call is charged to the run-wide token ledger, so MaxTokenBudget bounds
// sub-agents and auxiliary calls as well as the loop's own turns. A new
// Stream call site fails until it is routed through the ledger and listed.
func TestProviderStreams_AreChargedToTheRunLedger(t *testing.T) {
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness")

	t.Run("every Stream call site is a known ledger path", func(t *testing.T) {
		check := assert.New(t)
		sites := newAllowList(streamCallSites)
		eachCall(files, func(_ *sourceFile, encl funcDecl, call *ast.CallExpr) {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Stream" || len(call.Args) != 2 {
				return
			}
			if encl.decl != nil && sites.allows(encl.site()) {
				return
			}
			check.Failf("provider call bypasses the run ledger",
				"%s (%s): Stream is called outside the known ledger paths; charge it to the run's TokenLedger and list the site in streamCallSites", position(fset, call.Pos()), encl.site())
		})
		sites.assertAllUsed(t)
	})

	t.Run("the agentic turn charges the ledger it is passed", func(t *testing.T) {
		must := require.New(t)
		fn := findFunc(t, files, "harness/internal/core/turnrequest.go", "AgenticLoop.requestProvider")
		param := paramOfType(fn.decl, "TokenLedger")
		must.NotEmpty(param, "requestProvider takes no *TokenLedger")
		charged := false
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Charge" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == param {
						charged = true
					}
				}
			}
			return true
		})
		must.True(charged, "requestProvider never calls %s.Charge", param)
	})

	t.Run("auxiliary callers get the charging provider", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)
		builders := map[string]string{}
		for _, b := range auxiliaryConstructors {
			builders[b] = ""
		}
		builders["buildGuardRail"] = ""
		for name := range builders {
			fn := findFunc(t, files, "harness/internal/core/factory.go", name)
			builders[name] = paramOfType(fn.decl, "ProviderAdapter")
			must.NotEmpty(builders[name], "%s takes no ProviderAdapter", name)
		}

		constructed := map[string]int{}
		wiredFromAuxProv := 0
		eachCall(files, func(sf *sourceFile, encl funcDecl, call *ast.CallExpr) {
			name := calleeName(call)
			if builder, ok := auxiliaryConstructors[name]; ok {
				constructed[name]++
				where := position(fset, call.Pos())
				if !check.NotNil(encl.decl, "%s: %s outside a function", where, name) {
					return
				}
				check.Equal(builder, funcDeclName(encl.decl), "%s: %s must be built only by %s", where, name, builder)
				check.True(passesIdent(call, builders[builder]), "%s: %s must receive %s's provider parameter %q", where, name, builder, builders[builder])
				return
			}
			if _, ok := builders[name]; !ok || !strings.HasPrefix(sf.path, "harness/internal/core/") {
				return
			}
			where := position(fset, call.Pos())
			if !check.NotNil(encl.decl, "%s: %s outside a function", where, name) {
				return
			}
			if callerParam, isBuilder := builders[funcDeclName(encl.decl)]; isBuilder {
				check.True(passesIdent(call, callerParam), "%s: %s must forward the provider parameter %q", where, name, callerParam)
				return
			}
			wiredFromAuxProv++
			check.True(passesIdent(call, "auxProv"), "%s: %s must receive auxProv", where, name)
			check.True(assignedFrom(encl.decl, "auxProv", "newChargingProvider"), "%s: auxProv in %s must come from newChargingProvider", where, funcDeclName(encl.decl))
		})
		for name := range auxiliaryConstructors {
			check.Positive(constructed[name], "no production call to %s found; the check would be vacuous", name)
		}
		check.Positive(wiredFromAuxProv, "no factory call hands auxProv to a builder; the check would be vacuous")
	})
}

// findFunc returns the top-level function named name ("Recv.Func" for a
// method) declared in path.
func findFunc(t *testing.T, files []sourceFile, path, name string) funcDecl {
	t.Helper()
	for i := range files {
		if files[i].path != path {
			continue
		}
		for _, decl := range files[i].file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && funcDeclName(fd) == name {
				return funcDecl{file: &files[i], decl: fd}
			}
		}
	}
	require.New(t).FailNowf("function not found", "%s:%s not found; update the invariant", path, name)
	return funcDecl{}
}

// paramOfType returns the name of fn's first parameter whose type is T,
// *T, pkg.T or *pkg.T.
func paramOfType(fn *ast.FuncDecl, typeName string) string {
	for _, field := range fn.Type.Params.List {
		typ := field.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		matched := false
		switch tt := typ.(type) {
		case *ast.Ident:
			matched = tt.Name == typeName
		case *ast.SelectorExpr:
			matched = tt.Sel.Name == typeName
		}
		if matched && len(field.Names) > 0 {
			return field.Names[0].Name
		}
	}
	return ""
}

// passesIdent reports whether call passes the identifier name, directly or
// as a field of a composite-literal argument.
func passesIdent(call *ast.CallExpr, name string) bool {
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && id.Name == name {
			return true
		}
		lit, ok := arg.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// assignedFrom reports whether every assignment to name in fn is a call to
// the function fnName.
func assignedFrom(fn *ast.FuncDecl, name, fnName string) bool {
	values := assignedValues(fn, name)
	for _, v := range values {
		call, ok := v.(*ast.CallExpr)
		if !ok || calleeName(call) != fnName {
			return false
		}
	}
	return len(values) > 0
}
