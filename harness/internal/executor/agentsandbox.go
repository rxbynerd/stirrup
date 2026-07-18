package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	// gkeSandboxRuntimeKey / gkeSandboxRuntimeValue are the gVisor
	// nodeSelector/toleration GKE's secure-sandbox-policy admission requires on
	// every Sandbox; see docs/executors/k8s-agent-sandbox.md.
	gkeSandboxRuntimeKey   = "sandbox.gke.io/runtime"
	gkeSandboxRuntimeValue = "gvisor"

	// agentSandboxMaxTTL is the GC backstop TTL applied if Close() never runs;
	// see docs/executors/k8s-agent-sandbox.md.
	agentSandboxMaxTTL = 12 * time.Hour
)

// agentSandboxDefaultCPU / agentSandboxDefaultMemory are the per-container
// limit+request defaults GKE's secure-sandbox-policy requires (see
// docs/executors/k8s-agent-sandbox.md). Package vars, not consts:
// resource.Quantity is not constant-expressible.
var (
	agentSandboxDefaultCPU    = resource.MustParse("500m")
	agentSandboxDefaultMemory = resource.MustParse("512Mi")
)

// AgentSandboxExecutor implements Executor by provisioning the sandbox Pod
// through GKE's Agent Sandbox controller (agents.x-k8s.io/v1alpha1 Sandbox),
// rather than creating a raw Pod directly as K8sExecutor does. See
// docs/executors/k8s-agent-sandbox.md.
type AgentSandboxExecutor struct {
	podExecCore
	// dynamicClient drives the Sandbox CR; podExecCore's clientset handles
	// pods/exec and the NetworkPolicy.
	dynamicClient dynamic.Interface
	// sandboxName is the Sandbox CR's name; on the cold-create path it is
	// also the backing Pod's name (resolved into podExecCore.podName).
	sandboxName string
	// networkPolicyName is the egress NetworkPolicy installed alongside the
	// Sandbox; Close() deletes it best-effort after the Sandbox.
	networkPolicyName string
}

// applyAgentSandboxAdmissionDeltas mutates a base Pod spec (as produced by
// buildSandboxPodSpec) so the embedding Sandbox CR satisfies GKE's
// secure-sandbox-policy admission policy. See "The four GKE admission
// deltas" in docs/executors/k8s-agent-sandbox.md.
func applyAgentSandboxAdmissionDeltas(spec *corev1.PodSpec) {
	spec.RuntimeClassName = ptr.To(gkeSandboxRuntimeValue)

	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		// DeepCopy avoids aliasing resource.Quantity's internal *big.Int across
		// containers; a plain struct copy would let a later mutation corrupt the
		// shared package default.
		if _, ok := c.Resources.Limits[corev1.ResourceCPU]; !ok {
			cpu := agentSandboxDefaultCPU.DeepCopy()
			c.Resources.Limits[corev1.ResourceCPU] = cpu
			c.Resources.Requests[corev1.ResourceCPU] = cpu
		}
		if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
			mem := agentSandboxDefaultMemory.DeepCopy()
			c.Resources.Limits[corev1.ResourceMemory] = mem
			c.Resources.Requests[corev1.ResourceMemory] = mem
		}
	}

	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[gkeSandboxRuntimeKey] = gkeSandboxRuntimeValue

	// Append only if an equivalent toleration is not already present.
	hasToleration := false
	for _, t := range spec.Tolerations {
		if t.Key == gkeSandboxRuntimeKey && t.Value == gkeSandboxRuntimeValue &&
			t.Operator == corev1.TolerationOpEqual && t.Effect == corev1.TaintEffectNoSchedule {
			hasToleration = true
			break
		}
	}
	if !hasToleration {
		spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
			Key:      gkeSandboxRuntimeKey,
			Operator: corev1.TolerationOpEqual,
			Value:    gkeSandboxRuntimeValue,
			Effect:   corev1.TaintEffectNoSchedule,
		})
	}
}

