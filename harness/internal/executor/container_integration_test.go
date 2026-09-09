package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestContainerExecutor_SandboxIdentityToken_LiveEngine drives the token
// delivery path against a real Docker/Podman engine: a hardened container
// (read-only rootfs, unprivileged uid) receives a token over the hijacked
// exec's stdin, the file lands at SandboxIdentityTokenPath with mode 0600,
// a second delivery replaces it in place, and the workspace-scoped file
// API cannot reach it. Gated on STIRRUP_TEST_CONTAINER_SOCKET (the engine's
// Unix socket) so the default `just test` run never needs an engine;
// STIRRUP_TEST_CONTAINER_IMAGE (default busybox:latest) must already be
// present locally.
func TestContainerExecutor_SandboxIdentityToken_LiveEngine(t *testing.T) {
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	exec, err := NewContainerExecutorWithContext(ctx, ContainerExecutorConfig{
		Image:                image,
		HostDir:              t.TempDir(),
		SocketPath:           sock,
		Network:              &types.NetworkConfig{Mode: "none"},
		SandboxIdentityToken: true,
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	defer func() { _ = exec.Close() }()

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
	)
	for i, token := range []string{first, second} {
		if err := exec.WriteSandboxIdentityToken(ctx, token); err != nil {
			t.Fatalf("WriteSandboxIdentityToken #%d: %v", i+1, err)
		}
		res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
		if err != nil {
			t.Fatalf("cat token #%d: %v", i+1, err)
		}
		if res.Stdout != token {
			t.Errorf("token file after delivery #%d = %q, want %q (stderr %q)", i+1, res.Stdout, token, res.Stderr)
		}
	}

	res, err := exec.Exec(ctx, "stat -c '%a %U' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 || lines[0] != "600 nobody" {
		t.Errorf("token file mode/owner = %q, want \"600 nobody\" (output %q)", lines, res.Stdout)
	}
	if strings.Contains(res.Stdout, ".token.tmp") {
		t.Errorf("staging file left behind after rename: %q", res.Stdout)
	}

	if _, err := exec.ReadFile(ctx, SandboxIdentityTokenPath); err == nil {
		t.Error("ReadFile reached the token through the workspace-scoped file API")
	}
	entries, err := exec.ListDirectory(ctx, ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e, "token") {
			t.Errorf("workspace listing contains a token artefact: %q", entries)
		}
	}
}
