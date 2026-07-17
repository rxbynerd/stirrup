package sandboxidentity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestComposeEnv_GitCredentialHelper_StubHaybale is the issue #516
// "stub-haybale" acceptance test: it drives a REAL git binary against a
// stub HTTP server standing in for haybale, using ONLY the env
// ComposeEnv produces, and asserts the Basic-auth password git presents
// equals the sandbox identity token. This is feasible without a real
// sandbox because the GIT_CONFIG_* env vars are honoured by the git
// process itself, independent of whether it runs on the host or inside a
// container/Pod — the executor plumb-through (container_test.go /
// factory_sandboxidentity_test.go) already proves the env reaches the
// sandbox; this test proves git actually uses it the way the issue's
// acceptance criteria describe.
//
// No live E2E and no external network: the stub server binds 127.0.0.1
// only, and the "github.com" host is never contacted — the insteadOf
// rewrite is exactly what prevents that.
func TestComposeEnv_GitCredentialHelper_StubHaybale(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found in PATH")
	}

	const wantToken = "test-jwt-sandbox-identity-token"

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
		URL:   server.URL,
		Hosts: []string{"github.com"},
	}
	env := ComposeEnv("HAYBALE_TOKEN", wantToken, gp)

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
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "https://github.com/rxbynerd/dressage.git")
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
		t.Errorf("Basic-auth password = %q, want the sandbox identity token %q", capturedPass, wantToken)
	}
}
