//go:build integration_k8s

package executor

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sNitroIsolated_EndToEnd runs the nitro-isolated runtime on a cluster
// that offers AWS Nitro Sandboxes (EKS Auto Mode with the isolated-sandbox
// RuntimeClass). It is gated on STIRRUP_TEST_NITRO_ISOLATED as well as
// STIRRUP_TEST_KUBECONFIG, because on any other cluster the Pod is rejected
// or fails its diagnostics by design. The Pod is pinned to the tested
// instance type, m5d.xlarge. With STIRRUP_TEST_EVIDENCE_DIR set, the
// submitted Pod, the Create response, the Ready Pod and the workspace size
// are written there.
func TestK8sNitroIsolated_EndToEnd(t *testing.T) {
	cfg := testK8sEnv(t)
	if os.Getenv("STIRRUP_TEST_NITRO_ISOLATED") == "" {
		t.Skip("STIRRUP_TEST_NITRO_ISOLATED not set; this group needs an EKS cluster with Nitro Sandboxes")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()
	evidence := evidenceDirFromEnv()

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	must.NoError(err, "build verification clientset")
	owner := createStandInOwnerPod(t, ctx, clientset, cfg.namespace, cfg.image)

	security := &recordingSecurityEmitter{}
	execCfg := K8sExecutorConfig{
		Image:      cfg.image,
		Namespace:  cfg.namespace,
		Kubeconfig: cfg.kubeconfig,
		NodeSelector: map[string]string{
			"kubernetes.io/arch":               "amd64",
			"node.kubernetes.io/instance-type": "m5d.xlarge",
		},
		Runtime:    "nitro-isolated",
		Resources:  &types.ResourceLimits{CPUs: 1, MemoryMB: 1024},
		Network:    &types.NetworkConfig{Mode: "none"},
		Security:   security,
		Lifecycle:  Lifecycle{RunID: "nitro-end-to-end", MaxLifetime: k8sIntegrationTimeout},
		HarnessPod: &HarnessPod{Namespace: cfg.namespace, Name: owner.Name, UID: string(owner.UID)},
	}
	record := &podCreateRecord{}
	exec, err := newRecordedK8sExecutor(ctx, execCfg, record)
	evidence.writeCreateEvidence(t, record)
	must.NoError(err, "NewK8sExecutor: the admission check or the runtime diagnostics rejected the Pod")
	t.Cleanup(func() { _ = exec.Close() })

	ready, err := clientset.CoreV1().Pods(cfg.namespace).Get(ctx, exec.podName, metav1.GetOptions{})
	must.NoError(err)
	evidence.writePod(t, "ready-pod.json", ready)

	t.Run("runtime diagnostics passed", func(t *testing.T) {
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
		must.NotNil(ready.Spec.RuntimeClassName)
		must.Equal("isolated-sandbox", *ready.Spec.RuntimeClassName)
		must.Equal("true", ready.Annotations["sandbox.amazonaws.com/network-isolated"])
		must.Equal(ptr.To(false), ready.Spec.EnableServiceLinks)
		must.NotNil(ready.Spec.ActiveDeadlineSeconds, "activeDeadlineSeconds")
		must.Len(ready.OwnerReferences, 1)
		must.Equal(owner.UID, ready.OwnerReferences[0].UID)
		t.Logf("admitted annotations=%v nodeSelector=%v overhead=%v", ready.Annotations, ready.Spec.NodeSelector, ready.Spec.Overhead)
	})

	t.Run("exec success", func(t *testing.T) { assertExecSuccess(t, ctx, exec) })
	t.Run("exec non-zero exit", func(t *testing.T) { assertExecNonZeroExit(t, ctx, exec) })
	t.Run("exec timeout", func(t *testing.T) { assertExecTimeout(t, ctx, exec) })
	t.Run("timeout kills the process tree", func(t *testing.T) { assertTimeoutKillsProcessTree(t, ctx, exec) })
	t.Run("output cap", func(t *testing.T) { assertExecOutputCap(t, ctx, exec) })
	t.Run("file round trip", func(t *testing.T) { assertFileIORoundTrip(t, ctx, exec) })
	t.Run("workspace writable as the sandbox uid", func(t *testing.T) { assertWorkspaceWritableAsSandboxUID(t, ctx, exec) })

	t.Run("no guest network", func(t *testing.T) {
		must := require.New(t)
		res, err := exec.Exec(ctx, "command -v wget >/dev/null 2>&1 || { echo no-wget; exit 0; }; wget -q -T 5 -O /dev/null http://1.1.1.1/ && echo reached || echo unreachable", 0)
		must.NoError(err)
		out := strings.TrimSpace(res.Stdout)
		must.NotEqual("no-wget", out, "the image has no wget; set STIRRUP_TEST_IMAGE to an image that ships it (busybox does)")
		must.Equal("unreachable", out, "stderr %q", res.Stderr)
	})

	t.Run("workspace capacity recorded", func(t *testing.T) {
		must := require.New(t)
		res, err := exec.Exec(ctx, "df -k /workspace && : > /workspace/.stirrup-capacity-probe && rm /workspace/.stirrup-capacity-probe", 0)
		must.NoError(err)
		t.Logf("workspace capacity with no ephemeral-storage declared:\n%s", res.Stdout)
		evidence.writeText(t, "workspace-df.txt", res.Stdout)
		must.Equal(0, res.ExitCode, "the workspace is not writable: stderr %q", res.Stderr)
	})

	t.Run("teardown observed", func(t *testing.T) {
		assertCloseRemovesPolicyAfterPodIsGone(t, ctx, clientset, exec)
	})
}

// newRecordedK8sExecutor is NewK8sExecutor with the clientset's Pod creates
// recorded in record.
func newRecordedK8sExecutor(ctx context.Context, cfg K8sExecutorConfig, record *podCreateRecord) (*K8sExecutor, error) {
	return newK8sExecutorWithClient(ctx, cfg, func(c kubernetes.Interface) kubernetes.Interface {
		return recordingClientset{Interface: c, record: record}
	})
}
