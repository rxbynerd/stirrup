package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rxbynerd/stirrup/types"
)

// writeExecKubeconfig writes a kubeconfig whose user authenticates through an
// exec credential plugin running script, and returns its path.
func writeExecKubeconfig(t *testing.T, server, script string) string {
	t.Helper()
	dir := t.TempDir()
	plugin := filepath.Join(dir, "plugin.sh")
	require.New(t).NoError(os.WriteFile(plugin, []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	kubeconfig := filepath.Join(dir, "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %q, insecure-skip-tls-verify: true}
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: %q
      interactiveMode: Never
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
`, server, plugin)
	require.New(t).NoError(os.WriteFile(kubeconfig, []byte(content), 0o600))
	return kubeconfig
}

const (
	hangingPluginScript   = "sleep 3"
	returningPluginScript = `echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"t"}}'`
	// unreachableServer is never contacted by a hung plugin.
	unreachableServer = "https://127.0.0.1:1"
)

func TestProbeK8sAccess_HungCredentialPluginFailsWhenTheContextEnds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kubeconfig := writeExecKubeconfig(t, unreachableServer, hangingPluginScript)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ProbeK8sAccess(ctx, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Kubeconfig: kubeconfig})

	must.Error(err)
	check.Less(time.Since(start), 2*time.Second, "the probe waited for the plugin")
	must.ErrorContains(err, "credential plugin did not return")
	must.ErrorIs(err, context.DeadlineExceeded)
	check.Equal([]K8sProbeKind{K8sProbeCredentialPlugin}, probeErrorKinds(t, err))
}

func TestNewK8sExecutor_HungCredentialPluginFailsWhenTheContextEnds(t *testing.T) {
	cfg := K8sExecutorConfig{
		Image:     "busybox",
		Namespace: testK8sNamespace,
		Network:   &types.NetworkConfig{Mode: "none"},
	}
	tests := []struct {
		name  string
		build func(ctx context.Context, cfg K8sExecutorConfig) error
	}{
		{"k8s", func(ctx context.Context, cfg K8sExecutorConfig) error {
			_, err := NewK8sExecutor(ctx, cfg)
			return err
		}},
		{"k8s-sandbox", func(ctx context.Context, cfg K8sExecutorConfig) error {
			_, err := NewAgentSandboxExecutor(ctx, cfg)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cfg := cfg
			cfg.Kubeconfig = writeExecKubeconfig(t, unreachableServer, hangingPluginScript)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			start := time.Now()
			err := tt.build(ctx, cfg)

			must.Error(err)
			check.Less(time.Since(start), 2*time.Second, "construction waited for the plugin")
			must.ErrorContains(err, "credential plugin did not return")
			must.ErrorIs(err, context.DeadlineExceeded)
		})
	}
}

func versionHandler(hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"31"}`))
	})
}

func newExecRESTConfig(t *testing.T, server, script string) *rest.Config {
	t.Helper()
	restCfg, err := buildRESTConfig(writeExecKubeconfig(t, server, script))
	require.New(t).NoError(err)
	return restCfg
}

func TestAwaitCredentialPlugin_ReturnsOnceThePluginReturns(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(versionHandler(&hits))
	defer server.Close()
	restCfg := newExecRESTConfig(t, server.URL, returningPluginScript)

	must.NoError(awaitCredentialPlugin(context.Background(), restCfg))
	must.Equal(int32(1), hits.Load())
}

func TestAwaitCredentialPlugin_ReusesTheCredentialForLaterClients(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(versionHandler(&hits))
	defer server.Close()
	counter := filepath.Join(t.TempDir(), "runs")
	restCfg := newExecRESTConfig(t, server.URL, "echo run >> "+counter+"\n"+returningPluginScript)

	must.NoError(awaitCredentialPlugin(context.Background(), restCfg))
	kube, err := kubernetes.NewForConfig(restCfg)
	must.NoError(err)
	_, err = kube.Discovery().ServerVersion()
	must.NoError(err)

	runs, err := os.ReadFile(counter)
	must.NoError(err)
	must.Equal("run\n", string(runs), "the plugin ran once for both clients")
	must.Equal(int32(2), hits.Load())
}

func TestAwaitCredentialPlugin_SkipsAConfigWithoutAnExecPlugin(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(versionHandler(&hits))
	defer server.Close()
	restCfg := newExecRESTConfig(t, server.URL, returningPluginScript)
	restCfg.ExecProvider = nil

	must.NoError(awaitCredentialPlugin(context.Background(), restCfg))
	must.Zero(hits.Load(), "no request is made when there is no plugin to wait for")
}

func TestAwaitCredentialPlugin_FailingPluginIsLeftToTheNextRequest(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(versionHandler(&hits))
	defer server.Close()
	restCfg := newExecRESTConfig(t, server.URL, "exit 1")

	must.NoError(awaitCredentialPlugin(context.Background(), restCfg))
	must.Zero(hits.Load(), "the request never reached the server")
}

func TestAwaitCredentialPlugin_DoneContextStartsNothing(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	server := httptest.NewTLSServer(versionHandler(&hits))
	defer server.Close()
	counter := filepath.Join(t.TempDir(), "runs")
	restCfg := newExecRESTConfig(t, server.URL, "echo run >> "+counter+"\n"+returningPluginScript)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := awaitCredentialPlugin(ctx, restCfg)

	must.ErrorContains(err, "credential plugin not started")
	must.ErrorIs(err, context.Canceled)
	must.Zero(hits.Load())
	must.NoFileExists(counter)
}

func TestProbeK8sAccess_SlowAPIServerIsNotReportedAsAHungPlugin(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	kubeconfig := writeExecKubeconfig(t, server.URL, returningPluginScript)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := ProbeK8sAccess(ctx, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Kubeconfig: kubeconfig})

	must.Error(err)
	check.NotContains(err.Error(), "credential plugin")
	check.Equal([]K8sProbeKind{K8sProbeReviewFailed}, probeErrorKinds(t, err))
}
