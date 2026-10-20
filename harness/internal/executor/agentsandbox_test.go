package executor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// requireQuantityAt asserts that list[name] is set and numerically equal to
// want. Quantities are compared with Cmp, so "1" and "1000m" match.
func requireQuantityAt(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want resource.Quantity, what string) {
	t.Helper()
	must := require.New(t)
	got, ok := list[name]
	must.True(ok, "%s is not set", what)
	must.Zero(got.Cmp(want), "%s = %s, want %s", what, got.String(), want.String())
}

// asObjectMap returns an unstructured value as a map, failing the test when it
// has another type.
func asObjectMap(t *testing.T, path string, v any) map[string]any {
	t.Helper()
	must := require.New(t)
	m, ok := v.(map[string]any)
	must.Truef(ok, "%s is %T, want a map", path, v)
	return m
}

// asObjectSlice returns an unstructured value as a slice, failing the test when
// it has another type.
func asObjectSlice(t *testing.T, path string, v any) []any {
	t.Helper()
	must := require.New(t)
	s, ok := v.([]any)
	must.Truef(ok, "%s is %T, want a slice", path, v)
	return s
}

// TestBuildSandboxPodSpec_ProxyAndExtraEnvAdditive checks that buildSandboxPodSpec
// keeps both the proxy env and the extra env, with the proxy entries first.
func TestBuildSandboxPodSpec_ProxyAndExtraEnvAdditive(t *testing.T) {
	must := require.New(t)
	cfg := baseSandboxCfg()
	proxyEnv := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: "http://proxy.internal:8080"},
		{Name: "HTTPS_PROXY", Value: "http://proxy.internal:8080"},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1,::1"},
	}

	composed, err := sandboxidentity.ComposeEnv("HAYBALE_TOKEN", "the-jwt-token", SandboxIdentityTokenPath, &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com"},
		RewriteSsh: true,
	})
	must.NoError(err, "ComposeEnv")
	extraEnv := make([]EnvPair, len(composed))
	for i, e := range composed {
		extraEnv[i] = EnvPair{Name: e.Name, Value: e.Value}
	}
	must.NotEmpty(proxyEnv, "precondition: proxyEnv")
	must.NotEmpty(extraEnv, "precondition: extraEnv")

	spec := buildSandboxPodSpec(cfg, proxyEnv, extraEnv)

	gotEnv := spec.Containers[0].Env
	must.Len(gotEnv, len(proxyEnv)+len(extraEnv), "Containers[0].Env must hold proxyEnv and extraEnv")

	for i, want := range proxyEnv {
		must.Equal(want.Name, gotEnv[i].Name, "Env[%d] name", i)
		must.Equal(want.Value, gotEnv[i].Value, "Env[%d] value", i)
	}
	for i, want := range extraEnv {
		idx := len(proxyEnv) + i
		must.Equal(want.Name, gotEnv[idx].Name, "Env[%d] name", idx)
		must.Equal(want.Value, gotEnv[idx].Value, "Env[%d] value", idx)
	}
}

