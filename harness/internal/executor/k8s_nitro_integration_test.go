//go:build integration_k8s

package executor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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
// or fails its diagnostics by design. The Pod is pinned to the instance type
// in STIRRUP_TEST_NITRO_INSTANCE_TYPE and the architecture in
// STIRRUP_TEST_NITRO_ARCH (default m5d.xlarge and amd64, the tested recipe).
// With STIRRUP_TEST_EVIDENCE_DIR set, the
// submitted Pod, the Create response, the Ready Pod and the workspace size
// are written there.
func TestK8sNitroIsolated_EndToEnd(t *testing.T) {
	cfg := testK8sEnv(t)
	if os.Getenv("STIRRUP_TEST_NITRO_ISOLATED") == "" {
		t.Skip("STIRRUP_TEST_NITRO_ISOLATED not set; this group needs an EKS cluster with Nitro Sandboxes")
	}
	must := require.New(t)
	budget := nitroSubtestBudget(t)
	t.Logf("subtest budget %s after construction; pod lifetime %s", budget, k8sReadyTimeout+budget)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()
	evidence := evidenceDirFromEnv()

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	must.NoError(err, "build verification clientset")
	owner := createStandInOwnerPod(t, ctx, clientset, cfg.namespace, cfg.image)

	security := &recordingSecurityEmitter{}
	execCfg := K8sExecutorConfig{
		Image:        cfg.image,
		Namespace:    cfg.namespace,
		Kubeconfig:   cfg.kubeconfig,
		NodeSelector: nitroNodeSelector(),
		Runtime:      "nitro-isolated",
		Resources:    &types.ResourceLimits{CPUs: 1, MemoryMB: 1024},
		Network:      &types.NetworkConfig{Mode: "none"},
		Security:     security,
		Lifecycle:    Lifecycle{RunID: "nitro-end-to-end", MaxLifetime: k8sReadyTimeout + budget},
		HarnessPod:   &HarnessPod{Namespace: cfg.namespace, Name: owner.Name, UID: string(owner.UID)},
	}
	record := &podCreateRecord{}
	exec, err := newRecordedK8sExecutor(ctx, execCfg, record)
	evidence.writeCreateEvidence(t, record)
	must.NoError(err, "NewK8sExecutor: the admission check or the runtime diagnostics rejected the Pod")
	t.Cleanup(func() { _ = exec.Close() })

	// Cold start may have used most of the construction context, so the
	// subtests get their own.
	ctx, cancelSubtests := context.WithTimeout(context.Background(), budget)
	defer cancelSubtests()

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
		res, err := exec.Exec(ctx, "command -v wget >/dev/null 2>&1 || { echo no-wget; exit 0; }; wget -q -T 5 -O /dev/null http://1.1.1.1/ && echo reached || echo unreachable", nitroProbeTimeout)
		must.NoError(err)
		out := strings.TrimSpace(res.Stdout)
		must.NotEqual("no-wget", out, "the image has no wget; set STIRRUP_TEST_IMAGE to an image that ships it (busybox does)")
		must.Equal("unreachable", out, "stderr %q", res.Stderr)
	})

	t.Run("workspace capacity recorded", func(t *testing.T) {
		must := require.New(t)
		res, err := exec.Exec(ctx, "df -k /workspace && : > /workspace/.stirrup-capacity-probe && rm /workspace/.stirrup-capacity-probe", nitroProbeTimeout)
		must.NoError(err)
		t.Logf("workspace capacity with no ephemeral-storage declared:\n%s", res.Stdout)
		evidence.writeText(t, "workspace-df.txt", res.Stdout)
		must.Equal(0, res.ExitCode, "the workspace is not writable: stderr %q", res.Stderr)
	})

	t.Run("teardown observed", func(t *testing.T) {
		assertCloseRemovesPolicyAfterPodIsGone(t, ctx, clientset, exec)
	})
}

