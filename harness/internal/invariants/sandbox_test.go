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
	"os":                       true,
	"os/exec":                  true,
	"io/ioutil":                true,
	"syscall":                  true,
	"os/user":                  true,
	"golang.org/x/sys/unix":    true,
	"golang.org/x/sys/execabs": true,
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

// hostTemplateFuncs are text/template and html/template functions that read
// template files from the host. ParseFS is absent: it reads an fs.FS, and
// reaching the host through one needs os.DirFS, which the os ban covers.
var hostTemplateFuncs = map[string]bool{
	"ParseFiles": true,
	"ParseGlob":  true,
}

// TestExecutorOnlyPackages_DoNotTouchTheHost pins that no production file
// in the git, prompt and workspaceexport packages imports a host-access
// package or calls a host-reading path/filepath or template-file function. Test
// files are exempt: they build fixtures with the host's tools. Packages
// these import are not followed; a shared helper that reads the host
// needs its own entry.
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
				templateNames := []string{importName(sf.file, "text/template"), importName(sf.file, "html/template")}
				ast.Inspect(sf.file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					for name := range hostFilepathFuncs {
						check.False(isPkgSelector(call.Fun, filepathName, name),
							"%s calls filepath.%s, which reads the host filesystem", position(fset, call.Pos()), name)
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && hostTemplateFuncs[sel.Sel.Name] {
						check.False(templateNames[0] != "" || templateNames[1] != "",
							"%s calls %s, which reads template files from the host filesystem", position(fset, call.Pos()), sel.Sel.Name)
					}
					return true
				})
			}
		})
	}
}
