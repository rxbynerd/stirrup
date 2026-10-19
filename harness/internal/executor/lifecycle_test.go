package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func lifecycleK8sCfg() K8sExecutorConfig {
	return K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "stirrup",
		Network:   &types.NetworkConfig{Mode: "none"},
		Lifecycle: Lifecycle{RunID: "run-42", MaxLifetime: 70 * time.Minute},
	}
}

func TestBuildSandboxPod_DeadlineAndRunLabel(t *testing.T) {
	pod := buildSandboxPod(lifecycleK8sCfg(), "stirrup-abc", nil)

	t.Run("active deadline matches the run lifetime", func(t *testing.T) {
		require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
		require.Equal(t, int64(4200), *pod.Spec.ActiveDeadlineSeconds)
	})
	t.Run("run id label is set", func(t *testing.T) {
		require.Equal(t, "run-42", pod.Labels[k8sRunIDLabel])
	})
	t.Run("sandbox marker and pod name labels are kept", func(t *testing.T) {
		require.Equal(t, "true", pod.Labels[k8sSandboxLabel])
		require.Equal(t, "stirrup-abc", pod.Labels[k8sPodNameLabel])
	})
}

func TestBuildSandboxPod_NoLifetimeLeavesDeadlineUnset(t *testing.T) {
	cfg := lifecycleK8sCfg()
	cfg.Lifecycle = Lifecycle{}
	pod := buildSandboxPod(cfg, "stirrup-abc", nil)

	require.Nil(t, pod.Spec.ActiveDeadlineSeconds)
	require.NotContains(t, pod.Labels, k8sRunIDLabel, "an empty run ID must not produce a run-id label")
}

func TestBuildSandboxPod_OwnerReference(t *testing.T) {
	harness := &HarnessPod{Namespace: "stirrup", Name: "job-xyz", UID: "uid-1"}

	t.Run("same namespace owns the sandbox", func(t *testing.T) {
		cfg := lifecycleK8sCfg()
		cfg.HarnessPod = harness
		pod := buildSandboxPod(cfg, "stirrup-abc", nil)

		require.Len(t, pod.OwnerReferences, 1)
		ref := pod.OwnerReferences[0]
		require.Equal(t, "Pod", ref.Kind)
		require.Equal(t, "v1", ref.APIVersion)
		require.Equal(t, "job-xyz", ref.Name)
		require.Equal(t, "uid-1", string(ref.UID))
		require.Nil(t, ref.Controller, "owner reference must not set controller")
		require.Nil(t, ref.BlockOwnerDeletion, "blockOwnerDeletion needs finalizers RBAC, so it must stay unset")
	})

	t.Run("different namespace gets none", func(t *testing.T) {
		cfg := lifecycleK8sCfg()
		cfg.Namespace = "sandboxes"
		cfg.HarnessPod = harness
		require.Empty(t, buildSandboxPod(cfg, "stirrup-abc", nil).OwnerReferences, "no owner across namespaces")
	})

	t.Run("unknown harness pod gets none", func(t *testing.T) {
		require.Empty(t, buildSandboxPod(lifecycleK8sCfg(), "stirrup-abc", nil).OwnerReferences)
	})
}

func TestHarnessPodFromEnv_ReadsPodIdentity(t *testing.T) {
	env := map[string]string{
		EnvPodName:      "job-xyz",
		EnvPodUID:       "uid-1",
		EnvPodNamespace: "stirrup",
	}
	get := func(k string) string { return env[k] }

	got := HarnessPodFromEnv(get)
	require.NotNil(t, got)
	require.Equal(t, "job-xyz", got.Name)
	require.Equal(t, "uid-1", got.UID)
	require.Equal(t, "stirrup", got.Namespace)

	for _, missing := range []string{EnvPodName, EnvPodUID, EnvPodNamespace} {
		t.Run("nil when "+missing+" is empty", func(t *testing.T) {
			withoutMissing := func(k string) string {
				if k == missing {
					return ""
				}
				return env[k]
			}
			require.Nil(t, HarnessPodFromEnv(withoutMissing))
		})
	}
}

func TestEgressPolicyFor_CarriesRunLabel(t *testing.T) {
	for _, mode := range []string{"none", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			np, err := egressPolicyFor(&types.NetworkConfig{Mode: mode}, "stirrup", "stirrup-abc", "run-42")
			require.NoError(t, err)
			require.Equal(t, "run-42", np.Labels[k8sRunIDLabel])
			require.Equal(t, "true", np.Labels[k8sSandboxLabel])
			require.NotContains(t, np.Spec.PodSelector.MatchLabels, k8sRunIDLabel, "selector must stay keyed on the pod name only")
		})
	}
}

func TestK8sLabelValue_TruncatesToValidLabel(t *testing.T) {
	long := strings.Repeat("a", 62) + "-" + strings.Repeat("b", 20)
	tests := []struct{ name, in, want string }{
		{"keeps a valid run id", "run-1738000000000000000", "run-1738000000000000000"},
		{"keeps an empty value", "", ""},
		{"shortens a value over 63 characters", long, strings.Repeat("a", 62)},
		{"strips an invalid tail", strings.Repeat("a", 62) + "_x", strings.Repeat("a", 62)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := k8sLabelValue(tt.in)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), 63)
		})
	}
}

func TestBuildSandboxObject_CarriesRunLabel(t *testing.T) {
	obj, err := buildSandboxObject("stirrup", "stirrup-abc", "run-42", buildSandboxPodSpec(lifecycleK8sCfg(), nil, nil), time.Now())
	require.NoError(t, err)
	require.Equal(t, "run-42", obj.GetLabels()[k8sRunIDLabel])
}
