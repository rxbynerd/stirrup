package executor

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/rxbynerd/stirrup/harness/internal/sandboxidentity"
	"github.com/rxbynerd/stirrup/types"
)

// baseSandboxCfg returns a minimal valid K8sExecutorConfig for the pure-helper
// tests. Network mode "none" keeps proxyEnv empty so the spec is unencumbered.
func baseSandboxCfg() K8sExecutorConfig {
	return K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "default",
		Network:   &types.NetworkConfig{Mode: "none"},
	}
}

// TestBuildSandboxPodSpec_ProxyAndExtraEnvAdditive is B-K8S:
// buildSandboxPodSpec is the shared env-merge block for both the k8s and
// k8s-sandbox executors (issue #516) — the production GKE deployment
// target. Every other test in this file calls it with extraEnv == nil, so
// the shared "proxyEnv + extraEnv" append block (harness/internal/executor/k8s.go's
// buildSandboxPodSpec) has no coverage proving both sets survive together:
// a broken index, wrong append order, or accidental overwrite of proxyEnv
// would not be caught by anything else here. This test supplies a
// non-empty proxyEnv (HTTP(S)_PROXY/NO_PROXY, issue #42) alongside a
// realistic non-empty extraEnv (the sandbox identity token plus the
// GIT_CONFIG_* pairs sandboxidentity.ComposeEnv produces, issue #516) and
// asserts both land on the Pod additively.
func TestBuildSandboxPodSpec_ProxyAndExtraEnvAdditive(t *testing.T) {
	cfg := baseSandboxCfg()
	proxyEnv := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: "http://proxy.internal:8080"},
		{Name: "HTTPS_PROXY", Value: "http://proxy.internal:8080"},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1,::1"},
	}

	composed, err := sandboxidentity.ComposeEnv("HAYBALE_TOKEN", "the-jwt-token", &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com"},
		RewriteSsh: true,
	})
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}
	extraEnv := make([]EnvPair, len(composed))
	for i, e := range composed {
		extraEnv[i] = EnvPair{Name: e.Name, Value: e.Value}
	}
	if len(proxyEnv) == 0 || len(extraEnv) == 0 {
		t.Fatalf("precondition: both proxyEnv (%d) and extraEnv (%d) must be non-empty", len(proxyEnv), len(extraEnv))
	}

	spec := buildSandboxPodSpec(cfg, proxyEnv, extraEnv)

	gotEnv := spec.Containers[0].Env
	wantLen := len(proxyEnv) + len(extraEnv)
	if len(gotEnv) != wantLen {
		t.Fatalf("Containers[0].Env has %d entries, want %d (proxyEnv + extraEnv, additive)", len(gotEnv), wantLen)
	}

	// proxyEnv must occupy the leading entries, in order — buildSandboxPodSpec
	// appends proxyEnv first, then extraEnv.
	for i, want := range proxyEnv {
		if gotEnv[i].Name != want.Name || gotEnv[i].Value != want.Value {
			t.Errorf("Env[%d] = %+v, want proxyEnv entry %+v", i, gotEnv[i], want)
		}
	}
	// extraEnv must follow, in order, with none of proxyEnv's entries
	// truncated, overwritten, or reordered away.
	for i, want := range extraEnv {
		idx := len(proxyEnv) + i
		if gotEnv[idx].Name != want.Name || gotEnv[idx].Value != want.Value {
			t.Errorf("Env[%d] = %+v, want extraEnv entry %+v", idx, gotEnv[idx], want)
		}
	}
}

