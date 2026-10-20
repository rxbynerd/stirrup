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
	must := require.New(t)
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
	must.NoError(err, "NewK8sExecutor")
	t.Cleanup(func() { _ = exec.Close() })

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
	)
	for i, token := range []string{first, second} {
		must.NoError(exec.WriteSandboxIdentityToken(ctx, token), "WriteSandboxIdentityToken #%d", i+1)
		res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
		must.NoError(err, "cat token #%d", i+1)
		must.Equal(token, res.Stdout, "token file after delivery #%d (stderr %q)", i+1, res.Stderr)
	}

	res, err := exec.Exec(ctx, "stat -c '%a %u' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	must.NoError(err, "stat token")
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	must.GreaterOrEqual(len(lines), 2, "stat output %q", res.Stdout)
	must.Equal("600 65532", lines[0], "token file mode and uid (output %q)", res.Stdout)
	must.NotContains(res.Stdout, ".token.tmp", "staging file left behind after rename")

	_, err = exec.ReadFile(ctx, SandboxIdentityTokenPath)
	must.Error(err, "ReadFile reached the token through the workspace-scoped file API")

	// A symlink committed into the workspace must not reach the token
	// either, whether it names the file or a directory above it.
	setup := "ln -sf " + SandboxIdentityTokenPath + " /workspace/link && ln -sf " + SandboxIdentityTokenDir + " /workspace/dirlink"
	res, err = exec.Exec(ctx, setup, 30*time.Second)
	must.NoError(err, "symlink setup")
	must.Zero(res.ExitCode, "symlink setup result %+v", res)

	for _, tc := range []struct {
		name  string
		probe string
	}{
		{name: "file symlink", probe: "link"},
		{name: "directory symlink", probe: "dirlink/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			content, err := exec.ReadFile(ctx, tc.probe)
			must.ErrorContains(err, "escapes workspace", "ReadFile(%s) returned content %q", tc.probe, content)
			must.NotContains(content, second, "ReadFile(%s) disclosed the token", tc.probe)
		})
	}
}