// TestK8sNitroIsolated_DiskMbBoundsTheGuestDisk declares resources.diskMb
// and checks that the guest's workspace is about that size and refuses a
// write beyond it. Gated like TestK8sNitroIsolated_EndToEnd.
func TestK8sNitroIsolated_DiskMbBoundsTheGuestDisk(t *testing.T) {
	cfg := testK8sEnv(t)
	if os.Getenv("STIRRUP_TEST_NITRO_ISOLATED") == "" {
		t.Skip("STIRRUP_TEST_NITRO_ISOLATED not set; this test needs an EKS cluster with Nitro Sandboxes")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()

	const diskMB = 1024
	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:        cfg.image,
		Namespace:    cfg.namespace,
		Kubeconfig:   cfg.kubeconfig,
		NodeSelector: nitroNodeSelector(),
		Runtime:      "nitro-isolated",
		Resources:    &types.ResourceLimits{CPUs: 1, MemoryMB: 1024, DiskMB: diskMB},
		Network:      &types.NetworkConfig{Mode: "none"},
		Lifecycle:    Lifecycle{RunID: "nitro-disk", MaxLifetime: k8sIntegrationTimeout},
	})
	must.NoError(err, "NewK8sExecutor with resources.diskMb")
	t.Cleanup(func() { _ = exec.Close() })

	res, err := exec.Exec(ctx, "df -k /workspace | tail -1", nitroProbeTimeout)
	must.NoError(err)
	t.Logf("workspace with diskMb=%d: %s", diskMB, strings.TrimSpace(res.Stdout))
	fields := strings.Fields(res.Stdout)
	must.GreaterOrEqual(len(fields), 2, "df output %q", res.Stdout)
	totalKiB, err := strconv.ParseInt(fields[1], 10, 64)
	must.NoError(err, "df output %q", res.Stdout)
	must.LessOrEqual(totalKiB, int64(diskMB)*1024, "the workspace is no larger than the declared size")
	must.Greater(totalKiB, int64(diskMB)*1024*3/4, "the workspace is close to the declared size")

	res, err = exec.Exec(ctx, fmt.Sprintf("dd if=/dev/zero of=/workspace/fill bs=1M count=%d", diskMB+256), 2*time.Minute)
	must.NoError(err)
	must.NotEqual(0, res.ExitCode, "a write beyond the declared size fails")
	must.Contains(res.Stderr, "No space left on device")

	res, err = exec.Exec(ctx, "rm /workspace/fill && : > /workspace/after && echo ok", nitroProbeTimeout)
	must.NoError(err)
	must.Equal("ok", strings.TrimSpace(res.Stdout), "the sandbox stays usable after the disk filled: stderr %q", res.Stderr)
}

// nitroNodeSelector pins a Nitro test Pod to the architecture and instance
// type under test.
func nitroNodeSelector() map[string]string {
	return map[string]string{
		"kubernetes.io/arch":               envOr("STIRRUP_TEST_NITRO_ARCH", "amd64"),
		"node.kubernetes.io/instance-type": envOr("STIRRUP_TEST_NITRO_INSTANCE_TYPE", "m5d.xlarge"),
	}
}

// nitroProbeTimeout bounds the group's own network and capacity probes.
const nitroProbeTimeout = 30 * time.Second

// nitroSubtestBudget is the sum of the subtests' own bounds, one term per
// subtest in the order they run; the file round trip is four writes and
// four reads.
func nitroSubtestBudget(t *testing.T) time.Duration {
	t.Helper()
	return 2*10*time.Second +
		4*time.Second +
		(2*time.Second + 8*time.Second + 3*30*time.Second) +
		outputCapTimeout(t) +
		8*k8sFileIOTimeout +
		30*time.Second +
		2*nitroProbeTimeout +
		k8sCloseTimeout
}

// newRecordedK8sExecutor is NewK8sExecutor with the clientset's Pod creates
// recorded in record.
func newRecordedK8sExecutor(ctx context.Context, cfg K8sExecutorConfig, record *podCreateRecord) (*K8sExecutor, error) {
	return newK8sExecutorWithClient(ctx, cfg, func(c kubernetes.Interface) kubernetes.Interface {
		return recordingClientset{Interface: c, record: record}
	})
}
