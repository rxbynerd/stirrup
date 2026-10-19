//go:build integration_container

package prompt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// TestContainer_ComposedPromptReadsSandbox renders the composed prompt
// against a real container whose repository carries a planted
// core.fsmonitor that would create a marker in a host-only directory if
// it ran on the host. It needs STIRRUP_TEST_CONTAINER_SOCKET (the engine's
// Unix socket) and STIRRUP_TEST_CONTAINER_IMAGE (an image containing git).
func TestContainer_ComposedPromptReadsSandbox(t *testing.T) {
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_IMAGE not set; it must name an image containing git")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	hostDir := t.TempDir()
	require.NoError(t, os.Chmod(hostDir, 0o777), "chmod workspace")
	hostMarkers := t.TempDir()
	require.NoError(t, os.Chmod(hostMarkers, 0o777), "chmod markers")
	marker := filepath.Join(hostMarkers, "fsmonitor-ran")

	sandbox, err := executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
		Image:             image,
		HostDir:           hostDir,
		SocketPath:        sock,
		Network:           &types.NetworkConfig{Mode: "none"},
		RegistryAllowlist: []string{"localhost/*", "docker.io/library/*", "ghcr.io/rxbynerd/*"},
	})
	require.NoError(t, err, "NewContainerExecutorWithContext")
	t.Cleanup(func() { _ = sandbox.Close() })

	seed := "git init -q && git -c user.name=agent -c user.email=agent@example.com commit -q --allow-empty -m seed" +
		" && git config core.fsmonitor 'touch " + marker + "; echo /'" +
		" && mkdir pkg && echo 'package pkg' > pkg/pkg.go && echo hi > notes.txt"
	res, err := sandbox.Exec(ctx, seed, time.Minute)
	require.NoError(t, err, "seed sandbox")
	require.Zero(t, res.ExitCode, "seed sandbox result %+v", res)

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(ctx, PromptContext{Mode: "execution", Executor: sandbox})
	require.NoError(t, err, "Build()")

	_, statErr := os.Stat(marker)
	require.Error(t, statErr, "the planted fsmonitor payload ran on the host")

	tests := []struct {
		name string
		want string
	}{
		{name: "workspace listing", want: "Workspace files:\n  .git/\n  notes.txt\n  pkg/"},
		{name: "git status", want: "Git status:\n?? notes.txt\n?? pkg/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, got, tc.want)
		})
	}
}
