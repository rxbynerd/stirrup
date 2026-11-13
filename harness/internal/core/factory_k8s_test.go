package core

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

var errK8sConstructorReplaced = errors.New("k8s constructor replaced by the test")

// captureK8sExecutorConfig replaces the k8s executor constructor for the
// test, recording the config it is called with and failing the build.
func captureK8sExecutorConfig(t *testing.T) *[]executor.K8sExecutorConfig {
	t.Helper()
	var calls []executor.K8sExecutorConfig
	prev := newK8sExecutorFn
	newK8sExecutorFn = func(_ context.Context, cfg executor.K8sExecutorConfig) (*executor.K8sExecutor, error) {
		calls = append(calls, cfg)
		return nil, errK8sConstructorReplaced
	}
	t.Cleanup(func() { newK8sExecutorFn = prev })
	return &calls
}

func nitroExecutorConfig() types.ExecutorConfig {
	return types.ExecutorConfig{
		Type:         "k8s",
		Runtime:      "nitro-isolated",
		Image:        "busybox",
		K8sNamespace: "stirrup-sandbox",
		Network:      &types.NetworkConfig{Mode: "none"},
		Resources:    &types.ResourceLimits{CPUs: 1, MemoryMB: 1024},
	}
}

func TestBuildExecutor_K8sPassesRuntimeSecurityAndLimits(t *testing.T) {
	must := require.New(t)
	calls := captureK8sExecutorConfig(t)
	secLogger := security.NewSecurityLogger(io.Discard, "run-1")
	cfg := nitroExecutorConfig()

	_, err := buildExecutor(context.Background(), cfg, nil, secLogger, nil, executor.Lifecycle{RunID: "run-1"})

	must.ErrorIs(err, errK8sConstructorReplaced)
	must.Len(*calls, 1)
	got := (*calls)[0]
	must.Equal("nitro-isolated", got.Runtime)
	must.Same(secLogger, got.Security)
	must.Equal(cfg.Network, got.Network)
	must.Equal(cfg.Resources, got.Resources)
	must.Empty(got.ExtraEnv)
	must.False(got.SandboxIdentityToken)
}

// TestBuildLoopWithTransport_ExtraEnvOnlyWithSandboxIdentity pins the assumption the
// k8s executor makes when it treats any ExtraEnv as sandbox identity: the
// factory fills ExtraEnv only when executor.sandboxIdentity is configured.
func TestBuildLoopWithTransport_ExtraEnvOnlyWithSandboxIdentity(t *testing.T) {
	captureExtraEnv := func(t *testing.T) *[][]executor.EnvPair {
		t.Helper()
		var seen [][]executor.EnvPair
		prev := buildExecutorFn
		buildExecutorFn = func(_ context.Context, _ types.ExecutorConfig, _ security.SecretStore, _ *security.SecurityLogger, extraEnv []executor.EnvPair, _ executor.Lifecycle) (executor.Executor, error) {
			seen = append(seen, extraEnv)
			return nil, errK8sConstructorReplaced
		}
		t.Cleanup(func() { buildExecutorFn = prev })
		return &seen
	}

	t.Run("without sandbox identity", func(t *testing.T) {
		must := require.New(t)
		seen := captureExtraEnv(t)
		config := sandboxIdentityContainerConfig(t, "k8s-extra-env-none", "http://127.0.0.1:1")
		config.Executor = nitroExecutorConfig()

		_, err := BuildLoopWithTransport(context.Background(), config, &fakeControlPlaneTransport{respondToken: "jwt"})

		must.ErrorIs(err, errK8sConstructorReplaced)
		must.Len(*seen, 1)
		must.Empty((*seen)[0])
	})

	t.Run("with sandbox identity", func(t *testing.T) {
		must := require.New(t)
		seen := captureExtraEnv(t)
		config := sandboxIdentityContainerConfig(t, "k8s-extra-env-identity", "http://127.0.0.1:1")

		_, err := BuildLoopWithTransport(context.Background(), config, &fakeControlPlaneTransport{respondToken: "jwt"})

		must.ErrorIs(err, errK8sConstructorReplaced)
		must.Len(*seen, 1)
		must.NotEmpty((*seen)[0])
	})
}
