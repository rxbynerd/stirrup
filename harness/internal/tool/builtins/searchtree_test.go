package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// treeExecutor is a read-only Executor whose workspace exists only as an
// in-memory tree, mirroring the api executor's shape: ResolvePath is a no-op,
// there is no host directory to walk, and enumeration is served by ListTree.
type treeExecutor struct {
	files     map[string]string
	truncated bool
	readErr   map[string]error
	reads     atomic.Int32
}

func newTreeExecutor(files map[string]string) *treeExecutor {
	return &treeExecutor{files: files, readErr: map[string]error{}}
}

func (e *treeExecutor) ReadFile(_ context.Context, path string) (string, error) {
	e.reads.Add(1)
	if err, ok := e.readErr[path]; ok {
		return "", err
	}
	content, ok := e.files[path]
	if !ok {
		return "", fmt.Errorf("no such file: %s", path)
	}
	return content, nil
}

func (e *treeExecutor) WriteFile(context.Context, string, string) error {
	return fmt.Errorf("write operations not supported")
}

func (e *treeExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("not used by these tests")
}

func (e *treeExecutor) Exec(context.Context, string, time.Duration) (*executor.ExecResult, error) {
	return nil, fmt.Errorf("command execution not supported")
}

func (e *treeExecutor) ResolvePath(path string) (string, error) { return path, nil }

func (e *treeExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanNetwork: true}
}

func (e *treeExecutor) ListTree(_ context.Context, root string) (executor.TreeListing, error) {
	prefix := strings.Trim(root, "/")
	if prefix == "." {
		prefix = ""
	}
	if prefix != "" {
		prefix += "/"
	}
	paths := make([]string, 0, len(e.files))
	for path := range e.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	entries := make([]executor.TreeEntry, 0, len(paths))
	for _, path := range paths {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		entries = append(entries, executor.TreeEntry{
			Path: strings.TrimPrefix(path, prefix),
			Size: int64(len(e.files[path])),
		})
	}
	return executor.TreeListing{Entries: entries, Truncated: e.truncated}, nil
}

// unreachableWorkspaceExecutor exposes neither a host path (ResolvePath is a
// no-op) nor a tree, standing in for any future executor whose workspace the
// search tools cannot reach.
type unreachableWorkspaceExecutor struct{}

func (e *unreachableWorkspaceExecutor) ReadFile(context.Context, string) (string, error) {
	return "", fmt.Errorf("not used by these tests")
}

func (e *unreachableWorkspaceExecutor) WriteFile(context.Context, string, string) error {
	return fmt.Errorf("write operations not supported")
}

func (e *unreachableWorkspaceExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("not used by these tests")
}

func (e *unreachableWorkspaceExecutor) Exec(context.Context, string, time.Duration) (*executor.ExecResult, error) {
	return nil, fmt.Errorf("command execution not supported")
}

func (e *unreachableWorkspaceExecutor) ResolvePath(path string) (string, error) { return path, nil }

func (e *unreachableWorkspaceExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true}
}

// plantHostCanary moves the test process into a temp directory holding a file
// no workspace tool may surface, so any walk of the process working directory
// shows up as a match.
func plantHostCanary(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "host_canary.txt"), []byte("HOSTFS_CANARY leaked-from-host-filesystem\n"), 0o644)
	require.NoError(t, err, "write host canary")
	t.Chdir(dir)
}

func runSearchTool(t *testing.T, tl *tool.Tool, input map[string]any) tool.StructuredResult {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err, "marshal input")
	res, err := tl.StructuredHandler(context.Background(), raw)
	require.NoError(t, err, "%s: unexpected error", tl.Name)
	return res
}

func TestGrepFilesTool_TreeBackedExecutorNeverReadsHostWorkingDirectory(t *testing.T) {
	plantHostCanary(t)
	exec := newTreeExecutor(map[string]string{
		"harness/internal/executor/api.go": "func NewAPIExecutor(token string) *APIExecutor {\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "HOSTFS_CANARY"})
	require.Equal(t, noMatchesText, res.Text, "host working-directory content reached the model")
	require.NotContains(t, res.Text, "host_canary", "host path leaked into results")

	res = runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "func NewAPIExecutor"})
	want := "harness/internal/executor/api.go:1:func NewAPIExecutor(token string) *APIExecutor {"
	require.Equal(t, want, res.Text, "repository content not searched")
}

func TestFindFilesTool_TreeBackedExecutorNeverReadsHostWorkingDirectory(t *testing.T) {
	plantHostCanary(t)
	exec := newTreeExecutor(map[string]string{
		"harness/internal/executor/api.go": "package executor\n",
		"types/runconfig.go":               "package types\n",
	})

	res := runSearchTool(t, FindFilesTool(exec), map[string]any{"name": "*.txt"})
	require.Equal(t, noMatchesText, res.Text, "host working-directory content reached the model")

	res = runSearchTool(t, FindFilesTool(exec), map[string]any{"name": "*.go"})
	want := "harness/internal/executor/api.go\ntypes/runconfig.go"
	assert.Equal(t, want, res.Text, "repository tree not searched")
	assert.Zero(t, exec.reads.Load(), "name matching needs the tree only, not file contents")
}

