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

// TestExecutorConformance runs the shared contract suite against every
// executor buildable without an external daemon. ContainerExecutor and the
// k8s/k8s-sandbox executors require a live Docker/Kubernetes endpoint and
// are covered by their own build-tagged integration suites instead; adding
// them here only needs a newExecutor factory entry in this table.
func TestExecutorConformance(t *testing.T) {
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

	t.Run("ListDirectory marks directories with a trailing slash", func(t *testing.T) {
		names, err := exec.ListDirectory(context.Background(), ".")
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		var gotDir, gotFile bool
		for _, name := range names {
			switch name {
			case "sub/":
				gotDir = true
			case "top.txt":
				gotFile = true
			}
		}
		if !gotDir {
			t.Errorf("expected \"sub/\" marked as a directory, got %v", names)
		}
		if !gotFile {
			t.Errorf("expected \"top.txt\" listed without a trailing slash, got %v", names)
		}
	})

	t.Run("ReadFile round-trips fixture content", func(t *testing.T) {
		got, err := exec.ReadFile(context.Background(), "top.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got != conformanceFixture["top.txt"] {
			t.Errorf("ReadFile(top.txt) = %q, want %q", got, conformanceFixture["top.txt"])
		}
	})

	t.Run("ResolvePath traversal", func(t *testing.T) {
		_, err := exec.ResolvePath("../escape")
		switch {
		case target.wantTraversalRejected && err == nil:
			t.Error("expected traversal outside the workspace to be rejected")
		case !target.wantTraversalRejected && err != nil:
			t.Errorf("executor has no filesystem boundary to escape; expected no error, got: %v", err)
		}
	})

	if lister, ok := exec.(TreeLister); ok {
		t.Run("ListTree covers the fixture tree", func(t *testing.T) {
			listing, err := lister.ListTree(context.Background(), ".")
			if err != nil {
				t.Fatalf("ListTree: %v", err)
			}
			if listing.Truncated {
				t.Error("a small fixture tree must not report Truncated")
			}
			seen := make(map[string]int64, len(listing.Entries))
			for _, e := range listing.Entries {
				if strings.Contains(e.Path, "\\") {
					t.Errorf("entry path must be slash-separated, got %q", e.Path)
				}
				if strings.HasPrefix(e.Path, "/") {
					t.Errorf("entry path must be root-relative, got %q", e.Path)
				}
				if e.Size < 0 {
					t.Errorf("entry size must be non-negative, got %d for %q", e.Size, e.Path)
				}
				seen[e.Path] = e.Size
			}
			for path := range conformanceFixture {
				if _, ok := seen[path]; !ok {
					t.Errorf("fixture file %q missing from tree listing: %v", path, seen)
				}
			}
		})
	}

	t.Run("Capabilities", func(t *testing.T) {
		if !exec.Capabilities().CanRead {
			t.Error("expected CanRead true")
		}
		_, hasHostPathWorkspace := exec.(HostPathWorkspace)
		if hasHostPathWorkspace != target.wantHostPathWorkspace {
			t.Errorf("implements HostPathWorkspace = %v, want %v", hasHostPathWorkspace, target.wantHostPathWorkspace)
		}
	})
}

func newConformanceLocalExecutor(t *testing.T) Executor {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	for relPath, content := range conformanceFixture {
		full := filepath.Join(dir, filepath.FromSlash(relPath))
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", relPath, err)
		}
	}
	exec, err := NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
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
