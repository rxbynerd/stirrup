package executor

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sRuntimeProfiles_MatchTypesRuntimeSet pins the profile registry to
// the k8s runtime values ValidateRunConfig accepts, in both directions, so
// an accepted value always has a profile and no profile is unreachable.
func TestK8sRuntimeProfiles_MatchTypesRuntimeSet(t *testing.T) {
	accepted := append([]string{""}, types.ValidK8sExecutorRuntimeValues()...)
	for _, runtime := range accepted {
		t.Run(fmt.Sprintf("accepted %q has a profile", runtime), func(t *testing.T) {
			must := require.New(t)
			must.Contains(k8sRuntimeProfiles, runtime)
		})
	}
	for runtime := range k8sRuntimeProfiles {
		t.Run(fmt.Sprintf("profile %q is accepted", runtime), func(t *testing.T) {
			must := require.New(t)
			must.Contains(accepted, runtime)
		})
	}
}

func TestK8sRuntimeProfiles_RuntimeClassNames(t *testing.T) {
	t.Run("empty runtime leaves the class unset", func(t *testing.T) {
		must := require.New(t)
		must.Empty(k8sRuntimeProfiles[""].runtimeClassName)
	})
	for _, runtime := range types.ValidK8sExecutorRuntimeValues() {
		if runtime == "nitro-isolated" {
			continue
		}
		t.Run(runtime+" is its own class", func(t *testing.T) {
			must := require.New(t)
			profile := k8sRuntimeProfiles[runtime]
			must.Equal(runtime, profile.runtimeClassName)
			must.Empty(profile.podAnnotations)
			must.Empty(profile.noNetworkAnnotations)
			must.Nil(profile.diagnose)
		})
	}
	t.Run("nitro-isolated selects isolated-sandbox", func(t *testing.T) {
		must := require.New(t)
		profile := k8sRuntimeProfiles["nitro-isolated"]
		must.Equal("isolated-sandbox", profile.runtimeClassName)
		must.Equal(map[string]string{"sandbox.amazonaws.com/log-bandwidth": "20M"}, profile.podAnnotations)
		must.Equal(map[string]string{"sandbox.amazonaws.com/network-isolated": "true"}, profile.noNetworkAnnotations)
		must.NotNil(profile.diagnose)
	})
}

func TestBuildSandboxPod_ShapePerRuntimeProfile(t *testing.T) {
	podFor := func(runtime, mode string) *corev1.Pod {
		cfg := K8sExecutorConfig{Image: "busybox", Namespace: testK8sNamespace, Runtime: runtime, Network: &types.NetworkConfig{Mode: mode}}
		return buildSandboxPod(cfg, k8sRuntimeProfiles[runtime], testK8sPodName, nil)
	}
	for _, runtime := range append([]string{""}, types.ValidK8sExecutorRuntimeValues()...) {
		if runtime == "nitro-isolated" {
			continue
		}
		t.Run(fmt.Sprintf("%q has no annotations", runtime), func(t *testing.T) {
			must := require.New(t)
			pod := podFor(runtime, "none")
			must.Nil(pod.Annotations)
			if runtime == "" {
				must.Nil(pod.Spec.RuntimeClassName)
				return
			}
			must.Equal(ptr.To(runtime), pod.Spec.RuntimeClassName)
		})
	}
	t.Run("nitro-isolated in mode none", func(t *testing.T) {
		must := require.New(t)
		pod := podFor("nitro-isolated", "none")
		must.Equal(ptr.To("isolated-sandbox"), pod.Spec.RuntimeClassName)
		must.Equal(map[string]string{
			"sandbox.amazonaws.com/log-bandwidth":    "20M",
			"sandbox.amazonaws.com/network-isolated": "true",
		}, pod.Annotations)
		must.Empty(pod.Spec.NodeSelector, "the executor adds no node selector of its own")
		must.Empty(pod.Spec.Tolerations, "the executor adds no toleration of its own")
	})
	t.Run("no-network annotation only in mode none", func(t *testing.T) {
		must := require.New(t)
		pod := podFor("nitro-isolated", "allowlist")
		must.Equal(map[string]string{"sandbox.amazonaws.com/log-bandwidth": "20M"}, pod.Annotations)
	})
	for _, runtime := range append([]string{""}, types.ValidK8sExecutorRuntimeValues()...) {
		t.Run(fmt.Sprintf("%q disables service links", runtime), func(t *testing.T) {
			must := require.New(t)
			must.Equal(ptr.To(false), podFor(runtime, "none").Spec.EnableServiceLinks)
		})
	}
	t.Run("the shared spec leaves service links to the caller", func(t *testing.T) {
		must := require.New(t)
		cfg := K8sExecutorConfig{Image: "busybox", Namespace: testK8sNamespace, Network: &types.NetworkConfig{Mode: "none"}}
		must.Nil(buildSandboxPodSpec(cfg, nil, nil).EnableServiceLinks)
	})
	t.Run("the registry maps are not shared with the pod", func(t *testing.T) {
		must := require.New(t)
		pod := podFor("nitro-isolated", "none")
		pod.Annotations["sandbox.amazonaws.com/log-bandwidth"] = "1M"
		must.Equal("20M", k8sRuntimeProfiles["nitro-isolated"].podAnnotations["sandbox.amazonaws.com/log-bandwidth"])
	})
}

func TestK8sRuntimeProfileFor_RejectsUnknownRuntime(t *testing.T) {
	for _, runtime := range []string{"isolated-sandbox", "confidential-sandbox", "runsc", "kata"} {
		t.Run(runtime, func(t *testing.T) {
			must := require.New(t)
			_, err := k8sRuntimeProfileFor(runtime)
			must.ErrorContains(err, fmt.Sprintf("unsupported executor.runtime %q", runtime))
		})
	}
	t.Run("known runtime resolves", func(t *testing.T) {
		must := require.New(t)
		profile, err := k8sRuntimeProfileFor("gvisor")
		must.NoError(err)
		must.Equal("gvisor", profile.runtimeClassName)
	})
}