func TestFindFilesTool_TreeBackedExecutorScopesResultsToSearchPath(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"harness/internal/api.go": "package internal\n",
		"harness/main.go":         "package main\n",
		"types/runconfig.go":      "package types\n",
	})

	res := runSearchTool(t, FindFilesTool(exec), map[string]any{"name": "*.go", "path": "harness"})
	require.Equal(t, "internal/api.go\nmain.go", res.Text, "paths must be relative to the search root")
}

func TestGrepFilesTool_TreeBackedExecutorReadsPathsRelativeToRepositoryRoot(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"harness/internal/api.go": "needle here\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle", "path": "harness"})
	require.Equal(t, "internal/api.go:1:needle here", res.Text, "match path must be relative to the search root")
}

func TestGrepFilesTool_TreeBackedExecutorAppliesIncludeAndExclude(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"a.go":               "needle\n",
		"b.md":               "needle\n",
		"testdata/vendor.go": "needle\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{
		"pattern": "needle",
		"include": []string{"*.go"},
		"exclude": []string{"testdata/*"},
	})
	assert.Equal(t, "a.go:1:needle", res.Text, "filters not applied")
	assert.EqualValues(t, 1, exec.reads.Load(), "filtered-out files must not be fetched")
}

func TestGrepFilesTool_TreeBackedExecutorSkipsBinaryContent(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"blob.bin": "needle\x00binary\n",
		"text.txt": "needle\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle"})
	require.Equal(t, "text.txt:1:needle", res.Text, "binary content must be skipped")
}

func TestGrepFilesTool_TreeBackedExecutorBoundsFilesFetchedPerCall(t *testing.T) {
	files := map[string]string{}
	for i := range treeSearchMaxFiles + 50 {
		files[fmt.Sprintf("pkg/file%03d.go", i)] = "needle\n"
	}
	exec := newTreeExecutor(files)

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle", "max_results": 1000})
	assert.Equal(t, treeSearchMaxFiles, int(exec.reads.Load()), "a scan must stop at the file cap")
	assert.True(t, strings.HasSuffix(res.Text, treeSearchIncompleteNotice),
		"a capped scan must tell the model it was partial, got: %q", res.Text)

	var got searchResult
	require.NoError(t, json.Unmarshal(res.Structured, &got), "structured payload is not a searchResult")
	assert.True(t, got.Truncated, "a capped scan must be reported as truncated")
}

func TestGrepFilesTool_TreeBackedExecutorSkipsOversizedFiles(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"big.json":  "needle" + strings.Repeat(" ", treeSearchMaxFileSize),
		"small.txt": "needle\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle"})
	assert.Equal(t, "small.txt:1:needle", res.Text, "oversized files must be skipped")
	assert.EqualValues(t, 1, exec.reads.Load(), "oversized files must not be fetched")
}

func TestGrepFilesTool_TreeBackedExecutorReportsUnreadableFilesAsIncomplete(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"a.go": "needle\n",
		"b.go": "needle\n",
	})
	exec.readErr["b.go"] = fmt.Errorf("HTTP 403")

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle"})
	assert.Contains(t, res.Text, "a.go:1:needle", "readable files must still produce matches")
	assert.True(t, strings.HasSuffix(res.Text, treeSearchIncompleteNotice),
		"an unreadable file must mark the scan partial, got: %q", res.Text)
}

func TestFindFilesTool_TreeBackedExecutorReportsUpstreamTruncationAsIncomplete(t *testing.T) {
	exec := newTreeExecutor(map[string]string{"a.go": "package a\n"})
	exec.truncated = true

	res := runSearchTool(t, FindFilesTool(exec), map[string]any{"name": "*.go"})
	assert.True(t, strings.HasSuffix(res.Text, treeSearchIncompleteNotice),
		"a truncated tree must mark the enumeration partial, got: %q", res.Text)

	var got findResult
	require.NoError(t, json.Unmarshal(res.Structured, &got), "structured payload is not a findResult")
	assert.True(t, got.Truncated, "a truncated tree must be reported as truncated")
}

func TestSearchTools_ExecutorWithoutHostPathOrTreeFailsClosed(t *testing.T) {
	plantHostCanary(t)
	exec := &unreachableWorkspaceExecutor{}

	for _, tc := range []struct {
		name  string
		tl    *tool.Tool
		input map[string]any
	}{
		{"grep files", GrepFilesTool(exec), map[string]any{"pattern": "HOSTFS_CANARY"}},
		{"find files", FindFilesTool(exec), map[string]any{"name": "*.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.input)
			require.NoError(t, err, "marshal input")
			res, err := tc.tl.StructuredHandler(context.Background(), raw)
			require.Error(t, err, "expected an error rather than a host walk, got: %q", res.Text)
			require.ErrorContains(t, err, "does not expose a searchable workspace",
				"error should name the missing workspace")
		})
	}
}

// TestGrepFilesTool_TreeListerOmitsColumn pins the TreeLister branch's half of
// the searchMatch.Column contract: it matches with a regexp and keeps no match
// offset, so the field must be absent rather than defaulted to 0.
func TestGrepFilesTool_TreeListerOmitsColumn(t *testing.T) {
	exec := newTreeExecutor(map[string]string{
		"a.go": "package a\nvar needle = 1\n",
	})

	res := runSearchTool(t, GrepFilesTool(exec), map[string]any{"pattern": "needle"})
	assertNoColumnKey(t, res.Structured)
}
