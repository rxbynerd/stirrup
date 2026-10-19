package platform

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vendorNamePattern matches the platform-specific names that must stay in
// this package. types/ names the closed set of platforms, and gen/ is
// generated from the proto that documents it.
var vendorNamePattern = regexp.MustCompile(`(?i:cloudwatch|phoenix|openinference|sigv4|aws\.service\.type|x-langfuse-)|\blangfuse\.(trace|observation|release|session|user|version|environment)\b|"langfuse"`)

func TestVendorNamePattern_MatchesCodeNotProse(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "langfuse trace attribute key", line: `attribute.String("langfuse.trace.name", v)`, want: true},
		{name: "langfuse observation key prefix", line: `"langfuse.observation.metadata." + k`, want: true},
		{name: "langfuse ingestion version header", line: `key := "x-langfuse-ingestion-version"`, want: true},
		{name: "langfuse registry key", line: `registry["langfuse"]`, want: true},
		{name: "prose mention in comment", line: `// so the trace name reaches Langfuse.`, want: false},
		{name: "prose sentence starting with the vendor name", line: `// Langfuse.Whatever is prose, not a key.`, want: false},
		{name: "env var name in cli flag", line: `--otel-header Authorization=secret://LANGFUSE_AUTH`, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := vendorNamePattern.MatchString(tc.line)
			require.Equal(t, tc.want, got, "vendorNamePattern.MatchString(%q)", tc.line)
		})
	}
}

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
	var violations []string
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
				violations = append(violations, fmt.Sprintf("%s:%d mentions %q", rel, i+1, m))
			}
		}
		return nil
	})
	require.NoError(t, err, "walk %s", root)
	assert.Empty(t, violations, "vendor-specific names belong in harness/internal/observability/platform")
	require.GreaterOrEqual(t, scanned, 100, "scanned Go files under %s; the workspace root was not found correctly", root)
}

// workspaceRoot returns the directory holding go.work, above this package.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			require.Fail(t, "go.work not found above the test directory")
		}
		dir = parent
	}
}