// buildSandboxObject assembles the agents.x-k8s.io/v1alpha1 Sandbox custom
// resource the GKE controller reconciles into a Pod. shutdownPolicy
// "Delete" + shutdownTime is the GC backstop (agentSandboxMaxTTL); the
// controller propagates podTemplate labels to the Pod so the per-pod
// stirrup.dev/pod label set by podLabels lands on it and the egress
// NetworkPolicy binds to it.
func buildSandboxObject(namespace, name string, podSpec corev1.PodSpec, shutdownTime time.Time) (*metav1unstructured.Unstructured, error) {
	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podSpec)
	if err != nil {
		return nil, fmt.Errorf("convert pod spec to unstructured: %w", err)
	}

	// Widen to map[string]any (the unstructured tree's type) rather than
	// rebuild the labels inline and risk drift from the NetworkPolicy's
	// selector.
	labels := map[string]any{}
	for k, v := range podLabels(name) {
		labels[k] = v
	}

	return &metav1unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": sandboxGVR.GroupVersion().String(),
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]any{
					k8sSandboxLabel: "true",
				},
			},
			"spec": map[string]any{
				"shutdownPolicy": "Delete",
				"shutdownTime":   shutdownTime.Format(time.RFC3339),
				"podTemplate": map[string]any{
					"metadata": map[string]any{
						"labels": labels,
					},
					"spec": specMap,
				},
			},
		},
	}, nil
}

// NewAgentSandboxExecutor provisions a sandbox via the Agent Sandbox CRD and
// returns an executor bound to the backing Pod. It mirrors NewK8sExecutor's
// fail-closed ordering: validate config and compute the proxy env before any
// cluster op, install the egress NetworkPolicy BEFORE the Sandbox so the Pod
// never runs with unrestricted egress, then create the Sandbox and wait for the
// controller to report it Ready. On any failure the partially created objects
// (NetworkPolicy and/or Sandbox) are deleted best-effort before the error is
// returned.
func NewAgentSandboxExecutor(ctx context.Context, cfg K8sExecutorConfig) (*AgentSandboxExecutor, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("agent sandbox executor requires an image")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("agent sandbox executor requires a namespace")
	}

	if cfg.Network == nil {
		return nil, fmt.Errorf("agent sandbox executor requires a network config (set executor.network.mode to \"none\" or \"allowlist\")")
	}

	proxyEnv, err := proxyEnvFor(cfg.Network, cfg.EgressProxyURL)
	if err != nil {
		return nil, err
	}

	restCfg, err := buildRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build kube rest config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kube clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}

	logger := slog.Default()

	if cfg.RuntimeClassName != "" && cfg.RuntimeClassName != gkeSandboxRuntimeValue {
		logger.Warn(
			"agent sandbox executor: overriding executor.runtime with gvisor — the Agent Sandbox path is gVisor-only",
			"requested", cfg.RuntimeClassName,
		)
	}

	name, err := generatePodName()
	if err != nil {
		return nil, fmt.Errorf("generate sandbox name: %w", err)
	}

	spec := buildSandboxPodSpec(cfg, proxyEnv)
	applyAgentSandboxAdmissionDeltas(&spec)

	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, name)
	if err != nil {
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("install egress NetworkPolicy for sandbox %s/%s: %w", cfg.Namespace, name, err)
	}

	obj, err := buildSandboxObject(cfg.Namespace, name, spec, time.Now().Add(agentSandboxMaxTTL))
	if err != nil {
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		return nil, fmt.Errorf("build sandbox object: %w", err)
	}
	if _, err := dyn.Resource(sandboxGVR).Namespace(cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		return nil, fmt.Errorf("create sandbox %s/%s: %w", cfg.Namespace, name, err)
	}

	var ready *metav1unstructured.Unstructured
	err = wait.PollUntilContextTimeout(ctx, k8sReadyPollPeriod, k8sReadyTimeout, true, func(ctx context.Context) (bool, error) {
		got, getErr := dyn.Resource(sandboxGVR).Namespace(cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			// Only an RBAC denial fails fast; other errors retry until the
			// readiness deadline (see docs/executors/k8s-agent-sandbox.md).
			if apierrors.IsForbidden(getErr) || apierrors.IsUnauthorized(getErr) {
				return false, getErr
			}
			logger.Debug("transient error polling sandbox readiness, retrying", "namespace", cfg.Namespace, "sandbox", name, "err", getErr)
			return false, nil
		}
		ready = got
		return sandboxReady(got), nil
	})
	if err != nil {
		deleteSandboxBestEffort(dyn, cfg.Namespace, name, logger)
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		// Duration omitted: readiness can end on k8sReadyTimeout or a shorter
		// caller deadline, and a hardcoded value would misreport the latter.
		return nil, fmt.Errorf("sandbox %s/%s not ready: %w", cfg.Namespace, name, err)
	}

	podName := resolveSandboxPodName(ready)

	// Cold-create only: an adopted (warm-pool) Sandbox's Pod name would not
	// match, and the per-pod egress NetworkPolicy would bind to nothing. See
	// docs/executors/k8s-agent-sandbox.md.
	if podName != name {
		deleteSandboxBestEffort(dyn, cfg.Namespace, name, logger)
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		return nil, fmt.Errorf(
			"sandbox %s/%s resolved to backing pod %q: warm-pool adoption is not supported by the agent sandbox executor (its per-pod egress policy would not bind)",
			cfg.Namespace, name, podName,
		)
	}

	return &AgentSandboxExecutor{
		podExecCore: podExecCore{
			clientset:  clientset,
			restConfig: restCfg,
			namespace:  cfg.Namespace,
			podName:    podName,
			network:    cfg.Network,
			Security:   cfg.Security,
			logger:     logger,
		},
		dynamicClient:     dyn,
		sandboxName:       name,
		networkPolicyName: policy.Name,
	}, nil
}

