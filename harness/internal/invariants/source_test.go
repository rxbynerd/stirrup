package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	must.NoError(err, "read %s", dir)
	var files []sourceFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot, path), nil, parser.SkipObjectResolution)
		must.NoError(err, "parse %s", path)
		files = append(files, sourceFile{path: filepath.ToSlash(path), file: f})
	}
	must.NotEmpty(files, "no production Go files under %s; the check would be vacuous", dir)
	return files
}

// importName returns the name a file uses for the import path, or "" when
// the file does not import it.
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
