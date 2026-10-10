package git

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPackageDoesNotImportOSExec pins that no production file in this
// package can start a host process: every git command must go through the
// injected executor. Test files are excluded because they build fixtures
// with the host's git.
func TestPackageDoesNotImportOSExec(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			if path == "os/exec" {
				t.Errorf("%s imports os/exec; git must run through the executor, never on the harness host", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production files checked; the import guard would be vacuous")
	}
}
