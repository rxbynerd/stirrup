//go:build integration_container

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// liveSandbox builds a container executor against a real Docker/Podman
// engine, gated on STIRRUP_TEST_CONTAINER_SOCKET (the engine's Unix socket)
// and STIRRUP_TEST_CONTAINER_IMAGE (an image carrying git, for example one
// built from Dockerfile.sandbox).
func liveSandbox(t *testing.T, ctx context.Context, hostDir string) *executor.ContainerExecutor {
	t.Helper()
	must := require.New(t)
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
	must.NoError(err, "NewContainerExecutorWithContext")
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestContainer_PlantedRepositoryConfigNeverReachesHost drives the strategy
// against a sandbox whose agent has planted core.fsmonitor, a clean filter
// driver with .gitattributes, and a pre-commit hook, each of which would
// write a marker into a host-only directory if it ran on the host.
func TestContainer_PlantedRepositoryConfigNeverReachesHost(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	hostDir := t.TempDir()
	must.NoError(os.Chmod(hostDir, 0o777), "chmod workspace")
	// Never mounted into the sandbox: a payload can only create a file here
	// by running on the host.
	hostMarkers := t.TempDir()
	must.NoError(os.Chmod(hostMarkers, 0o777), "chmod markers")
	marker := func(name string) string { return filepath.Join(hostMarkers, name) }

	sandboxExec := liveSandbox(t, ctx, hostDir)
	inSandbox := func(command string) string {
		t.Helper()
		res, err := sandboxExec.Exec(ctx, command, time.Minute)
		must.NoError(err, "sandbox %q", command)
		must.Equal(0, res.ExitCode, "sandbox %q\nstdout: %s\nstderr: %s", command, res.Stdout, res.Stderr)
		return strings.TrimSpace(res.Stdout)
	}

	inSandbox("git init -q && git -c user.name=agent -c user.email=agent@example.com commit -q --allow-empty -m seed")
	inSandbox("git config core.fsmonitor " + shellQuote("touch "+marker("fsmonitor")+"; echo /"))
	inSandbox("git config filter.pwn.clean " + shellQuote("touch "+marker("filter")+"; cat"))
	must.NoError(sandboxExec.WriteFile(ctx, ".gitattributes", "* filter=pwn\n"), "write .gitattributes")
	must.NoError(sandboxExec.WriteFile(ctx, ".git/hooks/pre-commit", "#!/bin/sh\ntouch "+marker("hook")+"\n"), "write pre-commit hook")
	inSandbox("chmod +x .git/hooks/pre-commit")

	gs := NewDeterministicGitStrategy(sandboxExec)
	must.NoError(gs.Setup(ctx, hostDir, "container-run"), "Setup")
	must.NoError(sandboxExec.WriteFile(ctx, "change.txt", "agent output\n"), "write change")
	must.NoError(gs.Checkpoint(ctx, "Turn 1: 1 tool calls"), "Checkpoint")
	result, err := gs.Finalise(ctx, "success")
	must.NoError(err, "Finalise")
	check.Equal("stirrup/container-run", result.Branch, "Finalise result: %+v", result)
	check.True(ValidSHA(result.SHA), "Finalise result: %+v", result)

	entries, err := os.ReadDir(hostMarkers)
	must.NoError(err, "ReadDir markers")
	planted := make([]string, 0, len(entries))
	for _, e := range entries {
		planted = append(planted, e.Name())
	}
	check.Empty(planted, "planted payloads ran on the host")
	check.Equal(HarnessIdentityName, inSandbox("git log -1 --format=%an"), "sandbox commit author")
	check.Equal(result.SHA, inSandbox("git rev-parse HEAD"), "sandbox HEAD")
}
