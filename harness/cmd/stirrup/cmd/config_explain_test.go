package cmd

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

// newTestExplainCommand returns a fresh cobra command preloaded with
// the explain flag surface. The real configExplainCmd is process-global;
// a per-test command keeps the Changed() bits scoped.
func newTestExplainCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "explain"}
	cmd.Flags().Bool("list", false, "")
	cmd.Flags().Bool("root", false, "")
	cmd.Flags().String("output", "text", "")
	return cmd
}

func runExplainForTest(t *testing.T, args []string) (string, string, error) {
	t.Helper()
	cmd := newTestExplainCommand()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := runConfigExplainWithIO(cmd, cmd.Flags().Args(), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// TestConfigExplain_LeafField pins acceptance criterion 1:
// `stirrup config explain mode` prints documentation for RunConfig.Mode,
// including the closed-set values from validRunModes.
func TestConfigExplain_LeafField(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"mode"})
	if err != nil {
		t.Fatalf("explain mode: %v", err)
	}
	for _, want := range []string{
		"KIND:    RunConfig",
		"FIELD:   mode",
		"VALID VALUES:",
		"execution",
		"planning",
		"review",
		"research",
		"toil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestConfigExplain_NestedFieldWithDefault pins the worked example
// from the issue: explain provider.batch.maxWaitSeconds surfaces the
// default and the range.
func TestConfigExplain_NestedFieldWithDefault(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"provider.batch.maxWaitSeconds"})
	if err != nil {
		t.Fatalf("explain provider.batch.maxWaitSeconds: %v", err)
	}
	if !strings.Contains(out, "DEFAULT:") {
		t.Errorf("missing DEFAULT line\n%s", out)
	}
	if !strings.Contains(out, "86400") {
		t.Errorf("missing default value 86400\n%s", out)
	}
	if !strings.Contains(out, "RELATED FIELDS:") {
		t.Errorf("missing RELATED FIELDS\n%s", out)
	}
	if !strings.Contains(out, "provider.batch.enabled") {
		t.Errorf("missing sibling provider.batch.enabled\n%s", out)
	}
}

// TestConfigExplain_StructPrintsChildren pins acceptance criterion 2:
// explain provider.batch prints an overview of every BatchProviderConfig
// field.
func TestConfigExplain_StructPrintsChildren(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"provider.batch"})
	if err != nil {
		t.Fatalf("explain provider.batch: %v", err)
	}
	if !strings.Contains(out, "CHILDREN:") {
		t.Errorf("missing CHILDREN section\n%s", out)
	}
	wantChildren := []string{
		"enabled",
		"maxWaitSeconds",
		"harnessSidePolling",
		"fallbackOnTimeout",
		"cancelBundleOnRunCancel",
		"allowInteractiveModes",
	}
	for _, w := range wantChildren {
		if !strings.Contains(out, w) {
			t.Errorf("missing child %q\n%s", w, out)
		}
	}
}

// TestConfigExplain_ListEmitsAlphaSortedLeaves pins acceptance criterion 3.
func TestConfigExplain_ListEmitsAlphaSortedLeaves(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"--list"})
	if err != nil {
		t.Fatalf("explain --list: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 30 {
		t.Fatalf("expected many leaf paths, got %d\n%s", len(lines), out)
	}
	if !sort.StringsAreSorted(lines) {
		t.Errorf("leaf paths are not sorted: %v", lines)
	}
	// Every line should resolve to a leaf entry in FieldDocs.
	for _, p := range lines {
		fd, ok := types.FieldDocs[p]
		if !ok {
			t.Errorf("leaf %q not in FieldDocs", p)
			continue
		}
		if fd.Kind != "leaf" {
			t.Errorf("path %q has kind %q, want leaf", p, fd.Kind)
		}
	}
}

