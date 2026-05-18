//go:build integration_k8s

package executor

import (
	"context"
	"os"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestK8sExecutorLifecycle exercises the full create/wait/delete lifecycle
// against a real cluster. It is gated by build tag `integration_k8s` and by
// STIRRUP_TEST_KUBECONFIG so the default `just test` run never touches a
// real cluster.
func TestK8sExecutorLifecycle(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}

	namespace := "default"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      "busybox:latest",
		Namespace:  namespace,
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		_ = exec.Close()
	})

	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true

	clientset, err := kubeClientFromConfig(t, kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, getErr := clientset.CoreV1().Pods(namespace).Get(context.Background(), exec.podName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s still present 5s after Close (last get err: %v)", exec.podName, getErr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be idempotent, got: %v", err)
	}
}

// TestK8sExecutorCloseIdempotent verifies the standalone idempotency
// guarantee: calling Close twice on an executor whose Pod is already gone
// must return nil from the second call.
func TestK8sExecutorCloseIdempotent(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      "busybox:latest",
		Namespace:  "default",
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be nil, got: %v", err)
	}
}

func kubeClientFromConfig(t *testing.T, kubeconfig string) (kubernetes.Interface, error) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}
