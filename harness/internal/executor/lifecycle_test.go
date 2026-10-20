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
		must := require.New(t)
		must.NotNil(pod.Spec.ActiveDeadlineSeconds)
		must.Equal(int64(4200), *pod.Spec.ActiveDeadlineSeconds)
	})
	t.Run("run id label is set", func(t *testing.T) {
		must := require.New(t)
		must.Equal("run-42", pod.Labels[k8sRunIDLabel])
	})
	t.Run("sandbox marker and pod name labels are kept", func(t *testing.T) {
		must := require.New(t)
		must.Equal("true", pod.Labels[k8sSandboxLabel])
		must.Equal("stirrup-abc", pod.Labels[k8sPodNameLabel])
	})
}

func TestBuildSandboxPod_NoLifetimeLeavesDeadlineUnset(t *testing.T) {
	must := require.New(t)
	cfg := lifecycleK8sCfg()
	cfg.Lifecycle = Lifecycle{}
	pod := buildSandboxPod(cfg, "stirrup-abc", nil)

	must.Nil(pod.Spec.ActiveDeadlineSeconds)
	must.NotContains(pod.Labels, k8sRunIDLabel, "an empty run ID must not produce a run-id label")
}

func TestBuildSandboxPod_OwnerReference(t *testing.T) {
	harness := &HarnessPod{Namespace: "stirrup", Name: "job-xyz", UID: "uid-1"}

	t.Run("same namespace owns the sandbox", func(t *testing.T) {
		must := require.New(t)
		cfg := lifecycleK8sCfg()
		cfg.HarnessPod = harness
		pod := buildSandboxPod(cfg, "stirrup-abc", nil)

		must.Len(pod.OwnerReferences, 1)
		ref := pod.OwnerReferences[0]
		must.Equal("Pod", ref.Kind)
		must.Equal("v1", ref.APIVersion)
		must.Equal("job-xyz", ref.Name)
		must.Equal("uid-1", string(ref.UID))
		must.Nil(ref.Controller, "owner reference must not set controller")
		must.Nil(ref.BlockOwnerDeletion, "blockOwnerDeletion needs finalizers RBAC, so it must stay unset")
	})

	t.Run("different namespace gets none", func(t *testing.T) {
		must := require.New(t)
		cfg := lifecycleK8sCfg()
		cfg.Namespace = "sandboxes"
		cfg.HarnessPod = harness
		must.Empty(buildSandboxPod(cfg, "stirrup-abc", nil).OwnerReferences, "no owner across namespaces")
	})

	t.Run("unknown harness pod gets none", func(t *testing.T) {
		must := require.New(t)
		must.Empty(buildSandboxPod(lifecycleK8sCfg(), "stirrup-abc", nil).OwnerReferences)
	})
}

func TestHarnessPodFromEnv_ReadsPodIdentity(t *testing.T) {
	must := require.New(t)
	env := map[string]string{
		EnvPodName:      "job-xyz",
		EnvPodUID:       "uid-1",
		EnvPodNamespace: "stirrup",
	}
	get := func(k string) string { return env[k] }

	got := HarnessPodFromEnv(get)
	must.NotNil(got)
	must.Equal("job-xyz", got.Name)
	must.Equal("uid-1", got.UID)
	must.Equal("stirrup", got.Namespace)

	for _, missing := range []string{EnvPodName, EnvPodUID, EnvPodNamespace} {
		t.Run("nil when "+missing+" is empty", func(t *testing.T) {
			must := require.New(t)
			withoutMissing := func(k string) string {
				if k == missing {
					return ""
				}
				return env[k]
			}
			must.Nil(HarnessPodFromEnv(withoutMissing))
		})
	}
}

func TestEgressPolicyFor_CarriesRunLabel(t *testing.T) {
	for _, mode := range []string{"none", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			np, err := egressPolicyFor(&types.NetworkConfig{Mode: mode}, "stirrup", "stirrup-abc", "run-42")
			must.NoError(err)
			must.Equal("run-42", np.Labels[k8sRunIDLabel])
			must.Equal("true", np.Labels[k8sSandboxLabel])
			must.NotContains(np.Spec.PodSelector.MatchLabels, k8sRunIDLabel, "selector must stay keyed on the pod name only")
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
			must := require.New(t)
			got := k8sLabelValue(tt.in)
			must.Equal(tt.want, got)
			must.LessOrEqual(len(got), 63)
		})
	}
}

func TestBuildSandboxObject_CarriesRunLabel(t *testing.T) {
	must := require.New(t)
	obj, err := buildSandboxObject("stirrup", "stirrup-abc", "run-42", buildSandboxPodSpec(lifecycleK8sCfg(), nil, nil), time.Now())
	must.NoError(err)
	must.Equal("run-42", obj.GetLabels()[k8sRunIDLabel])
}
