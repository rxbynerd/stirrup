package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			name: "read file",
			call: func(ctx context.Context, e Executor) error {
				_, err := e.ReadFile(ctx, "README.md")
				return err
			},
		},
		{
			name: "list directory",
			call: func(ctx context.Context, e Executor) error {
				_, err := e.ListDirectory(ctx, ".")
				return err
			},
		},
	}

	tokenCases := []struct {
		name  string
		token string
	}{
		{name: "with token", token: "test-token"},
		{name: "unauthenticated", token: ""},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			for _, op := range operations {
				t.Run(op.name, func(t *testing.T) {
					for _, tokenCase := range tokenCases {
						t.Run(tokenCase.name, func(t *testing.T) {
							want := backend.wantHeader
							if tokenCase.token == "" {
								want = ""
							}
							// The handler runs on the server's goroutine, so it must use assert, not require.
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								assert.Equal(t, want, r.Header.Get(backend.header), "header %s", backend.header)
								if want == "" {
									_, present := r.Header[http.CanonicalHeaderKey(backend.header)]
									assert.False(t, present, "header %s should be absent for an empty token", backend.header)
								}
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write([]byte("[]"))
							}))
							defer server.Close()

							e := backend.newFor(server.URL, tokenCase.token)
							require.NoError(t, op.call(context.Background(), e))
						})
					}
				})
			}
		})
	}
}