// TestApplyAgentSandboxAdmissionDeltas_AppliesGVisorPolicy covers the four GKE
// secure-sandbox-policy deltas: the forced gvisor runtime, default CPU and memory
// limits and requests, the gvisor nodeSelector merge, and the gvisor toleration,
// which is added once even when the deltas are applied twice.
func TestApplyAgentSandboxAdmissionDeltas_AppliesGVisorPolicy(t *testing.T) {
	t.Run("forces gvisor even when runtime was empty", func(t *testing.T) {
		must := require.New(t)
		cfg := baseSandboxCfg() // Runtime is unset, so buildSandboxPodSpec leaves RuntimeClassName nil.
		spec := buildSandboxPodSpec(cfg, nil, nil)
		must.Nil(spec.RuntimeClassName, "precondition: base RuntimeClassName")

		applyAgentSandboxAdmissionDeltas(&spec)

		must.NotNil(spec.RuntimeClassName)
		must.Equal(gkeSandboxRuntimeValue, *spec.RuntimeClassName)
	})

	t.Run("fills default cpu+mem limits and requests when resources nil", func(t *testing.T) {
		must := require.New(t)
		cfg := baseSandboxCfg()
		spec := buildSandboxPodSpec(cfg, nil, nil)
		must.Empty(spec.Containers[0].Resources.Limits, "precondition: no limits without cfg.Resources")

		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		requireQuantityAt(t, res.Limits, corev1.ResourceCPU, agentSandboxDefaultCPU, "cpu limit")
		requireQuantityAt(t, res.Limits, corev1.ResourceMemory, agentSandboxDefaultMemory, "memory limit")
		requireQuantityAt(t, res.Requests, corev1.ResourceCPU, agentSandboxDefaultCPU, "cpu request")
		requireQuantityAt(t, res.Requests, corev1.ResourceMemory, agentSandboxDefaultMemory, "memory request")
	})

	t.Run("preserves caller-set resources", func(t *testing.T) {
		cfg := baseSandboxCfg()
		cfg.Resources = &types.ResourceLimits{CPUs: 2, MemoryMB: 1024}
		spec := buildSandboxPodSpec(cfg, nil, nil)
		wantCPU := resource.MustParse("2")
		wantMem := resource.MustParse("1024Mi")
		// The caller's values are already on the base spec via resourcesToPodResources.
		requireQuantityAt(t, spec.Containers[0].Resources.Limits, corev1.ResourceCPU, wantCPU, "precondition: base cpu limit")

		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		requireQuantityAt(t, res.Limits, corev1.ResourceCPU, wantCPU, "cpu limit")
		requireQuantityAt(t, res.Limits, corev1.ResourceMemory, wantMem, "memory limit")
	})

	t.Run("fills only the missing side when caller pins one resource", func(t *testing.T) {
		// The caller pins memory but not CPU. The CPU default must fill both the
		// limit and the request, and the caller's memory must stay as set, because
		// the admission policy requires both CPU and memory limits.
		cfg := baseSandboxCfg()
		cfg.Resources = &types.ResourceLimits{MemoryMB: 1024}
		spec := buildSandboxPodSpec(cfg, nil, nil)

		applyAgentSandboxAdmissionDeltas(&spec)

		res := spec.Containers[0].Resources
		requireQuantityAt(t, res.Limits, corev1.ResourceCPU, agentSandboxDefaultCPU, "cpu limit")
		requireQuantityAt(t, res.Requests, corev1.ResourceCPU, agentSandboxDefaultCPU, "cpu request")
		requireQuantityAt(t, res.Limits, corev1.ResourceMemory, resource.MustParse("1024Mi"), "memory limit")
	})

	t.Run("merges nodeSelector with a pre-existing entry", func(t *testing.T) {
		must := require.New(t)
		cfg := baseSandboxCfg()
		cfg.NodeSelector = map[string]string{"disktype": "ssd"}
		spec := buildSandboxPodSpec(cfg, nil, nil)

		applyAgentSandboxAdmissionDeltas(&spec)

		must.Equal(gkeSandboxRuntimeValue, spec.NodeSelector[gkeSandboxRuntimeKey], "nodeSelector[%s]", gkeSandboxRuntimeKey)
		must.Equal("ssd", spec.NodeSelector["disktype"], "pre-existing nodeSelector entry")
	})

	t.Run("adds the gvisor toleration and is idempotent", func(t *testing.T) {
		must := require.New(t)
		cfg := baseSandboxCfg()
		spec := buildSandboxPodSpec(cfg, nil, nil)

		applyAgentSandboxAdmissionDeltas(&spec)
		applyAgentSandboxAdmissionDeltas(&spec) // The second call must not add a duplicate.

		count := 0
		for _, tol := range spec.Tolerations {
			if tol.Key == gkeSandboxRuntimeKey && tol.Value == gkeSandboxRuntimeValue &&
				tol.Operator == corev1.TolerationOpEqual && tol.Effect == corev1.TaintEffectNoSchedule {
				count++
			}
		}
		must.Equal(1, count, "gvisor toleration count")
	})
}

