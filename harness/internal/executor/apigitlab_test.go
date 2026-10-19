package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestGitLabExecutor(serverURL string) *GitLabExecutor {
	e := NewGitLabExecutor("test-token", "acme/team/hello-world", "main")
	e.baseURL = serverURL + "/api/v4"
	return e
}

// TestGitLabExecutor_RequestShape pins the wire shape of every request the
// executor issues: the project path and the file path are single path
// segments, so their slashes must stay percent-encoded or GitLab resolves a
// different project.
func TestGitLabExecutor_RequestShape(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		ref       string
		call      func(context.Context, *GitLabExecutor) error
		wantPath  string
		wantQuery map[string]string
		absent    []string
	}{
		{
			name:    "read file escapes project and path",
			project: "acme/team/hello-world",
			ref:     "main",
			call: func(ctx context.Context, e *GitLabExecutor) error {
				_, err := e.ReadFile(ctx, "docs/api guide.md")
				return err
			},
			wantPath:  "/api/v4/projects/acme%2Fteam%2Fhello-world/repository/files/docs%2Fapi%20guide.md/raw",
			wantQuery: map[string]string{"ref": "main"},
		},
		{
			name:    "read file without ref omits the query param",
			project: "acme/hello-world",
			call: func(ctx context.Context, e *GitLabExecutor) error {
				_, err := e.ReadFile(ctx, "README.md")
				return err
			},
			wantPath: "/api/v4/projects/acme%2Fhello-world/repository/files/README.md/raw",
			absent:   []string{"ref"},
		},
		{
			name:    "read file trims leading slash",
			project: "acme/hello-world",
			ref:     "v1.0",
			call: func(ctx context.Context, e *GitLabExecutor) error {
				_, err := e.ReadFile(ctx, "/main.go")
				return err
			},
			wantPath:  "/api/v4/projects/acme%2Fhello-world/repository/files/main.go/raw",
			wantQuery: map[string]string{"ref": "v1.0"},
		},
		{
			name:    "list root omits the path param",
			project: "acme/hello-world",
			ref:     "main",
			call: func(ctx context.Context, e *GitLabExecutor) error {
				_, err := e.ListDirectory(ctx, ".")
				return err
			},
			wantPath:  "/api/v4/projects/acme%2Fhello-world/repository/tree",
			wantQuery: map[string]string{"ref": "main", "per_page": "100", "page": "1"},
			absent:    []string{"path"},
		},
		{
			name:    "list subdirectory sends the raw path param",
			project: "acme/hello-world",
			ref:     "main",
			call: func(ctx context.Context, e *GitLabExecutor) error {
				_, err := e.ListDirectory(ctx, "harness/internal/")
				return err
			},
			wantPath:  "/api/v4/projects/acme%2Fhello-world/repository/tree",
			wantQuery: map[string]string{"path": "harness/internal", "ref": "main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotQuery = r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[]"))
			}))
			defer server.Close()

			e := newTestGitLabExecutor(server.URL)
			e.project = tt.project
			e.ref = tt.ref

			require.NoError(t, tt.call(context.Background(), e))
			require.Equal(t, tt.wantPath, gotPath)
			for key, want := range tt.wantQuery {
				require.Equal(t, want, gotQuery.Get(key), "query %q", key)
			}
			for _, key := range tt.absent {
				require.NotContains(t, gotQuery, key, "query %q should be absent", key)
			}
		})
	}
}