// TestApplyAgentSandboxAdmissionDeltas covers the four GKE secure-sandbox-policy
// deltas: forced gvisor runtime, default CPU+mem limits/requests, the gvisor
// nodeSelector merge, and the gvisor toleration (added once, idempotently).
func TestApplyAgentSandboxAdmissionDeltas(t *testing.T) {
	t.Run("forces gvisor even when runtime was empty", func(t *testing.T) {
		cfg := baseSandboxCfg() // Runtime unset → buildSandboxPodSpec leaves RuntimeClassName nil
		spec := buildSandboxPodSpec(cfg, nil, nil)
		if spec.RuntimeClassName != nil {
			t.Fatalf("precondition: base spec RuntimeClassName = %v, want nil", spec.RuntimeClassName)
		}
		applyAgentSandboxAdmissionDeltas(&spec)
		if spec.RuntimeClassName == nil || *spec.RuntimeClassName != gkeSandboxRuntimeValue {
			t.Errorf("RuntimeClassName = %v, want %q", spec.RuntimeClassName, gkeSandboxRuntimeValue)
		}
	})

	t.Run("fills default cpu+mem limits and requests when resources nil", func(t *testing.T) {
		cfg := baseSandboxCfg()
		spec := buildSandboxPodSpec(cfg, nil, nil)
		// Precondition: no resources pinned (cfg.Resources nil).
		if len(spec.Containers[0].Resources.Limits) != 0 {
			t.Fatalf("precondition: base container has limits %v, want none", spec.Containers[0].Resources.Limits)
		}
		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		cpuLim, ok := res.Limits[corev1.ResourceCPU]
		if !ok || cpuLim.Cmp(agentSandboxDefaultCPU) != 0 {
			t.Errorf("cpu limit = %v, want %v", cpuLim, agentSandboxDefaultCPU)
		}
		memLim, ok := res.Limits[corev1.ResourceMemory]
		if !ok || memLim.Cmp(agentSandboxDefaultMemory) != 0 {
			t.Errorf("memory limit = %v, want %v", memLim, agentSandboxDefaultMemory)
		}
		cpuReq, ok := res.Requests[corev1.ResourceCPU]
		if !ok || cpuReq.Cmp(agentSandboxDefaultCPU) != 0 {
			t.Errorf("cpu request = %v, want %v", cpuReq, agentSandboxDefaultCPU)
		}
		memReq, ok := res.Requests[corev1.ResourceMemory]
		if !ok || memReq.Cmp(agentSandboxDefaultMemory) != 0 {
			t.Errorf("memory request = %v, want %v", memReq, agentSandboxDefaultMemory)
		}
	})

	t.Run("preserves caller-set resources", func(t *testing.T) {
		cfg := baseSandboxCfg()
		cfg.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 1024}
		spec := buildSandboxPodSpec(cfg, nil, nil)
		// Precondition: the caller's values are already on the base spec via
		// resourcesToPodResources.
		wantCPU := resource.MustParse("2")
		wantMem := resource.MustParse("1024Mi")
		if got := spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
			t.Fatalf("precondition: base cpu limit = %v, want %v", got, wantCPU)
		}

		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		if got := res.Limits[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
			t.Errorf("cpu limit = %v, want caller value %v (must not be overwritten with default)", got, wantCPU)
		}
		if got := res.Limits[corev1.ResourceMemory]; got.Cmp(wantMem) != 0 {
			t.Errorf("memory limit = %v, want caller value %v (must not be overwritten with default)", got, wantMem)
		}
	})

	t.Run("fills only the missing side when caller pins one resource", func(t *testing.T) {
		// Caller pins memory but not CPU. The gap-fill must add the CPU default
		// (limit + request) while leaving the caller's memory untouched — the
		// admission policy requires BOTH cpu and memory limits.
		cfg := baseSandboxCfg()
		cfg.Resources = &types.ResourceLimits{MemoryMB: 1024}
		spec := buildSandboxPodSpec(cfg, nil, nil)
		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		if got := res.Limits[corev1.ResourceCPU]; got.Cmp(agentSandboxDefaultCPU) != 0 {
			t.Errorf("cpu limit = %v, want filled default %v", got, agentSandboxDefaultCPU)
		}
		if got := res.Requests[corev1.ResourceCPU]; got.Cmp(agentSandboxDefaultCPU) != 0 {
			t.Errorf("cpu request = %v, want filled default %v", got, agentSandboxDefaultCPU)
		}
		wantMem := resource.MustParse("1024Mi")
		if got := res.Limits[corev1.ResourceMemory]; got.Cmp(wantMem) != 0 {
			t.Errorf("memory limit = %v, want caller value %v (not overwritten by the default)", got, wantMem)
		}
	})

	t.Run("merges nodeSelector with a pre-existing entry", func(t *testing.T) {
		cfg := baseSandboxCfg()
		cfg.NodeSelector = map[string]string{"disktype": "ssd"}
		spec := buildSandboxPodSpec(cfg, nil, nil)
		applyAgentSandboxAdmissionDeltas(&spec)

		if spec.NodeSelector[gkeSandboxRuntimeKey] != gkeSandboxRuntimeValue {
			t.Errorf("nodeSelector[%q] = %q, want %q", gkeSandboxRuntimeKey, spec.NodeSelector[gkeSandboxRuntimeKey], gkeSandboxRuntimeValue)
		}
		if spec.NodeSelector["disktype"] != "ssd" {
			t.Errorf("pre-existing nodeSelector entry was clobbered: %v", spec.NodeSelector)
		}
	})

	t.Run("adds the gvisor toleration and is idempotent", func(t *testing.T) {
		cfg := baseSandboxCfg()
		spec := buildSandboxPodSpec(cfg, nil, nil)

		applyAgentSandboxAdmissionDeltas(&spec)
		applyAgentSandboxAdmissionDeltas(&spec) // second call must not duplicate

		count := 0
		for _, tol := range spec.Tolerations {
			if tol.Key == gkeSandboxRuntimeKey && tol.Value == gkeSandboxRuntimeValue &&
				tol.Operator == corev1.TolerationOpEqual && tol.Effect == corev1.TaintEffectNoSchedule {
				count++
			}
		}
		if count != 1 {
			t.Errorf("gvisor toleration count = %d, want exactly 1 (idempotent)", count)
		}
	})
}

