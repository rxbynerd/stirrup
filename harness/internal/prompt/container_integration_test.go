//go:build integration_container

package prompt

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
	if err := os.Chmod(hostDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	hostMarkers := t.TempDir()
	if err := os.Chmod(hostMarkers, 0o777); err != nil {
		t.Fatalf("chmod markers: %v", err)
	}
	marker := filepath.Join(hostMarkers, "fsmonitor-ran")

	sandbox, err := executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
		Image:             image,
		HostDir:           hostDir,
		SocketPath:        sock,
		Network:           &types.NetworkConfig{Mode: "none"},
		RegistryAllowlist: []string{"localhost/*", "docker.io/library/*", "ghcr.io/rxbynerd/*"},
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Close() })

	seed := "git init -q && git -c user.name=agent -c user.email=agent@example.com commit -q --allow-empty -m seed" +
		" && git config core.fsmonitor 'touch " + marker + "; echo /'" +
		" && mkdir pkg && echo 'package pkg' > pkg/pkg.go && echo hi > notes.txt"
	res, err := sandbox.Exec(ctx, seed, time.Minute)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed sandbox: err %v, result %+v", err, res)
	}

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(ctx, PromptContext{Mode: "execution", Executor: sandbox})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the planted fsmonitor payload ran on the host")
	}
	for _, want := range []string{"Workspace files:\n  .git/\n  notes.txt\n  pkg/", "Git status:\n?? notes.txt\n?? pkg/"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q:\n%s", want, got)
		}
	}
}