func TestGitLabExecutor_ReadFile_ContentAndErrors(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	tests := []struct {
		name         string
		status       int
		body         string
		path         string
		want         string
		wantErr      string
		wantNotExist bool
	}{
		{name: "success", status: http.StatusOK, body: content, path: "main.go", want: content},
		{name: "not found", status: http.StatusNotFound, body: `{"message":"404 File Not Found"}`, path: "nope.go", wantErr: "HTTP 404", wantNotExist: true},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"message":"401 Unauthorized"}`, path: "main.go", wantErr: "HTTP 401"},
		{name: "empty path", status: http.StatusOK, body: content, path: "/", wantErr: "path is empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			got, err := newTestGitLabExecutor(server.URL).ReadFile(context.Background(), tt.path)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Equal(t, tt.wantNotExist, errors.Is(err, fs.ErrNotExist), "errors.Is(err, fs.ErrNotExist)")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestGitLabExecutor_ListDirectory_DirectoryMarkers pins the trailing slash
// on tree entries: list_directory's recursion and its "directories carry a
// trailing slash" contract both depend on it.
func TestGitLabExecutor_ListDirectory_DirectoryMarkers(t *testing.T) {
	entries := []gitlabTreeEntry{
		{Name: "docs", Type: "tree"},
		{Name: "go.mod", Type: "blob"},
		{Name: "vendored", Type: "commit"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Accept"), "Accept header")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	names, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), ".")
	require.NoError(t, err)
	require.Equal(t, []string{"docs/", "go.mod", "vendored"}, names)
}

func TestGitLabExecutor_ListDirectory_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	names, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), "empty-dir")
	require.NoError(t, err)
	require.Empty(t, names)
}

// TestGitLabExecutor_ListDirectory_Paginates covers the tree endpoint's
// per-page ceiling: a full page means more entries may follow, and the walk
// stops at gitlabTreeMaxPages so one listing cannot issue unbounded requests.
func TestGitLabExecutor_ListDirectory_Paginates(t *testing.T) {
	tests := []struct {
		name       string
		totalPages int
		lastPage   int
		wantNames  int
		wantCalls  int
	}{
		{name: "single short page", totalPages: 1, lastPage: 3, wantNames: 3, wantCalls: 1},
		{name: "two pages", totalPages: 2, lastPage: 7, wantNames: gitlabTreePageSize + 7, wantCalls: 2},
		{name: "capped at max pages", totalPages: gitlabTreeMaxPages + 5, lastPage: gitlabTreePageSize,
			wantNames: gitlabTreeMaxPages * gitlabTreePageSize, wantCalls: gitlabTreeMaxPages},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				page := r.URL.Query().Get("page")
				size := gitlabTreePageSize
				if page == fmt.Sprint(tt.totalPages) {
					size = tt.lastPage
				}
				entries := make([]gitlabTreeEntry, size)
				for i := range entries {
					entries[i] = gitlabTreeEntry{Name: fmt.Sprintf("file-%s-%d.go", page, i), Type: "blob"}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(entries)
			}))
			defer server.Close()

			names, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), "pkg")
			require.NoError(t, err)
			require.Len(t, names, tt.wantNames, "entry count")
			require.Equal(t, tt.wantCalls, int(calls.Load()), "request count")
		})
	}
}

func TestGitLabExecutor_ListDirectory_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Tree Not Found"}`))
	}))
	defer server.Close()

	_, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), "nope")
	require.ErrorContains(t, err, "HTTP 404")
}

func TestGitLabExecutor_WriteFileAndExec_AreUnsupported(t *testing.T) {
	e := NewGitLabExecutor("token", "acme/repo", "main")

	t.Run("write file", func(t *testing.T) {
		err := e.WriteFile(context.Background(), "file.txt", "content")
		require.ErrorContains(t, err, "write operations not supported")
	})
	t.Run("exec", func(t *testing.T) {
		_, err := e.Exec(context.Background(), "ls", time.Second)
		require.ErrorContains(t, err, "command execution not supported")
	})
}

func TestGitLabExecutor_ResolvePath_ReturnsInputUnchanged(t *testing.T) {
	e := NewGitLabExecutor("token", "acme/repo", "main")

	got, err := e.ResolvePath("some/path")
	require.NoError(t, err)
	require.Equal(t, "some/path", got)
}

func TestGitLabExecutor_Capabilities_ReadOnlyWithNetwork(t *testing.T) {
	caps := NewGitLabExecutor("token", "acme/repo", "main").Capabilities()

	require.True(t, caps.CanRead, "capabilities = %+v", caps)
	require.False(t, caps.CanWrite, "capabilities = %+v", caps)
	require.False(t, caps.CanExec, "capabilities = %+v", caps)
	require.True(t, caps.CanNetwork, "capabilities = %+v", caps)
}

// Verify that GitLabExecutor satisfies the Executor interface.
var _ Executor = (*GitLabExecutor)(nil)
