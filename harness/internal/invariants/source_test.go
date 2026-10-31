package invariants

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot is the repository root relative to this package's directory,
// which is the working directory of `go test`.
const repoRoot = "../../.."

// sourceFile is one parsed production Go file.
type sourceFile struct {
	path string
	file *ast.File
}

// parseProductionFiles parses every non-test Go file directly inside dir,
// which is relative to the repository root. It fails the test when the
// directory holds none, so a moved package cannot make a check vacuous.
func parseProductionFiles(t *testing.T, fset *token.FileSet, dir string) []sourceFile {
	t.Helper()
	must := require.New(t)
	files, err := parseDir(fset, dir)
	must.NoError(err)
	must.NotEmpty(files, "no production Go files under %s; the check would be vacuous", dir)
	return files
}

// parseProductionTree parses every non-test Go file under dir, recursively,
// skipping testdata directories.
func parseProductionTree(t *testing.T, fset *token.FileSet, dir string) []sourceFile {
	t.Helper()
	must := require.New(t)
	var dirs []string
	err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		dirs = append(dirs, rel)
		return nil
	})
	must.NoError(err, "walk %s", dir)
	sort.Strings(dirs)
	var files []sourceFile
	for _, d := range dirs {
		parsed, err := parseDir(fset, d)
		must.NoError(err)
		files = append(files, parsed...)
	}
	must.NotEmpty(files, "no production Go files under %s; the check would be vacuous", dir)
	return files
}

func parseDir(fset *token.FileSet, dir string) ([]sourceFile, error) {
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var files []sourceFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, name))
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot, path), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		files = append(files, sourceFile{path: path, file: f})
	}
	return files, nil
}

// importName returns the name a file uses for the import path, or "" when
// the file does not import it. An unaliased import is named by the path's
// last element, which holds for every package these checks look for.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// isPkgSelector reports whether expr is pkg.name, pkg being a file-level
// import name.
func isPkgSelector(expr ast.Expr, pkg, name string) bool {
	if pkg == "" {
		return false
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

// calleeName returns the final identifier of a call's function: "f" for
// f(...) and "m" for x.y.m(...).
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// funcDecl is a top-level function together with the file declaring it.
type funcDecl struct {
	file *sourceFile
	decl *ast.FuncDecl
}

// site names the function at a location as "dir/file.go:Recv.Func", the
// key the allow-lists in this package use.
func (fd funcDecl) site() string {
	return fd.file.path + ":" + funcDeclName(fd.decl)
}

func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		return receiverTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiverTypeName(e.X)
	case *ast.IndexListExpr:
		return receiverTypeName(e.X)
	}
	return ""
}

// enclosingFunc returns the top-level function containing pos.
func enclosingFunc(sf *sourceFile, pos token.Pos) (funcDecl, bool) {
	for _, decl := range sf.file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if ok && fd.Body != nil && pos >= fd.Pos() && pos <= fd.End() {
			return funcDecl{file: sf, decl: fd}, true
		}
	}
	return funcDecl{}, false
}

// eachCall calls fn for every call expression in files, with the
// top-level function containing it (zero for package-level calls).
func eachCall(files []sourceFile, fn func(sf *sourceFile, encl funcDecl, call *ast.CallExpr)) {
	for i := range files {
		sf := &files[i]
		ast.Inspect(sf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			encl, _ := enclosingFunc(sf, call.Pos())
			fn(sf, encl, call)
			return true
		})
	}
}

// callsIn reports whether body contains a call to a function named name.
func callsIn(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == name {
			found = true
		}
		return !found
	})
	return found
}

// allowList records the sites an invariant exempts, with the reason for
// each, and fails the test for an entry no site used, so the list cannot
// outlive the code it describes.
type allowList struct {
	reasons map[string]string
	used    map[string]bool
}

func newAllowList(reasons map[string]string) *allowList {
	return &allowList{reasons: reasons, used: map[string]bool{}}
}

func (a *allowList) allows(site string) bool {
	if _, ok := a.reasons[site]; ok {
		a.used[site] = true
		return true
	}
	return false
}

func (a *allowList) assertAllUsed(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	for site := range a.reasons {
		check.True(a.used[site], "allow-list entry %q matched no site; remove it", site)
	}
}

// position formats a node's position relative to the repository root.
func position(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	rel, err := filepath.Rel(repoRoot, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), p.Line)
}
