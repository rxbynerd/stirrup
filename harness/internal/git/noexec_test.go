package git

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackage_DoesNotImportOSExec pins that no production file in this
// package can start a host process: every git command must go through the
// injected executor. Test files are excluded because they build fixtures
// with the host's git.
func TestPackage_DoesNotImportOSExec(t *testing.T) {
	must := require.New(t)
	entries, err := os.ReadDir(".")
	must.NoError(err)

	checked := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
			must.NoError(err)
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				must.NoError(err, "unquote import %s", imp.Path.Value)
				must.NotEqual("os/exec", path,
					"git must run through the executor, never on the harness host")
			}
		})
	}
	must.NotZero(checked, "no production files checked; the import guard would be vacuous")
}
