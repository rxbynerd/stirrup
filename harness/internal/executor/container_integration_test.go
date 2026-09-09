package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// liveContainerExecutor builds a hardened container against a real
// Docker/Podman engine, gated on STIRRUP_TEST_CONTAINER_SOCKET (the
// engine's Unix socket) so the default `just test` run never needs an
// engine; STIRRUP_TEST_CONTAINER_IMAGE (default busybox:latest) must
// already be present locally.
func liveContainerExecutor(t *testing.T, ctx context.Context, sandboxIdentity bool) *ContainerExecutor {
	t.Helper()
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}

	exec, err := NewContainerExecutorWithContext(ctx, ContainerExecutorConfig{
		Image:                image,
		HostDir:              t.TempDir(),
		SocketPath:           sock,
		Network:              &types.NetworkConfig{Mode: "none"},
		SandboxIdentityToken: sandboxIdentity,
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestContainerExecutor_SandboxIdentityToken_LiveEngine drives the token
// delivery path against a real engine: a hardened container (read-only
// rootfs, unprivileged uid) receives a token over the hijacked exec's
// stdin, the file lands at SandboxIdentityTokenPath with mode 0600, a
// second delivery replaces it in place, an aborted delivery leaves the
// previous token intact, and the workspace-scoped file API cannot reach
// the file.
func TestContainerExecutor_SandboxIdentityToken_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
		third  = "eyJhbGciOiJFUzI1NiJ9.third-token-that-never-fully-arrives.sig"
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

	// A client that dies mid-stream must not replace the valid token with
	// a partial one.
	aborted := io.MultiReader(strings.NewReader(third[:12]), iotest.ErrReader(errors.New("client aborted")))
	result, err := exec.execInContainerInput(ctx, sandboxIdentityWriteCommand(len(third)), exec.workspace, aborted, containerFileIOTimeout)
	if err == nil && (result == nil || result.ExitCode == 0) {
		t.Fatalf("aborted delivery reported success (result %+v)", result)
	}
	res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
	if err != nil {
		t.Fatalf("cat token after aborted delivery: %v", err)
	}
	if res.Stdout != second {
		t.Errorf("token file after an aborted delivery = %q, want the previous token %q", res.Stdout, second)
	}

	res, err = exec.Exec(ctx, "stat -c '%a %U' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 || lines[0] != "600 nobody" {
		t.Errorf("token file mode/owner = %q, want \"600 nobody\" (output %q)", lines, res.Stdout)
	}
	if strings.Contains(res.Stdout, ".token.tmp") {
		t.Errorf("staging file left behind: %q", res.Stdout)
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

// TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine proves the read
// guard closes the symlink class against a real engine: a workspace
// symlink to the token, a symlink to a file outside the workspace, and a
// symlinked directory all fail with a workspace-escape error and return
// no content, while a symlink that stays inside the workspace still reads.
func TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const token = "SECRET-TOKEN-VALUE-eyJhbGciOiJFUzI1NiJ9"
	if err := exec.WriteSandboxIdentityToken(ctx, token); err != nil {
		t.Fatalf("WriteSandboxIdentityToken: %v", err)
	}
	setup := strings.Join([]string{
		"printf inside > /workspace/real.txt",
		"ln -sf real.txt /workspace/alias",
		"ln -sf " + SandboxIdentityTokenPath + " /workspace/link",
		"ln -sf /etc/hostname /workspace/hostlink",
		"ln -sf " + SandboxIdentityTokenDir + " /workspace/dirlink",
	}, " && ")
	if res, err := exec.Exec(ctx, setup, 30*time.Second); err != nil || res.ExitCode != 0 {
		t.Fatalf("symlink setup failed: err=%v result=%+v", err, res)
	}

	hostname, err := exec.Exec(ctx, "cat /etc/hostname", 30*time.Second)
	if err != nil {
		t.Fatalf("read hostname: %v", err)
	}

	for _, probe := range []string{"link", "hostlink", "dirlink/token"} {
		emitter := &mockSecurityEmitter{}
		exec.Security = emitter
		content, err := exec.ReadFile(ctx, probe)
		if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
			t.Errorf("ReadFile(%s) = (%q, %v), want a workspace-escape error", probe, content, err)
		}
		if strings.Contains(content, token) || (hostname.Stdout != "" && strings.Contains(content, strings.TrimSpace(hostname.Stdout))) {
			t.Errorf("ReadFile(%s) disclosed out-of-workspace content: %q", probe, content)
		}
		if emitter.pathTraversalCount != 1 {
			t.Errorf("ReadFile(%s) emitted PathTraversalBlocked %d times, want 1", probe, emitter.pathTraversalCount)
		}
	}
	exec.Security = nil

	got, err := exec.ReadFile(ctx, "alias")
	if err != nil || got != "inside" {
		t.Errorf("ReadFile(alias) = (%q, %v), want an in-workspace symlink to read its target", got, err)
	}
}
