package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

			if err := tt.call(context.Background(), e); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
			for key, want := range tt.wantQuery {
				if got := gotQuery.Get(key); got != want {
					t.Errorf("query %q = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.absent {
				if _, ok := gotQuery[key]; ok {
					t.Errorf("query %q should be absent, got %q", key, gotQuery.Get(key))
				}
			}
		})
	}
}

func TestGitLabExecutor_ReadFile(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	tests := []struct {
		name    string
		status  int
		body    string
		path    string
		want    string
		wantErr string
	}{
		{name: "success", status: http.StatusOK, body: content, path: "main.go", want: content},
		{name: "not found", status: http.StatusNotFound, body: `{"message":"404 File Not Found"}`, path: "nope.go", wantErr: "HTTP 404"},
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
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("content = %q, want %q", got, tt.want)
			}
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
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer server.Close()

	names, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"docs/", "go.mod", "vendored"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("entry %d = %q, want %q", i, name, want[i])
		}
	}
}

func TestGitLabExecutor_ListDirectory_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	names, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), "empty-dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected no entries, got %v", names)
	}
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
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != tt.wantNames {
				t.Errorf("names = %d entries, want %d", len(names), tt.wantNames)
			}
			if got := int(calls.Load()); got != tt.wantCalls {
				t.Errorf("requests = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestGitLabExecutor_ListDirectory_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Tree Not Found"}`))
	}))
	defer server.Close()

	_, err := newTestGitLabExecutor(server.URL).ListDirectory(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error = %q, want it to mention HTTP 404", err.Error())
	}
}

func TestGitLabExecutor_UnsupportedOperations(t *testing.T) {
	e := NewGitLabExecutor("token", "acme/repo", "main")

	if err := e.WriteFile(context.Background(), "file.txt", "content"); err == nil ||
		!strings.Contains(err.Error(), "write operations not supported") {
		t.Errorf("WriteFile error = %v, want write operations not supported", err)
	}
	if _, err := e.Exec(context.Background(), "ls", time.Second); err == nil ||
		!strings.Contains(err.Error(), "command execution not supported") {
		t.Errorf("Exec error = %v, want command execution not supported", err)
	}
}

func TestGitLabExecutor_ResolvePathAndCapabilities(t *testing.T) {
	e := NewGitLabExecutor("token", "acme/repo", "main")

	got, err := e.ResolvePath("some/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "some/path" {
		t.Errorf("ResolvePath = %q, want %q", got, "some/path")
	}

	caps := e.Capabilities()
	if !caps.CanRead || caps.CanWrite || caps.CanExec || !caps.CanNetwork {
		t.Errorf("capabilities = %+v, want read-only with network", caps)
	}
}

// Verify that GitLabExecutor satisfies the Executor interface.
var _ Executor = (*GitLabExecutor)(nil)