// Close tears down the sandbox with its own background context, deleting
// only the Sandbox (not the Pod, which the controller cascade-GCs via
// Foreground propagation) and then the egress NetworkPolicy. See "The
// controller-owned TTL GC backstop" in docs/executors/k8s-agent-sandbox.md
// for the fail-closed ordering.
func (e *AgentSandboxExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()

	foreground := metav1.DeletePropagationForeground
	sbErr := e.dynamicClient.Resource(sandboxGVR).Namespace(e.namespace).Delete(ctx, e.sandboxName, metav1.DeleteOptions{
		PropagationPolicy: &foreground,
	})
	if sbErr != nil && !apierrors.IsNotFound(sbErr) {
		// The Pod may still be running; the policy is left in place.
		return fmt.Errorf("delete sandbox: %w", sbErr)
	}

	if e.networkPolicyName == "" {
		return nil
	}

	gone := apierrors.IsNotFound(sbErr)
	if !gone {
		waitErr := wait.PollUntilContextTimeout(ctx, k8sReadyPollPeriod, k8sCloseTimeout, true, func(ctx context.Context) (bool, error) {
			_, getErr := e.dynamicClient.Resource(sandboxGVR).Namespace(e.namespace).Get(ctx, e.sandboxName, metav1.GetOptions{})
			return apierrors.IsNotFound(getErr), nil
		})
		gone = waitErr == nil
	}
	if !gone {
		e.logger.Warn(
			"agent sandbox executor: sandbox not confirmed deleted within close timeout; leaving egress NetworkPolicy in place (fail closed)",
			"namespace", e.namespace, "sandbox", e.sandboxName, "networkpolicy", e.networkPolicyName,
		)
		return nil
	}

	npErr := e.clientset.NetworkingV1().NetworkPolicies(e.namespace).Delete(ctx, e.networkPolicyName, metav1.DeleteOptions{})
	if npErr != nil && !apierrors.IsNotFound(npErr) {
		e.logger.Warn("agent sandbox executor networkpolicy delete failed", "namespace", e.namespace, "networkpolicy", e.networkPolicyName, "err", npErr)
	}
	return nil
}

// deleteSandboxBestEffort deletes the Sandbox CR, logging (not returning)
// any error. Used on constructor error paths to undo a partial create.
func deleteSandboxBestEffort(dyn dynamic.Interface, namespace, name string, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()
	err := dyn.Resource(sandboxGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		logger.Warn("agent sandbox executor cleanup sandbox delete failed", "namespace", namespace, "sandbox", name, "err", err)
	}
}

var _ Executor = (*AgentSandboxExecutor)(nil)
