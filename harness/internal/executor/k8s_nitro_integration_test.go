//go:build integration_k8s

package executor

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sNitroIsolated_EndToEnd runs the nitro-isolated runtime on a cluster
// that offers AWS Nitro Sandboxes (EKS Auto Mode with the isolated-sandbox
// RuntimeClass). It is gated on STIRRUP_TEST_NITRO_ISOLATED as well as
// STIRRUP_TEST_KUBECONFIG, because on any other cluster the Pod is rejected
// or fails its diagnostics by design.
func TestK8sNitroIsolated_EndToEnd(t *testing.T) {
	cfg := testK8sEnv(t)
	if os.Getenv("STIRRUP_TEST_NITRO_ISOLATED") == "" {
		t.Skip("STIRRUP_TEST_NITRO_ISOLATED not set; this group needs an EKS cluster with Nitro Sandboxes")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()

	security := &recordingSecurityEmitter{}
	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:        cfg.image,
		Namespace:    cfg.namespace,
		Kubeconfig:   cfg.kubeconfig,
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
		Runtime:      "nitro-isolated",
		Resources:    &types.ResourceLimits{CPUs: 1, MemoryMB: 1024},
		Network:      &types.NetworkConfig{Mode: "none"},
		Security:     security,
	})
	must.NoError(err, "NewK8sExecutor: the admission check or the diagnostics rejected the Pod")
	t.Cleanup(func() { _ = exec.Close() })

	t.Run("diagnostics passed", func(t *testing.T) {
		must := require.New(t)
		must.Len(security.runtimeDiagnostics, 1)
		event := security.runtimeDiagnostics[0]
		t.Logf("observed=%v", event.observed)
		must.True(event.passed)
		must.True(strings.HasSuffix(event.observed["dmiProductName"], ".enclave"))
		must.Equal("lo,dummy0", event.observed["interfaces"])
		must.Equal("0", event.observed["routeEntries"])
	})

	t.Run("pod shape accepted", func(t *testing.T) {
		must := require.New(t)
		clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
		must.NoError(err)
		pod, err := clientset.CoreV1().Pods(cfg.namespace).Get(ctx, exec.podName, metav1.GetOptions{})
		must.NoError(err)
		must.NotNil(pod.Spec.RuntimeClassName)
		must.Equal("isolated-sandbox", *pod.Spec.RuntimeClassName)
		must.Equal("true", pod.Annotations["sandbox.amazonaws.com/network-isolated"])
		t.Logf("admitted annotations=%v nodeSelector=%v overhead=%v", pod.Annotations, pod.Spec.NodeSelector, pod.Spec.Overhead)
	})

	t.Run("no guest network", func(t *testing.T) {
		must := require.New(t)
		res, err := exec.Exec(ctx, "command -v wget >/dev/null 2>&1 || { echo no-wget; exit 0; }; wget -q -T 5 -O /dev/null http://1.1.1.1/ && echo reached || echo unreachable", 0)
		must.NoError(err)
		out := strings.TrimSpace(res.Stdout)
		if out == "no-wget" {
			t.Skip("the image has no wget; the interface and route diagnostics are the only network evidence")
		}
		must.Equal("unreachable", out, "stderr %q", res.Stderr)
	})

	t.Run("workspace capacity recorded", func(t *testing.T) {
		res, err := exec.Exec(ctx, "df -k /workspace 2>&1 || echo df-unavailable", 0)
		require.New(t).NoError(err)
		t.Logf("workspace capacity with no ephemeral-storage declared:\n%s", res.Stdout)
	})
}
