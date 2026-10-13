//go:build integration_container

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// liveSandbox builds a container executor against a real Docker/Podman
// engine, gated on STIRRUP_TEST_CONTAINER_SOCKET (the engine's Unix socket)
// and STIRRUP_TEST_CONTAINER_IMAGE (an image carrying git, for example one
// built from Dockerfile.sandbox).
func liveSandbox(t *testing.T, ctx context.Context, hostDir string) *executor.ContainerExecutor {
	t.Helper()
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_IMAGE not set; it must name an image containing git")
	}
	exec, err := executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
		Image:             image,
		HostDir:           hostDir,
		SocketPath:        sock,
		Network:           &types.NetworkConfig{Mode: "none"},
		RegistryAllowlist: []string{"localhost/*", "docker.io/library/*", "ghcr.io/rxbynerd/*"},
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestContainer_PlantedRepositoryConfigNeverReachesHost drives the strategy
// against a sandbox whose agent has planted core.fsmonitor, a clean filter
// driver with .gitattributes, and a pre-commit hook, each of which would
// write a marker into a host-only directory if it ran on the host.
func TestContainer_PlantedRepositoryConfigNeverReachesHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	hostDir := t.TempDir()
	if err := os.Chmod(hostDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	// Never mounted into the sandbox: a payload can only create a file here
	// by running on the host.
	hostMarkers := t.TempDir()
	if err := os.Chmod(hostMarkers, 0o777); err != nil {
		t.Fatalf("chmod markers: %v", err)
	}
	marker := func(name string) string { return filepath.Join(hostMarkers, name) }

	sandboxExec := liveSandbox(t, ctx, hostDir)
	inSandbox := func(command string) string {
		t.Helper()
		res, err := sandboxExec.Exec(ctx, command, time.Minute)
		if err != nil {
			t.Fatalf("sandbox %q: %v", command, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("sandbox %q: exit %d\nstdout: %s\nstderr: %s", command, res.ExitCode, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	inSandbox("git init -q && git -c user.name=agent -c user.email=agent@example.com commit -q --allow-empty -m seed")
	inSandbox("git config core.fsmonitor " + shellQuote("touch "+marker("fsmonitor")+"; echo /"))
	inSandbox("git config filter.pwn.clean " + shellQuote("touch "+marker("filter")+"; cat"))
	if err := sandboxExec.WriteFile(ctx, ".gitattributes", "* filter=pwn\n"); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := sandboxExec.WriteFile(ctx, ".git/hooks/pre-commit", "#!/bin/sh\ntouch "+marker("hook")+"\n"); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	inSandbox("chmod +x .git/hooks/pre-commit")

	gs := NewDeterministicGitStrategy(sandboxExec)
	if err := gs.Setup(ctx, hostDir, "container-run"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := sandboxExec.WriteFile(ctx, "change.txt", "agent output\n"); err != nil {
		t.Fatalf("write change: %v", err)
	}
	if err := gs.Checkpoint(ctx, "Turn 1: 1 tool calls"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if result.Branch != "stirrup/container-run" || !ValidSHA(result.SHA) {
		t.Errorf("Finalise = %+v", result)
	}

	entries, err := os.ReadDir(hostMarkers)
	if err != nil {
		t.Fatalf("ReadDir markers: %v", err)
	}
	for _, e := range entries {
		t.Errorf("planted %s payload ran on the host", e.Name())
	}
	if got := inSandbox("git log -1 --format=%an"); got != HarnessIdentityName {
		t.Errorf("sandbox commit author = %q, want %q", got, HarnessIdentityName)
	}
	if got := inSandbox("git rev-parse HEAD"); got != result.SHA {
		t.Errorf("sandbox HEAD = %q, Finalise reported %q", got, result.SHA)
	}
}