// TestConfigExplain_MissingPathSuggests pins acceptance criterion 4.
func TestConfigExplain_MissingPathSuggests(t *testing.T) {
	_, _, err := runExplainForTest(t, []string{"not.a.real.path"})
	if err == nil {
		t.Fatalf("expected error for unknown path")
	}
	// Distance between "not.a.real.path" and any real path is too large
	// for a suggestion; only the plain error.
	if !strings.Contains(err.Error(), "no field at path") {
		t.Errorf("error %q lacks expected prefix", err.Error())
	}
}

func TestConfigExplain_MissingPath_NearMatch(t *testing.T) {
	_, _, err := runExplainForTest(t, []string{"provider.batch.maxWaitSecond"})
	if err == nil {
		t.Fatalf("expected error for typo path")
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("typo path %q lacks fuzzy-match suggestion: %v", "maxWaitSecond", err)
	}
	if !strings.Contains(err.Error(), "provider.batch.maxWaitSeconds") {
		t.Errorf("fuzzy suggestion did not point at canonical path: %v", err)
	}
}

// TestConfigExplain_JSONOutput pins acceptance criterion 5.
func TestConfigExplain_JSONOutput(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"--output=json", "mode"})
	if err != nil {
		t.Fatalf("explain mode --output=json: %v", err)
	}
	var fd types.FieldDoc
	if err := json.Unmarshal([]byte(out), &fd); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, out)
	}
	if fd.Path != "mode" {
		t.Errorf("FieldDoc.Path = %q, want mode", fd.Path)
	}
	if len(fd.Enum) == 0 {
		t.Errorf("FieldDoc.Enum unexpectedly empty: %+v", fd)
	}
}

func TestConfigExplain_Wildcard(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"provider.batch.*"})
	if err != nil {
		t.Fatalf("explain provider.batch.*: %v", err)
	}
	// Should contain multiple FIELD: lines, one per child.
	count := strings.Count(out, "FIELD:")
	if count < 5 {
		t.Errorf("expected >= 5 FIELD: entries, got %d\n%s", count, out)
	}
}

func TestConfigExplain_RootOverview(t *testing.T) {
	out, _, err := runExplainForTest(t, []string{"--root"})
	if err != nil {
		t.Fatalf("explain --root: %v", err)
	}
	if !strings.Contains(out, "KIND:    RunConfig") {
		t.Errorf("missing RunConfig kind line\n%s", out)
	}
	if !strings.Contains(out, "CHILDREN:") {
		t.Errorf("missing CHILDREN section\n%s", out)
	}
	if !strings.Contains(out, "provider") || !strings.Contains(out, "mode") {
		t.Errorf("expected provider and mode among children\n%s", out)
	}
}

func TestConfigExplain_FlagConflicts(t *testing.T) {
	if _, _, err := runExplainForTest(t, []string{"--list", "--root"}); err == nil {
		t.Errorf("expected error for --list + --root")
	}
	if _, _, err := runExplainForTest(t, []string{"--list", "mode"}); err == nil {
		t.Errorf("expected error for --list + arg")
	}
	if _, _, err := runExplainForTest(t, []string{"--root", "mode"}); err == nil {
		t.Errorf("expected error for --root + arg")
	}
}

func TestConfigExplain_InvalidOutput(t *testing.T) {
	if _, _, err := runExplainForTest(t, []string{"--output=yaml", "mode"}); err == nil {
		t.Errorf("expected error for unsupported --output")
	}
}

// TestFieldDocs_GeneratedShapeIsCoherent guards the generator output:
// every entry's ParentPath must itself exist in the map (or be ""),
// and every Children element must resolve.
func TestFieldDocs_GeneratedShapeIsCoherent(t *testing.T) {
	for path, fd := range types.FieldDocs {
		if path == "" {
			continue
		}
		if _, ok := types.FieldDocs[fd.ParentPath]; !ok {
			t.Errorf("path %q has parentPath %q not in FieldDocs", path, fd.ParentPath)
		}
		for _, c := range fd.Children {
			childPath := c
			if path != "" {
				childPath = path + "." + c
			}
			if _, ok := types.FieldDocs[childPath]; !ok {
				t.Errorf("path %q lists child %q which is not in FieldDocs", path, c)
			}
		}
	}
}