// TestBuildSandboxObject_EmitsSandboxCR checks the assembled Sandbox CR: the
// v1alpha1 apiVersion and kind, the Delete shutdown policy, a 12h shutdown time,
// the per-pod labels on the pod template, and the admission deltas and hardened
// security fields that must survive the typed-to-unstructured conversion.
func TestBuildSandboxObject_EmitsSandboxCR(t *testing.T) {
	must := require.New(t)
	const ns, name = "default", "stirrup-abc123"

	cfg := baseSandboxCfg()
	spec := buildSandboxPodSpec(cfg, nil, nil)
	applyAgentSandboxAdmissionDeltas(&spec)

	before := time.Now()
	obj, err := buildSandboxObject(ns, name, "", spec, before.Add(agentSandboxMaxTTL))
	must.NoError(err, "buildSandboxObject")

	specMap := asObjectMap(t, "spec", obj.Object["spec"])
	podTemplate := asObjectMap(t, "spec.podTemplate", specMap["podTemplate"])
	ptSpec := asObjectMap(t, "spec.podTemplate.spec", podTemplate["spec"])
	containers := asObjectSlice(t, "spec.podTemplate.spec.containers", ptSpec["containers"])
	must.NotEmpty(containers, "spec.podTemplate.spec.containers")
	c0 := asObjectMap(t, "containers[0]", containers[0])

	t.Run("identifies the sandbox kind", func(t *testing.T) {
		must := require.New(t)
		must.Equal("agents.x-k8s.io/v1alpha1", obj.Object["apiVersion"])
		must.Equal("Sandbox", obj.Object["kind"])
	})

	t.Run("sets the Delete shutdown policy and a 12h shutdown time", func(t *testing.T) {
		must := require.New(t)
		must.Equal("Delete", specMap["shutdownPolicy"])

		shutdownStr, ok := specMap["shutdownTime"].(string)
		must.Truef(ok, "spec.shutdownTime is %T, want a string", specMap["shutdownTime"])
		shutdown, err := time.Parse(time.RFC3339, shutdownStr)
		must.NoError(err, "spec.shutdownTime %q", shutdownStr)
		// The one-minute slack keeps the check stable while still catching a wrong TTL.
		must.WithinDuration(before.Add(agentSandboxMaxTTL), shutdown, time.Minute, "spec.shutdownTime")
	})

	t.Run("labels the pod template", func(t *testing.T) {
		must := require.New(t)
		ptMeta := asObjectMap(t, "spec.podTemplate.metadata", podTemplate["metadata"])
		ptLabels := asObjectMap(t, "spec.podTemplate.metadata.labels", ptMeta["labels"])
		must.Equal(name, ptLabels[k8sPodNameLabel], "label %s", k8sPodNameLabel)
		must.Equal("true", ptLabels[k8sSandboxLabel], "label %s", k8sSandboxLabel)
	})

	t.Run("keeps the gvisor runtime and default resource limits", func(t *testing.T) {
		must := require.New(t)
		must.Equal(gkeSandboxRuntimeValue, ptSpec["runtimeClassName"])

		resources := asObjectMap(t, "containers[0].resources", c0["resources"])
		limits := asObjectMap(t, "containers[0].resources.limits", resources["limits"])
		must.Contains(limits, "cpu")
		must.Contains(limits, "memory")
	})

	t.Run("keeps the hardened pod security context", func(t *testing.T) {
		must := require.New(t)
		must.Equal(false, ptSpec["automountServiceAccountToken"])

		podSC := asObjectMap(t, "spec.podTemplate.spec.securityContext", ptSpec["securityContext"])
		must.Equal(int64(k8sRunAsUserUID), podSC["fsGroup"])
	})

	t.Run("keeps the hardened container security context", func(t *testing.T) {
		must := require.New(t)
		cSC := asObjectMap(t, "containers[0].securityContext", c0["securityContext"])
		must.Equal(false, cSC["allowPrivilegeEscalation"])
		must.Equal(true, cSC["runAsNonRoot"])
		must.Equal(int64(k8sRunAsUserUID), cSC["runAsUser"])

		seccomp := asObjectMap(t, "containers[0].securityContext.seccompProfile", cSC["seccompProfile"])
		must.Equal("RuntimeDefault", seccomp["type"])

		caps := asObjectMap(t, "containers[0].securityContext.capabilities", cSC["capabilities"])
		must.Equal([]any{"ALL"}, caps["drop"])
	})

	t.Run("keeps the ephemeral workspace emptyDir volume", func(t *testing.T) {
		must := require.New(t)
		// Without this volume a non-root UID cannot write the workspace.
		volumes := asObjectSlice(t, "spec.podTemplate.spec.volumes", ptSpec["volumes"])
		must.NotEmpty(volumes, "spec.podTemplate.spec.volumes")
		v0 := asObjectMap(t, "volumes[0]", volumes[0])
		must.Equal(k8sWorkspaceVolume, v0["name"])
		must.Contains(v0, "emptyDir")
	})
}