// TestBuildSandboxObject asserts the assembled Sandbox CR carries the
// v1alpha1 apiVersion/kind, the Delete shutdown policy, a ~12h shutdownTime,
// the per-pod labels on the podTemplate, and that the admission deltas
// (gvisor runtime + resource limits) survived the typed→unstructured
// conversion into spec.podTemplate.spec.
func TestBuildSandboxObject(t *testing.T) {
	const ns, name = "default", "stirrup-abc123"

	cfg := baseSandboxCfg()
	spec := buildSandboxPodSpec(cfg, nil, nil)
	applyAgentSandboxAdmissionDeltas(&spec)

	before := time.Now()
	obj, err := buildSandboxObject(ns, name, spec, before.Add(agentSandboxMaxTTL))
	if err != nil {
		t.Fatalf("buildSandboxObject: %v", err)
	}

	if got := obj.Object["apiVersion"]; got != "agents.x-k8s.io/v1alpha1" {
		t.Errorf("apiVersion = %v, want agents.x-k8s.io/v1alpha1", got)
	}
	if got := obj.Object["kind"]; got != "Sandbox" {
		t.Errorf("kind = %v, want Sandbox", got)
	}

	specMap, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec is not a map: %T", obj.Object["spec"])
	}
	if got := specMap["shutdownPolicy"]; got != "Delete" {
		t.Errorf("spec.shutdownPolicy = %v, want Delete", got)
	}

	shutdownStr, ok := specMap["shutdownTime"].(string)
	if !ok {
		t.Fatalf("spec.shutdownTime is not a string: %T", specMap["shutdownTime"])
	}
	shutdown, err := time.Parse(time.RFC3339, shutdownStr)
	if err != nil {
		t.Fatalf("spec.shutdownTime %q does not parse as RFC3339: %v", shutdownStr, err)
	}
	// Should be ~12h ahead of when we built it. Allow a generous slack window so
	// the assertion is not flaky, while still catching a wildly wrong TTL.
	delta := shutdown.Sub(before)
	if delta < agentSandboxMaxTTL-time.Minute || delta > agentSandboxMaxTTL+time.Minute {
		t.Errorf("shutdownTime delta = %v, want ~%v ahead", delta, agentSandboxMaxTTL)
	}

	podTemplate, ok := specMap["podTemplate"].(map[string]any)
	if !ok {
		t.Fatalf("spec.podTemplate is not a map: %T", specMap["podTemplate"])
	}
	ptMeta, ok := podTemplate["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("spec.podTemplate.metadata is not a map: %T", podTemplate["metadata"])
	}
	ptLabels, ok := ptMeta["labels"].(map[string]any)
	if !ok {
		t.Fatalf("spec.podTemplate.metadata.labels is not a map: %T", ptMeta["labels"])
	}
	if got := ptLabels[k8sPodNameLabel]; got != name {
		t.Errorf("podTemplate label %q = %v, want %q", k8sPodNameLabel, got, name)
	}
	if got := ptLabels[k8sSandboxLabel]; got != "true" {
		t.Errorf("podTemplate label %q = %v, want \"true\"", k8sSandboxLabel, got)
	}

	ptSpec, ok := podTemplate["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec.podTemplate.spec is not a map: %T", podTemplate["spec"])
	}
	if got := ptSpec["runtimeClassName"]; got != gkeSandboxRuntimeValue {
		t.Errorf("podTemplate.spec.runtimeClassName = %v, want %q", got, gkeSandboxRuntimeValue)
	}

	containers, ok := ptSpec["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("spec.podTemplate.spec.containers is not a non-empty slice: %T", ptSpec["containers"])
	}
	c0, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("containers[0] is not a map: %T", containers[0])
	}
	resources, ok := c0["resources"].(map[string]any)
	if !ok {
		t.Fatalf("containers[0].resources is not a map: %T", c0["resources"])
	}
	limits, ok := resources["limits"].(map[string]any)
	if !ok {
		t.Fatalf("containers[0].resources.limits is not a map: %T", resources["limits"])
	}
	if _, ok := limits["cpu"]; !ok {
		t.Errorf("containers[0].resources.limits missing cpu: %v", limits)
	}
	if _, ok := limits["memory"]; !ok {
		t.Errorf("containers[0].resources.limits missing memory: %v", limits)
	}

	// The hardened security fields must survive the typed→unstructured conversion
	// into the CR. A regression that silently dropped any of these would weaken
	// the sandbox without failing any other assertion, so guard them explicitly.
	if got := ptSpec["automountServiceAccountToken"]; got != false {
		t.Errorf("podTemplate.spec.automountServiceAccountToken = %v, want false", got)
	}
	if podSC, ok := ptSpec["securityContext"].(map[string]any); !ok {
		t.Errorf("podTemplate.spec.securityContext missing/!map: %T", ptSpec["securityContext"])
	} else if got := podSC["fsGroup"]; got != int64(k8sRunAsUserUID) {
		t.Errorf("pod securityContext.fsGroup = %v (%T), want %d", got, got, k8sRunAsUserUID)
	}

	cSC, ok := c0["securityContext"].(map[string]any)
	if !ok {
		t.Fatalf("containers[0].securityContext is not a map: %T", c0["securityContext"])
	}
	if got := cSC["allowPrivilegeEscalation"]; got != false {
		t.Errorf("container allowPrivilegeEscalation = %v, want false", got)
	}
	if got := cSC["runAsNonRoot"]; got != true {
		t.Errorf("container runAsNonRoot = %v, want true", got)
	}
	if got := cSC["runAsUser"]; got != int64(k8sRunAsUserUID) {
		t.Errorf("container runAsUser = %v (%T), want %d", got, got, k8sRunAsUserUID)
	}
	if seccomp, ok := cSC["seccompProfile"].(map[string]any); !ok {
		t.Errorf("container seccompProfile missing/!map: %T", cSC["seccompProfile"])
	} else if got := seccomp["type"]; got != "RuntimeDefault" {
		t.Errorf("container seccompProfile.type = %v, want RuntimeDefault", got)
	}
	if caps, ok := cSC["capabilities"].(map[string]any); !ok {
		t.Errorf("container capabilities missing/!map: %T", cSC["capabilities"])
	} else if drop, ok := caps["drop"].([]any); !ok || len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("container capabilities.drop = %v, want [ALL]", caps["drop"])
	}

	// The ephemeral /workspace emptyDir must survive too — without it a non-root
	// UID cannot write the workspace.
	vols, ok := ptSpec["volumes"].([]any)
	if !ok || len(vols) == 0 {
		t.Fatalf("podTemplate.spec.volumes is not a non-empty slice: %T", ptSpec["volumes"])
	}
	v0, ok := vols[0].(map[string]any)
	if !ok {
		t.Fatalf("volumes[0] is not a map: %T", vols[0])
	}
	if got := v0["name"]; got != k8sWorkspaceVolume {
		t.Errorf("volumes[0].name = %v, want %q", got, k8sWorkspaceVolume)
	}
	if _, ok := v0["emptyDir"]; !ok {
		t.Errorf("volumes[0] is not an emptyDir: %v", v0)
	}
}
