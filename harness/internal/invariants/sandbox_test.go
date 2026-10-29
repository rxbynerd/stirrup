package invariants

import (
	"go/ast"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executorOnlyPackages reach the workspace only through the executor
// interface: run against a sandbox executor, a host read or host process
// would act on the harness host instead of the sandbox.
var executorOnlyPackages = []string{
	"harness/internal/git",
	"harness/internal/prompt",
	"harness/internal/workspaceexport",
}

// hostImports are packages whose use reads the host filesystem or starts
// a host process.
var hostImports = map[string]bool{
	"os":                    true,
	"os/exec":               true,
	"io/ioutil":             true,
	"syscall":               true,
	"golang.org/x/sys/unix": true,
}

// hostFilepathFuncs are path/filepath functions that consult the host
// filesystem rather than only manipulating path strings.
var hostFilepathFuncs = map[string]bool{
	"Abs":          true,
	"EvalSymlinks": true,
	"Glob":         true,
	"Walk":         true,
	"WalkDir":      true,
}

// TestExecutorOnlyPackages_DoNotTouchTheHost pins that no production file
// in the git, prompt and workspaceexport packages imports a host-access
// package or calls a host-reading path/filepath function. Test files are
// exempt: they build fixtures with the host's tools.
func TestExecutorOnlyPackages_DoNotTouchTheHost(t *testing.T) {
	for _, dir := range executorOnlyPackages {
		t.Run(dir, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			fset := token.NewFileSet()
			for _, sf := range parseProductionTree(t, fset, dir) {
				for _, imp := range sf.file.Imports {
					path, err := strconv.Unquote(imp.Path.Value)
					must.NoError(err)
					check.False(hostImports[path], "%s imports %s; reach the workspace through the executor", position(fset, imp.Pos()), path)
				}
				filepathName := importName(sf.file, "path/filepath")
				ast.Inspect(sf.file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					for name := range hostFilepathFuncs {
						check.False(isPkgSelector(call.Fun, filepathName, name),
							"%s calls filepath.%s, which reads the host filesystem", position(fset, call.Pos()), name)
					}
					return true
				})
			}
		})
	}
}
