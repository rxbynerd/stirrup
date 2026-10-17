package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// vendorNamePattern matches the platform-specific names that must stay in
// this package. types/ names the closed set of platforms, and gen/ is
// generated from the proto that documents it.
var vendorNamePattern = regexp.MustCompile(`(?i)cloudwatch|phoenix|openinference|sigv4|aws\.service\.type|langfuse\.|x-langfuse-`)

// TestVendorNamesStayInPlatformPackage keeps every vendor-specific name in
// Go code inside this package, so the rest of the harness stays
// platform-neutral.
func TestVendorNamesStayInPlatformPackage(t *testing.T) {
	root := workspaceRoot(t)
	allowed := []string{
		filepath.Join(root, "harness", "internal", "observability", "platform") + string(filepath.Separator),
		filepath.Join(root, "types") + string(filepath.Separator),
		filepath.Join(root, "gen") + string(filepath.Separator),
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) {
				return nil
			}
		}
		scanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if m := vendorNamePattern.FindString(line); m != "" {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d mentions %q; vendor-specific names belong in harness/internal/observability/platform", rel, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files under %s; the workspace root was not found correctly", scanned, root)
	}
}

// workspaceRoot returns the directory holding go.work, above this package.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found above the test directory")
		}
		dir = parent
	}
}
