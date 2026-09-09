//go:build integration_k8s

package executor

import (
	"context"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

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

	res, err := exec.Exec(ctx, "stat -c '%a %u' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 || lines[0] != "600 65532" {
		t.Errorf("token file mode/uid = %q, want \"600 65532\" (output %q)", lines, res.Stdout)
	}
	if strings.Contains(res.Stdout, ".token.tmp") {
		t.Errorf("staging file left behind after rename: %q", res.Stdout)
	}

	if _, err := exec.ReadFile(ctx, SandboxIdentityTokenPath); err == nil {
		t.Error("ReadFile reached the token through the workspace-scoped file API")
	}
}
