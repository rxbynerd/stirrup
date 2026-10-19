//go:build integration_k8s

package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sSandboxIdentityToken_RoundTrip drives token delivery against a
// real cluster: a Pod created with the sandbox identity volume receives a
// token over pods/exec stdin, the file lands at SandboxIdentityTokenPath
// with mode 0600 owned by the non-root UID, a second delivery replaces it
// in place, and the workspace-scoped file API cannot reach it. Gated on
// STIRRUP_TEST_KUBECONFIG like the rest of this suite.
func TestK8sSandboxIdentityToken_RoundTrip(t *testing.T) {
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:                cfg.image,
		Namespace:            cfg.namespace,
		Kubeconfig:           cfg.kubeconfig,
		RuntimeClassName:     cfg.runtimeClass,
		Network:              &types.NetworkConfig{Mode: "none"},
		SandboxIdentityToken: true,
	})
	require.NoError(t, err, "NewK8sExecutor")
	t.Cleanup(func() { _ = exec.Close() })

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
	)
	for i, token := range []string{first, second} {
		require.NoError(t, exec.WriteSandboxIdentityToken(ctx, token), "WriteSandboxIdentityToken #%d", i+1)
		res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
		require.NoError(t, err, "cat token #%d", i+1)
		require.Equal(t, token, res.Stdout, "token file after delivery #%d (stderr %q)", i+1, res.Stderr)
	}

	res, err := exec.Exec(ctx, "stat -c '%a %u' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	require.NoError(t, err, "stat token")
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "stat output %q", res.Stdout)
	require.Equal(t, "600 65532", lines[0], "token file mode and uid (output %q)", res.Stdout)
	require.NotContains(t, res.Stdout, ".token.tmp", "staging file left behind after rename")

	_, err = exec.ReadFile(ctx, SandboxIdentityTokenPath)
	require.Error(t, err, "ReadFile reached the token through the workspace-scoped file API")

	// A symlink committed into the workspace must not reach the token
	// either, whether it names the file or a directory above it.
	setup := "ln -sf " + SandboxIdentityTokenPath + " /workspace/link && ln -sf " + SandboxIdentityTokenDir + " /workspace/dirlink"
	res, err = exec.Exec(ctx, setup, 30*time.Second)
	require.NoError(t, err, "symlink setup")
	require.Zero(t, res.ExitCode, "symlink setup result %+v", res)

	for _, tc := range []struct {
		name  string
		probe string
	}{
		{name: "file symlink", probe: "link"},
		{name: "directory symlink", probe: "dirlink/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := exec.ReadFile(ctx, tc.probe)
			require.ErrorContains(t, err, "escapes workspace", "ReadFile(%s) returned content %q", tc.probe, content)
			require.NotContains(t, content, second, "ReadFile(%s) disclosed the token", tc.probe)
		})
	}
}
