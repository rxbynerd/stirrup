package executor

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAPIExecutor(serverURL string) *APIExecutor {
	e := NewAPIExecutor("test-token", "octocat", "hello-world", "main")
	e.baseURL = serverURL
	return e
}

func TestAPIExecutor_ReadFile_Success(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/contents/main.go"), "unexpected path: %s", r.URL.Path)
		assert.Equal(t, "main", r.URL.Query().Get("ref"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/vnd.github.v3.raw", r.Header.Get("Accept"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	got, err := e.ReadFile(context.Background(), "main.go")
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestAPIExecutor_ReadFile_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	_, err := e.ReadFile(context.Background(), "nonexistent.go")
	require.Error(t, err)
	require.ErrorContains(t, err, "HTTP 404")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestAPIExecutor_ListDirectory_Success(t *testing.T) {
	entries := []githubContentEntry{
		{Name: "README.md"},
		{Name: "main.go"},
		{Name: "go.mod"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/vnd.github.v3+json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	names, err := e.ListDirectory(context.Background(), ".")
	require.NoError(t, err)
	require.Equal(t, []string{"README.md", "main.go", "go.mod"}, names)
}

func TestAPIExecutor_ListDirectory_MarksDirectoriesWithTrailingSlash(t *testing.T) {
	entries := []githubContentEntry{
		{Name: "docs", Type: "dir"},
		{Name: "go.work", Type: "file"},
		{Name: "link", Type: "symlink"},
		{Name: "vendored", Type: "submodule"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	names, err := e.ListDirectory(context.Background(), ".")
	require.NoError(t, err)
	require.Equal(t, []string{"docs/", "go.work", "link", "vendored"}, names)
}

func TestAPIExecutor_ListDirectory_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	names, err := e.ListDirectory(context.Background(), "empty-dir")
	require.NoError(t, err)
	require.Empty(t, names)
}

func TestAPIExecutor_WriteFile_Unsupported(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	err := e.WriteFile(context.Background(), "file.txt", "content")
	require.Error(t, err)
	require.ErrorContains(t, err, "write operations not supported")
}

func TestAPIExecutor_Exec_Unsupported(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	_, err := e.Exec(context.Background(), "ls", time.Second)
	require.Error(t, err)
	require.ErrorContains(t, err, "command execution not supported")
}

func TestAPIExecutor_ResolvePath(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	got, err := e.ResolvePath("some/path")
	require.NoError(t, err)
	require.Equal(t, "some/path", got)
}

func TestAPIExecutor_Capabilities(t *testing.T) {
	e := NewAPIExecutor("token", "owner", "repo", "ref")
	caps := e.Capabilities()

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{name: "can read", got: caps.CanRead, want: true},
		{name: "can write", got: caps.CanWrite, want: false},
		{name: "can exec", got: caps.CanExec, want: false},
		{name: "can network", got: caps.CanNetwork, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.got)
		})
	}
}

func TestAPIExecutor_ReadFile_OmitsRefWhenUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.Query().Get("ref"))
		_, _ = w.Write([]byte("content"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.ref = ""
	_, err := e.ReadFile(context.Background(), "file.txt")
	require.NoError(t, err)
}

func TestAPIExecutor_EncodesPathAndRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.EscapedPath(), "/contents/dir%20with%20spaces/file%23name.go")
		assert.Equal(t, "feature/bugfix?one=1", r.URL.Query().Get("ref"))
		_, _ = w.Write([]byte("content"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.ref = "feature/bugfix?one=1"

	_, err := e.ReadFile(context.Background(), "dir with spaces/file#name.go")
	require.NoError(t, err)
}

func TestAPIExecutor_EmptyToken_OmitsAuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("public content"))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.token = ""

	content, err := e.ReadFile(context.Background(), "file.txt")
	require.NoError(t, err)
	require.Equal(t, "public content", content)
}

func TestAPIExecutor_EmptyToken_ListDirectory_OmitsAuthHeader(t *testing.T) {
	entries := []githubContentEntry{{Name: "file.txt"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.token = ""

	names, err := e.ListDirectory(context.Background(), ".")
	require.NoError(t, err)
	require.Equal(t, []string{"file.txt"}, names)
}

const testTreeResponse = `{
  "sha": "abc123",
  "tree": [
    {"path": "docs", "type": "tree"},
    {"path": "docs/guide.md", "type": "blob", "size": 42},
    {"path": "harness", "type": "tree"},
    {"path": "harness/main.go", "type": "blob", "size": 128},
    {"path": "harness/internal", "type": "tree"},
    {"path": "harness/internal/api.go", "type": "blob", "size": 256},
    {"path": "vendored", "type": "commit"}
  ],
  "truncated": false
}`

func TestAPIExecutor_ListTree_ReturnsBlobsOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasSuffix(r.URL.Path, "/git/trees/main"), "unexpected path: %s", r.URL.Path)
		assert.Equal(t, "1", r.URL.Query().Get("recursive"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(testTreeResponse))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	listing, err := e.ListTree(context.Background(), ".")
	require.NoError(t, err)
	require.False(t, listing.Truncated)
	require.Equal(t, []TreeEntry{
		{Path: "docs/guide.md", Size: 42},
		{Path: "harness/main.go", Size: 128},
		{Path: "harness/internal/api.go", Size: 256},
	}, listing.Entries)
}

func TestAPIExecutor_ListTree_ScopesToRootAndStripsPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testTreeResponse))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	listing, err := e.ListTree(context.Background(), "harness")
	require.NoError(t, err)

	got := make([]string, len(listing.Entries))
	for i, entry := range listing.Entries {
		got[i] = entry.Path
	}
	require.Equal(t, []string{"main.go", "internal/api.go"}, got)
}

func TestAPIExecutor_ListTree_ReportsUpstreamTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tree": [{"path": "a.go", "type": "blob", "size": 1}], "truncated": true}`))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	listing, err := e.ListTree(context.Background(), ".")
	require.NoError(t, err)
	require.True(t, listing.Truncated, "expected Truncated to be true when GitHub caps the tree")
}

func TestAPIExecutor_ListTree_UnsetRefUsesHEAD(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasSuffix(r.URL.Path, "/git/trees/HEAD"), "expected HEAD tree-ish, got path %s", r.URL.Path)
		_, _ = w.Write([]byte(`{"tree": [], "truncated": false}`))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.ref = ""
	_, err := e.ListTree(context.Background(), ".")
	require.NoError(t, err)
}

func TestAPIExecutor_ListTree_SlashedRefKeepsSeparators(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasSuffix(r.URL.Path, "/git/trees/docs/api guide"), "unexpected path: %s", r.URL.Path)
		assert.True(t, strings.HasSuffix(r.URL.EscapedPath(), "/git/trees/docs/api%20guide"), "unexpected escaped path: %s", r.URL.EscapedPath())
		_, _ = w.Write([]byte(`{"tree": [], "truncated": false}`))
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	e.ref = "docs/api guide"
	_, err := e.ListTree(context.Background(), ".")
	require.NoError(t, err)
}

func TestAPIExecutor_ListTree_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	e := newTestAPIExecutor(server.URL)
	_, err := e.ListTree(context.Background(), ".")
	require.Error(t, err)
	require.ErrorContains(t, err, "HTTP 403")
}

// Verify that APIExecutor satisfies the Executor interface.
var _ Executor = (*APIExecutor)(nil)

// The api executor has no host filesystem, so the search tools depend on
// TreeLister to enumerate the repository instead of walking the harness
// process's own working directory.
var _ TreeLister = (*APIExecutor)(nil)
