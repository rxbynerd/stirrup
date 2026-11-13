//go:build integration_k8s

package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sRuntimeDiagnostics_NitroIsolatedFailsOnNonNitroPod runs the
// nitro-isolated runtime diagnostics over the real pods/exec path of a Pod
// that is not a Nitro Sandbox. The guest files must parse, and the DMI
// check must fail closed. Gated on STIRRUP_TEST_KUBECONFIG like the rest of
// this suite; STIRRUP_TEST_RUNTIME_CLASS must not select a Nitro class.
func TestK8sRuntimeDiagnostics_NitroIsolatedFailsOnNonNitroPod(t *testing.T) {
	must := require.New(t)
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      cfg.image,
		Namespace:  cfg.namespace,
		Kubeconfig: cfg.kubeconfig,
		Runtime:    cfg.runtimeClass,
		Network:    &types.NetworkConfig{Mode: "none"},
	})
	must.NoError(err, "NewK8sExecutor")
	t.Cleanup(func() { _ = exec.Close() })

	security := &recordingSecurityEmitter{}
	exec.Security = security
	err = runK8sRuntimeDiagnostics(ctx, "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], &exec.podExecCore)
	must.ErrorContains(err, "dmi_product_name: ")

	must.Len(security.runtimeDiagnostics, 1)
	event := security.runtimeDiagnostics[0]
	t.Logf("reason=%q observed=%v", event.reason, event.observed)
	must.False(event.passed)
	must.Equal("dmi_product_name", event.check)
	must.Contains(event.observed["interfaces"], "lo", "/proc/net/dev parsed from the real guest")
	must.Contains(event.observed, "routeEntries", "/proc/net/route parsed from the real guest")
}
