package executor

import (
	"strings"
	"testing"
	"time"

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

	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 4200 {
		t.Errorf("activeDeadlineSeconds = %v, want 4200", pod.Spec.ActiveDeadlineSeconds)
	}
	if got := pod.Labels[k8sRunIDLabel]; got != "run-42" {
		t.Errorf("run-id label = %q, want run-42", got)
	}
	if pod.Labels[k8sSandboxLabel] != "true" || pod.Labels[k8sPodNameLabel] != "stirrup-abc" {
		t.Errorf("labels = %v, want the sandbox marker and pod-name labels kept", pod.Labels)
	}
}

func TestBuildSandboxPod_NoLifetimeLeavesDeadlineUnset(t *testing.T) {
	cfg := lifecycleK8sCfg()
	cfg.Lifecycle = Lifecycle{}
	pod := buildSandboxPod(cfg, "stirrup-abc", nil)

	if pod.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("activeDeadlineSeconds = %d, want nil", *pod.Spec.ActiveDeadlineSeconds)
	}
	if _, ok := pod.Labels[k8sRunIDLabel]; ok {
		t.Errorf("labels = %v, want no run-id label for an empty run ID", pod.Labels)
	}
}

func TestBuildSandboxPod_OwnerReference(t *testing.T) {
	harness := &HarnessPod{Namespace: "stirrup", Name: "job-xyz", UID: "uid-1"}

	t.Run("same namespace owns the sandbox", func(t *testing.T) {
		cfg := lifecycleK8sCfg()
		cfg.HarnessPod = harness
		pod := buildSandboxPod(cfg, "stirrup-abc", nil)

		if len(pod.OwnerReferences) != 1 {
			t.Fatalf("ownerReferences = %v, want exactly one", pod.OwnerReferences)
		}
		ref := pod.OwnerReferences[0]
		if ref.Kind != "Pod" || ref.APIVersion != "v1" || ref.Name != "job-xyz" || string(ref.UID) != "uid-1" {
			t.Errorf("ownerReference = %+v, want Pod v1 job-xyz/uid-1", ref)
		}
		if ref.Controller != nil || ref.BlockOwnerDeletion != nil {
			t.Errorf("ownerReference = %+v, want neither controller nor blockOwnerDeletion (the latter needs finalizers RBAC)", ref)
		}
	})

	t.Run("different namespace gets none", func(t *testing.T) {
		cfg := lifecycleK8sCfg()
		cfg.Namespace = "sandboxes"
		cfg.HarnessPod = harness
		if refs := buildSandboxPod(cfg, "stirrup-abc", nil).OwnerReferences; len(refs) != 0 {
			t.Errorf("ownerReferences = %v, want none across namespaces", refs)
		}
	})

	t.Run("unknown harness pod gets none", func(t *testing.T) {
		if refs := buildSandboxPod(lifecycleK8sCfg(), "stirrup-abc", nil).OwnerReferences; len(refs) != 0 {
			t.Errorf("ownerReferences = %v, want none", refs)
		}
	})
}

func TestHarnessPodFromEnv(t *testing.T) {
	env := map[string]string{
		EnvPodName:      "job-xyz",
		EnvPodUID:       "uid-1",
		EnvPodNamespace: "stirrup",
	}
	get := func(k string) string { return env[k] }

	got := HarnessPodFromEnv(get)
	if got == nil || got.Name != "job-xyz" || got.UID != "uid-1" || got.Namespace != "stirrup" {
		t.Fatalf("HarnessPodFromEnv = %+v, want job-xyz/uid-1 in stirrup", got)
	}

	for _, missing := range []string{EnvPodName, EnvPodUID, EnvPodNamespace} {
		saved := env[missing]
		env[missing] = ""
		if p := HarnessPodFromEnv(get); p != nil {
			t.Errorf("HarnessPodFromEnv without %s = %+v, want nil", missing, p)
		}
		env[missing] = saved
	}
}

func TestEgressPolicyFor_CarriesRunLabel(t *testing.T) {
	for _, mode := range []string{"none", "allowlist"} {
		np, err := egressPolicyFor(&types.NetworkConfig{Mode: mode}, "stirrup", "stirrup-abc", "run-42")
		if err != nil {
			t.Fatalf("egressPolicyFor(%s): %v", mode, err)
		}
		if np.Labels[k8sRunIDLabel] != "run-42" || np.Labels[k8sSandboxLabel] != "true" {
			t.Errorf("%s policy labels = %v, want sandbox marker and run-id", mode, np.Labels)
		}
		if _, ok := np.Spec.PodSelector.MatchLabels[k8sRunIDLabel]; ok {
			t.Errorf("%s policy selector = %v, must stay keyed on the pod name only", mode, np.Spec.PodSelector.MatchLabels)
		}
	}
}

func TestK8sLabelValue(t *testing.T) {
	long := strings.Repeat("a", 62) + "-" + strings.Repeat("b", 20)
	tests := []struct{ in, want string }{
		{"run-1738000000000000000", "run-1738000000000000000"},
		{"", ""},
		{long, strings.Repeat("a", 62)},
		{strings.Repeat("a", 62) + "_x", strings.Repeat("a", 62)},
	}
	for _, tt := range tests {
		if got := k8sLabelValue(tt.in); got != tt.want {
			t.Errorf("k8sLabelValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if len(k8sLabelValue(tt.in)) > 63 {
			t.Errorf("k8sLabelValue(%q) exceeds 63 characters", tt.in)
		}
	}
}

func TestBuildSandboxObject_CarriesRunLabel(t *testing.T) {
	obj, err := buildSandboxObject("stirrup", "stirrup-abc", "run-42", buildSandboxPodSpec(lifecycleK8sCfg(), nil, nil), time.Now())
	if err != nil {
		t.Fatalf("buildSandboxObject: %v", err)
	}
	if got := obj.GetLabels()[k8sRunIDLabel]; got != "run-42" {
		t.Errorf("sandbox run-id label = %q, want run-42", got)
	}
}
