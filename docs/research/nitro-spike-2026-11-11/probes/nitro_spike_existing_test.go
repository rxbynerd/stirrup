//go:build integration_k8s

package executor

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

type nitroSpikeRoundTripper struct {
	next http.RoundTripper
	t    *testing.T
}

func (r nitroSpikeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	status := 0
	protocol := ""
	responseUpgrade := ""
	if resp != nil {
		status = resp.StatusCode
		protocol = resp.Header.Get("Sec-WebSocket-Protocol")
		responseUpgrade = resp.Header.Get("Upgrade")
	}
	// Record protocol evidence only. Never log authorization or complete headers.
	if status == http.StatusSwitchingProtocols || req.Header.Get("Upgrade") != "" || strings.HasSuffix(req.URL.Path, "/exec") {
		r.t.Logf("stream method=%s request_upgrade=%s response_upgrade=%s status=%d websocket_protocol=%s error=%v", req.Method, req.Header.Get("Upgrade"), responseUpgrade, status, protocol, err)
	}
	return resp, err
}

// Diagnostic only: the original 30-second cap failure remains separate evidence.
func TestNitroSpike_OutputDiagnostics(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	pod := os.Getenv("STIRRUP_NITRO_SPIKE_POD")
	ns := os.Getenv("STIRRUP_TEST_NAMESPACE")
	if kubeconfig == "" || pod == "" || ns == "" {
		t.Skip("set dedicated kubeconfig, namespace and existing Pod")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return nitroSpikeRoundTripper{rt, t} })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	core := &podExecCore{clientset: client, restConfig: cfg, namespace: ns, podName: pod}
	for _, protocol := range []string{"normal", "forced_spdy"} {
		t.Run(protocol, func(t *testing.T) {
			previous := newRemoteExecutor
			t.Cleanup(func() { newRemoteExecutor = previous })
			if protocol == "forced_spdy" {
				newRemoteExecutor = func(config *rest.Config, target *url.URL) (remotecommand.Executor, error) {
					return remotecommand.NewSPDYExecutor(config, "POST", target)
				}
			}
			t.Run("one_mib", func(t *testing.T) {
				start := time.Now()
				r, err := core.Exec(context.Background(), "head -c 1048576 /dev/zero", 30*time.Second)
				length := 0
				if r != nil {
					length = len(r.Stdout)
				}
				t.Logf("protocol=%s elapsed=%s stdout_bytes=%d error=%v", protocol, time.Since(start), length, err)
				require.NoError(t, err)
				require.Equal(t, 1048576, length)
			})
			t.Run("original_cap_90s", func(t *testing.T) {
				start := time.Now()
				r, err := core.Exec(context.Background(), "yes x | head -c 11534336", 90*time.Second)
				length := 0
				if r != nil {
					length = len(r.Stdout)
				}
				t.Logf("protocol=%s elapsed=%s retained_stdout_bytes=%d error=%v", protocol, time.Since(start), length, err)
				require.ErrorIs(t, err, errK8sOutputCap)
			})
		})
	}
}

// Research adapter to an already-created Pod. This does not test constructor validation/admission.
func TestNitroSpike_ExistingPod(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	pod := os.Getenv("STIRRUP_NITRO_SPIKE_POD")
	ns := os.Getenv("STIRRUP_TEST_NAMESPACE")
	if kubeconfig == "" || pod == "" || ns == "" {
		t.Skip("set dedicated kubeconfig, namespace and existing Pod")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return nitroSpikeRoundTripper{rt, t} })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	core := &podExecCore{clientset: client, restConfig: cfg, namespace: ns, podName: pod, sandboxIdentity: true}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	t.Run("exec", func(t *testing.T) {
		r, err := core.Exec(ctx, "echo out; echo err >&2; exit 7", 10*time.Second)
		require.NoError(t, err)
		require.Equal(t, 7, r.ExitCode)
		require.Equal(t, "out\n", r.Stdout)
		require.Equal(t, "err\n", r.Stderr)
	})
	t.Run("forced_spdy", func(t *testing.T) {
		previous := newRemoteExecutor
		t.Cleanup(func() { newRemoteExecutor = previous })
		newRemoteExecutor = func(config *rest.Config, target *url.URL) (remotecommand.Executor, error) {
			return remotecommand.NewSPDYExecutor(config, "POST", target)
		}
		r, err := core.Exec(ctx, "echo spdy; echo stderr >&2; exit 7", 10*time.Second)
		require.NoError(t, err)
		require.Equal(t, 7, r.ExitCode)
		require.Equal(t, "spdy\n", r.Stdout)
		require.Equal(t, "stderr\n", r.Stderr)
	})
	t.Run("tar", func(t *testing.T) {
		for name, content := range map[string]string{"ascii.txt": "hello\n", "utf8.txt": "héllo 世界\n", "nul.bin": "before\x00after", "nested/a.txt": "deep\n"} {
			require.NoError(t, core.WriteFile(ctx, name, content))
			got, err := core.ReadFile(ctx, name)
			require.NoError(t, err)
			require.Equal(t, content, got)
		}
	})
	t.Run("output_cap", func(t *testing.T) {
		_, err := core.Exec(ctx, "yes x | head -c 11534336", 30*time.Second)
		require.ErrorIs(t, err, errK8sOutputCap)
	})
	t.Run("timeout_process_tree", func(t *testing.T) {
		r, err := core.Exec(ctx, "rm -f /tmp/exec-after-timeout /tmp/stream-after-timeout", 10*time.Second)
		require.NoError(t, err)
		require.Equal(t, 0, r.ExitCode)
		assertTimeoutKillsProcessTree(t, ctx, core)
	})
	t.Run("identity_tmpfs", func(t *testing.T) {
		require.NoError(t, core.WriteSandboxIdentityToken(ctx, "dummy-research-token"))
		r, err := core.Exec(ctx, `awk '$2 == "/run/stirrup/sandbox-identity" { print $3; found=1 } END { if (!found) exit 1 }' /proc/mounts`, 10*time.Second)
		require.NoError(t, err)
		require.Equal(t, 0, r.ExitCode)
		require.Equal(t, "tmpfs\n", r.Stdout)
	})
}
