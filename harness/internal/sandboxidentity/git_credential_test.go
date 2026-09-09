package sandboxidentity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestComposeEnv_GitCredentialHelper_StubHaybale drives a REAL git binary
// against a stub HTTP server standing in for haybale, using ONLY the env
// ComposeEnv produces plus the token file it points at, and asserts the
// Basic-auth password git presents equals the token in that file — the
// current file content, not the value the env var was composed with, which
// is what lets a refreshed token take effect without recreating the
// sandbox. This is feasible without a real sandbox because the GIT_CONFIG_*
// env vars are honoured by the git process itself, independent of whether
// it runs on the host or inside a container/Pod; the executor tests prove
// the env and the token file reach the sandbox.
//
// No live E2E and no external network: the stub server binds 127.0.0.1
// only, and the "github.com" host is never contacted — the insteadOf
// rewrite is exactly what prevents that.
//
// Both the "https://" form and the "git@host:"/"ssh://" insteadOf
// rewriting must route through the proxy; the "ssh scp-form" subtest pins
// the latter through a real git binary rather than by string match alone.
func TestComposeEnv_GitCredentialHelper_StubHaybale(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found in PATH")
	}

	const (
		composedToken = "stale-token-from-sandbox-creation"
		wantToken     = "test-jwt-sandbox-identity-token"
	)

	cases := []struct {
		name       string
		remote     string
		rewriteSsh bool
	}{
		{name: "https", remote: "https://github.com/rxbynerd/dressage.git", rewriteSsh: false},
		{name: "ssh scp-form", remote: "git@github.com:rxbynerd/dressage.git", rewriteSsh: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu           sync.Mutex
				sawNoAuth    bool
				capturedUser string
				capturedPass string
				sawAuth      bool
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				mu.Lock()
				if !ok {
					sawNoAuth = true
				} else {
					sawAuth = true
					capturedUser = user
					capturedPass = pass
				}
				mu.Unlock()

				if !ok {
					// Prompt git to retry with the credential helper's output, the
					// same way haybale (or any git smart-HTTP server) would.
					w.Header().Set("WWW-Authenticate", `Basic realm="haybale-stub"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				// The stub does not need to speak the full smart-HTTP protocol —
				// once the credential has been captured, failing the request is
				// sufficient; the test only asserts what was presented, not that
				// the clone succeeded.
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			gp := &types.GitProxyConfig{
				URL:        server.URL,
				Hosts:      []string{"github.com"},
				RewriteSsh: tc.rewriteSsh,
			}
			// t.TempDir() is under /var/folders on macOS, whose characters
			// tokenPathPattern accepts; the file stands in for the
			// executor-delivered SandboxIdentityTokenPath.
			tokenPath := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenPath, []byte(wantToken), 0o600); err != nil {
				t.Fatalf("write token file: %v", err)
			}
			env, err := ComposeEnv("HAYBALE_TOKEN", composedToken, tokenPath, gp)
			if err != nil {
				t.Fatalf("ComposeEnv() error: %v", err)
			}

			path, ok := os.LookupEnv("PATH")
			if !ok {
				t.Fatal("PATH is not set in the test process environment")
			}
			envStrings := []string{
				"PATH=" + path,
				"HOME=" + t.TempDir(),
				// Isolate from the host's real git config (a developer machine's
				// global credential.helper or insteadOf rules would otherwise
				// interfere with — or mask a regression in — the composed config
				// this test exists to verify).
				"GIT_CONFIG_GLOBAL=/dev/null",
				"GIT_CONFIG_SYSTEM=/dev/null",
				"GIT_TERMINAL_PROMPT=0",
			}
			for _, e := range env {
				envStrings = append(envStrings, e.Name+"="+e.Value)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// ls-remote performs a single info/refs discovery request — everything
			// the credential helper / insteadOf rewrite needs to be exercised,
			// without the overhead (or on-disk side effects) of a full clone.
			cmd := exec.CommandContext(ctx, "git", "ls-remote", tc.remote)
			cmd.Env = envStrings
			// Intentionally ignore the error: the stub always fails the
			// authenticated request (by design, see above), so git is expected to
			// exit non-zero. What matters is what it presented on the wire.
			_ = cmd.Run()

			mu.Lock()
			defer mu.Unlock()

			if !sawNoAuth {
				t.Error("expected an initial unauthenticated request (git should probe before invoking the credential helper)")
			}
			if !sawAuth {
				t.Fatal("expected a follow-up request carrying Basic auth from the composed credential helper")
			}
			if capturedUser != "x-access-token" {
				t.Errorf("Basic-auth username = %q, want %q", capturedUser, "x-access-token")
			}
			if capturedPass != wantToken {
				t.Errorf("Basic-auth password = %q, want the token file's content %q", capturedPass, wantToken)
			}
			if capturedPass == composedToken {
				t.Error("git presented the env var's token rather than reading the token file")
			}
		})
	}
}
