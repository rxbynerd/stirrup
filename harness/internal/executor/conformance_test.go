package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conformanceFixture is the minimal tree every conformance target must
// expose: one top-level file and one file in a subdirectory, so
// ListDirectory's directory-marking and TreeLister's recursive coverage both
// have something to prove.
var conformanceFixture = map[string]string{
	"top.txt":        "hello\n",
	"sub/nested.txt": "nested content\n",
}

// conformanceTarget names one executor conformance run and the expectations
// that vary per executor.
type conformanceTarget struct {
	name        string
	newExecutor func(t *testing.T) Executor
	// capabilitiesOnly restricts the run to the Capabilities subtest. The
	// container and k8s executors need a live daemon or cluster for the I/O
	// subtests, but their HostPathWorkspace declaration is a type property
	// that a bare struct literal can check.
	capabilitiesOnly bool
	// wantHostPathWorkspace pins whether the executor is expected to
	// declare HostPathWorkspace, forcing a future executor author to
	// decide explicitly rather than inheriting the capability by accident.
	wantHostPathWorkspace bool
	// wantTraversalRejected is false for executors with no local
	// filesystem to escape: APIExecutor.ResolvePath is an identity
	// function by design (see api.go) since a relative-path GitHub API
	// request has no host meaning to contain.
	wantTraversalRejected bool
}

// TestExecutor_Conformance runs the shared contract suite against every
// executor buildable without an external daemon. The container/k8s/
// k8s-sandbox targets only run the Capabilities subtest (see
// conformanceTarget.capabilitiesOnly); their I/O contracts are covered by
// their own build-tagged integration suites instead.
func TestExecutor_Conformance(t *testing.T) {
	targets := []conformanceTarget{
		{
			name:                  "local",
			newExecutor:           newConformanceLocalExecutor,
			wantHostPathWorkspace: true,
			wantTraversalRejected: true,
		},
		{
			name:                  "api",
			newExecutor:           newConformanceAPIExecutor,
			wantHostPathWorkspace: false,
			wantTraversalRejected: false,
		},
		{
			name:                  "container",
			newExecutor:           func(*testing.T) Executor { return &ContainerExecutor{workspace: containerWorkspace} },
			capabilitiesOnly:      true,
			wantHostPathWorkspace: false,
		},
		{
			name:                  "k8s",
			newExecutor:           func(*testing.T) Executor { return &K8sExecutor{} },
			capabilitiesOnly:      true,
			wantHostPathWorkspace: false,
		},
		{
			name:                  "k8s-sandbox",
			newExecutor:           func(*testing.T) Executor { return &AgentSandboxExecutor{} },
			capabilitiesOnly:      true,
			wantHostPathWorkspace: false,
		},
	}

	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			runExecutorConformance(t, target)
		})
	}
}

func runExecutorConformance(t *testing.T, target conformanceTarget) {
	t.Helper()
	exec := target.newExecutor(t)

	if !target.capabilitiesOnly {
		t.Run("list directory marks directories with a trailing slash", func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			names, err := exec.ListDirectory(context.Background(), ".")
			must.NoError(err)
			check.Contains(names, "sub/", "directories are listed with a trailing slash")
			check.Contains(names, "top.txt", "files are listed without a trailing slash")
		})

		t.Run("read file round-trips fixture content", func(t *testing.T) {
			must := require.New(t)
			got, err := exec.ReadFile(context.Background(), "top.txt")
			must.NoError(err)
			must.Equal(conformanceFixture["top.txt"], got)
		})

		t.Run("resolve path traversal", func(t *testing.T) {
			must := require.New(t)
			_, err := exec.ResolvePath("../escape")
			if target.wantTraversalRejected {
				must.Error(err, "traversal outside the workspace must be rejected")
			} else {
				must.NoError(err, "executor has no filesystem boundary to escape")
			}
		})

		if lister, ok := exec.(TreeLister); ok {
			t.Run("list tree covers fixture tree", func(t *testing.T) {
				must := require.New(t)
				check := assert.New(t)
				listing, err := lister.ListTree(context.Background(), ".")
				must.NoError(err)
				must.False(listing.Truncated, "a small fixture tree must not report truncated")

				seen := make(map[string]int64, len(listing.Entries))
				for _, e := range listing.Entries {
					check.NotContains(e.Path, "\\", "entry path must be slash-separated")
					check.False(strings.HasPrefix(e.Path, "/"), "entry path must be root-relative: %q", e.Path)
					check.GreaterOrEqual(e.Size, int64(0), "entry size must be non-negative: %q", e.Path)
					seen[e.Path] = e.Size
				}
				for path := range conformanceFixture {
					check.Contains(seen, path, "fixture file missing from tree listing")
				}
			})
		}
	}

	t.Run("capabilities", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)
		check.True(exec.Capabilities().CanRead)

		hostPathWorkspace, hasHostPathWorkspace := exec.(HostPathWorkspace)
		check.Equal(target.wantHostPathWorkspace, hasHostPathWorkspace, "implements HostPathWorkspace")

		// A declaring executor's HostWorkspaceRoot() must actually name its
		// resolved workspace root, not just satisfy the interface shape —
		// ResolvePath(".") is that root by construction for every executor.
		if hasHostPathWorkspace && !target.capabilitiesOnly {
			resolvedRoot, err := exec.ResolvePath(".")
			must.NoError(err)
			must.Equal(resolvedRoot, hostPathWorkspace.HostWorkspaceRoot())
		}
	})
}

func newConformanceLocalExecutor(t *testing.T) Executor {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "sub"), 0o755), "create fixture subdirectory")
	for relPath, content := range conformanceFixture {
		full := filepath.Join(dir, filepath.FromSlash(relPath))
		must.NoError(os.WriteFile(full, []byte(content), 0o644), "write fixture %s", relPath)
	}
	exec, err := NewLocalExecutor(dir)
	must.NoError(err)
	return exec
}

func newConformanceAPIExecutor(t *testing.T) Executor {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/"):
			serveConformanceTree(w)
		case strings.HasSuffix(r.URL.Path, "/contents"):
			serveConformanceDirListing(w)
		default:
			serveConformanceFileContent(w, r)
		}
	}))
	t.Cleanup(server.Close)

	e := NewAPIExecutor("test-token", "octocat", "hello-world", "main")
	e.baseURL = server.URL
	return e
}

func serveConformanceDirListing(w http.ResponseWriter) {
	entries := []githubContentEntry{
		{Name: "sub", Type: "dir"},
		{Name: "top.txt", Type: "file"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func serveConformanceFileContent(w http.ResponseWriter, r *http.Request) {
	const marker = "/contents/"
	idx := strings.Index(r.URL.Path, marker)
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	requested := r.URL.Path[idx+len(marker):]
	content, ok := conformanceFixture[requested]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func serveConformanceTree(w http.ResponseWriter) {
	type treeEntry struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	entries := make([]treeEntry, 0, len(conformanceFixture))
	for path, content := range conformanceFixture {
		entries = append(entries, treeEntry{Path: path, Type: "blob", Size: int64(len(content))})
	}
	payload := struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}{Tree: entries, Truncated: false}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
