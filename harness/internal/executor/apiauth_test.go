package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIExecutorBackends_AuthHeaders pins the per-forge auth header of both
// VCS backends, including the unauthenticated case: public repositories are
// readable without a token, so an empty token must send no credential header
// rather than an empty one.
func TestAPIExecutorBackends_AuthHeaders(t *testing.T) {
	backends := []struct {
		name       string
		header     string
		wantHeader string
		newFor     func(baseURL, token string) Executor
	}{
		{
			name:       "github",
			header:     "Authorization",
			wantHeader: "Bearer test-token",
			newFor: func(baseURL, token string) Executor {
				e := NewAPIExecutor(token, "octocat", "hello-world", "main")
				e.baseURL = baseURL
				return e
			},
		},
		{
			name:       "gitlab",
			header:     "PRIVATE-TOKEN",
			wantHeader: "test-token",
			newFor: func(baseURL, token string) Executor {
				e := NewGitLabExecutor(token, "acme/hello-world", "main")
				e.baseURL = baseURL
				return e
			},
		},
	}

	operations := []struct {
		name string
		call func(context.Context, Executor) error
	}{
		{
			name: "ReadFile",
			call: func(ctx context.Context, e Executor) error {
				_, err := e.ReadFile(ctx, "README.md")
				return err
			},
		},
		{
			name: "ListDirectory",
			call: func(ctx context.Context, e Executor) error {
				_, err := e.ListDirectory(ctx, ".")
				return err
			},
		},
	}

	for _, backend := range backends {
		for _, op := range operations {
			for _, tokenCase := range []struct {
				name  string
				token string
			}{{name: "with token", token: "test-token"}, {name: "unauthenticated", token: ""}} {
				t.Run(backend.name+"/"+op.name+"/"+tokenCase.name, func(t *testing.T) {
					want := backend.wantHeader
					if tokenCase.token == "" {
						want = ""
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if got := r.Header.Get(backend.header); got != want {
							t.Errorf("%s = %q, want %q", backend.header, got, want)
						}
						if _, ok := r.Header[http.CanonicalHeaderKey(backend.header)]; ok && want == "" {
							t.Errorf("%s should be absent for an empty token", backend.header)
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte("[]"))
					}))
					defer server.Close()

					if err := op.call(context.Background(), backend.newFor(server.URL, tokenCase.token)); err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
				})
			}
		}
	}
}
