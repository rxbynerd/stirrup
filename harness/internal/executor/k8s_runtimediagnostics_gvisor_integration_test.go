//go:build integration_k8s

package executor

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sRuntimeDiagnostics_GVisorPassesOnGVisorPod constructs an executor
// with executor.runtime gvisor and requires the gVisor diagnostics to pass
// over the real pods/exec path. It runs when STIRRUP_TEST_RUNTIME_CLASS is
// gvisor and that class's handler is runsc.
func TestK8sRuntimeDiagnostics_GVisorPassesOnGVisorPod(t *testing.T) {
	cfg := testK8sEnv(t)
	if cfg.runtimeClass != "gvisor" {
		t.Skip("STIRRUP_TEST_RUNTIME_CLASS is not gvisor")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()

	security := &recordingSecurityEmitter{}
	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      cfg.image,
		Namespace:  cfg.namespace,
		Kubeconfig: cfg.kubeconfig,
		Runtime:    "gvisor",
		Network:    &types.NetworkConfig{Mode: "none"},
		Security:   security,
	})
	must.NoError(err, "NewK8sExecutor")
	t.Cleanup(func() { _ = exec.Close() })

	must.Len(security.runtimeDiagnostics, 1)
	event := security.runtimeDiagnostics[0]
	t.Logf("observed=%v", event.observed)
	must.True(event.passed)
	must.True(strings.HasSuffix(event.observed["kernelVersion"], gvisorKernelBuildStamp), "kernelVersion %q", event.observed["kernelVersion"])
	must.Equal("present", event.observed["sentryMeminfo"])
}

// TestK8sRuntimeDiagnostics_GVisorFailsOnNonGVisorPod runs the gVisor
// diagnostics over the real pods/exec path of a Pod that is not under
// gVisor. Both guest files must be read, and the kernel version check must
// fail closed. STIRRUP_TEST_RUNTIME_CLASS must not select gvisor.
func TestK8sRuntimeDiagnostics_GVisorFailsOnNonGVisorPod(t *testing.T) {
	cfg := testK8sEnv(t)
	if cfg.runtimeClass == "gvisor" {
		t.Skip("STIRRUP_TEST_RUNTIME_CLASS selects gvisor; this test needs a Pod outside gVisor")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
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
	err = runK8sRuntimeDiagnostics(ctx, "gvisor", k8sRuntimeProfiles["gvisor"], &exec.podExecCore)
	must.ErrorContains(err, "kernel_version: ")

	must.Len(security.runtimeDiagnostics, 1)
	event := security.runtimeDiagnostics[0]
	t.Logf("reason=%q observed=%v", event.reason, event.observed)
	must.False(event.passed)
	must.Equal("kernel_version", event.check)
	must.True(strings.HasPrefix(event.observed["kernelVersion"], "Linux version "), "/proc/version read from the real guest")
	must.Equal("absent", event.observed["sentryMeminfo"])
}

// TestK8sRuntimeDiagnostics_GVisorRejectsMiswiredClass runs executor.runtime
// gvisor on the RuntimeClass named by STIRRUP_TEST_MISWIRED_GVISOR_CLASS,
// whose handler must be runc, standing in for a class named gvisor that does
// not run gVisor. Construction must fail on the diagnostics and remove the
// Pod.
func TestK8sRuntimeDiagnostics_GVisorRejectsMiswiredClass(t *testing.T) {
	cfg := testK8sEnv(t)
	miswired := os.Getenv("STIRRUP_TEST_MISWIRED_GVISOR_CLASS")
	if miswired == "" {
		t.Skip("STIRRUP_TEST_MISWIRED_GVISOR_CLASS not set; it names a RuntimeClass whose handler is runc")
	}
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), k8sIntegrationTimeout)
	defer cancel()

	security := &recordingSecurityEmitter{}
	runID := "gvisor-miswired-" + time.Now().UTC().Format("150405")
	execCfg := K8sExecutorConfig{
		Image:     cfg.image,
		Namespace: cfg.namespace,
		Runtime:   "gvisor",
		Network:   &types.NetworkConfig{Mode: "none"},
		Security:  security,
		Lifecycle: Lifecycle{RunID: runID},
	}
	profile, proxyEnv, err := prepareK8sExecutor(execCfg)
	must.NoError(err)
	profile.runtimeClassName = miswired
	restCfg, err := buildRESTConfig(cfg.kubeconfig)
	must.NoError(err)
	clientset, err := kubernetes.NewForConfig(restCfg)
	must.NoError(err)

	exec, err := newK8sExecutor(ctx, execCfg, profile, proxyEnv, clientset, restCfg, nil, defaultK8sTiming, slog.Default())
	if exec != nil {
		t.Cleanup(func() { _ = exec.Close() })
	}
	must.Nil(exec)
	must.ErrorContains(err, `runtime diagnostics for executor.runtime "gvisor" failed: kernel_version: `)
	t.Logf("error=%v", err)

	must.Len(security.runtimeDiagnostics, 1)
	event := security.runtimeDiagnostics[0]
	t.Logf("observed=%v", event.observed)
	must.False(event.passed)
	must.Equal("kernel_version", event.check)
	must.Equal("absent", event.observed["sentryMeminfo"])

	pods, err := clientset.CoreV1().Pods(cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: k8sRunIDLabel + "=" + runID})
	must.NoError(err)
	must.Empty(pods.Items, "the failed run's Pod was removed")
	policies, err := clientset.NetworkingV1().NetworkPolicies(cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: k8sRunIDLabel + "=" + runID})
	must.NoError(err)
	must.Empty(policies.Items, "the failed run's NetworkPolicy was removed")
}
