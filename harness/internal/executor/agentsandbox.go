package executor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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
	"k8s.io/client-go/rest"
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
	// Sandbox; Close() deletes it once the Sandbox is confirmed gone.
	networkPolicyName string
	runID             string
	timing            k8sTiming

	// closeMu serialises Close; closed records a completed teardown so later
	// calls return without touching the cluster.
	closeMu sync.Mutex
	closed  bool
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
func buildSandboxObject(namespace, name, runID string, podSpec corev1.PodSpec, shutdownTime time.Time) (*metav1unstructured.Unstructured, error) {
	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podSpec)
	if err != nil {
		return nil, fmt.Errorf("convert pod spec to unstructured: %w", err)
	}

	// Widen to map[string]any (the unstructured tree's type) rather than
	// rebuild the labels inline and risk drift from the NetworkPolicy's
	// selector.
	labels := map[string]any{}
	for k, v := range podLabels(name, runID) {
		labels[k] = v
	}

	crLabels := map[string]any{}
	for k, v := range sandboxLabels(runID) {
		crLabels[k] = v
	}

	return &metav1unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": sandboxGVR.GroupVersion().String(),
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels":    crLabels,
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
// controller to report it Ready. A failure after the Sandbox create tears down
// through Close, which removes the policy only once the Sandbox and its
// backing Pod are gone; a failed create is reconciled by name first.
func NewAgentSandboxExecutor(ctx context.Context, cfg K8sExecutorConfig) (*AgentSandboxExecutor, error) {
	if err := checkAgentSandboxRuntime(cfg.Runtime); err != nil {
		return nil, err
	}
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

	return newAgentSandboxExecutor(ctx, cfg, proxyEnv, clientset, dyn, restCfg, defaultK8sTiming, slog.Default())
}

// newAgentSandboxExecutor is NewAgentSandboxExecutor after the clients are
// built.
func newAgentSandboxExecutor(ctx context.Context, cfg K8sExecutorConfig, proxyEnv []corev1.EnvVar, clientset kubernetes.Interface, dyn dynamic.Interface, restCfg *rest.Config, timing k8sTiming, logger *slog.Logger) (*AgentSandboxExecutor, error) {
	if err := checkAgentSandboxRuntime(cfg.Runtime); err != nil {
		return nil, err
	}

	name, err := generatePodName()
	if err != nil {
		return nil, fmt.Errorf("generate sandbox name: %w", err)
	}

	spec := buildSandboxPodSpec(cfg, proxyEnv, cfg.ExtraEnv)
	applyAgentSandboxAdmissionDeltas(&spec)

	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, name, cfg.Lifecycle.RunID)
	if err != nil {
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("install egress NetworkPolicy for sandbox %s/%s: %w", cfg.Namespace, name, boundClusterErr(err))
	}

	partial := &AgentSandboxExecutor{
		podExecCore:       podExecCore{clientset: clientset, namespace: cfg.Namespace, podName: name, logger: logger},
		dynamicClient:     dyn,
		sandboxName:       name,
		networkPolicyName: policy.Name,
		runID:             cfg.Lifecycle.RunID,
		timing:            timing,
	}

	obj, err := buildSandboxObject(cfg.Namespace, name, cfg.Lifecycle.RunID, spec, time.Now().Add(agentSandboxMaxTTL))
	if err != nil {
		return nil, withTeardownErr(fmt.Errorf("build sandbox object: %w", err), partial.egress().removePolicy())
	}

	if _, err := dyn.Resource(sandboxGVR).Namespace(cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		createErr := fmt.Errorf("create sandbox %s/%s: %w", cfg.Namespace, name, boundClusterErr(err))
		return nil, withTeardownErr(createErr, partial.reconcileFailedCreate(sandboxLabels(cfg.Lifecycle.RunID), err))
	}

	// rollback tears a partial construction down through Close, so the
	// policy outlives every Sandbox that might still back a running Pod.
	rollback := func(cause error) error {
		return withTeardownErr(cause, partial.Close())
	}

	var ready *metav1unstructured.Unstructured
	err = wait.PollUntilContextTimeout(ctx, timing.pollPeriod, timing.readyTimeout, true, func(ctx context.Context) (bool, error) {
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
		// Duration omitted: readiness can end on the ceiling or a shorter
		// caller deadline, and a hardcoded value would misreport the latter.
		return nil, rollback(fmt.Errorf("sandbox %s/%s not ready: %w", cfg.Namespace, name, boundClusterErr(err)))
	}

	podName := resolveSandboxPodName(ready)

	// Cold-create only: an adopted (warm-pool) Sandbox's Pod name would not
	// match, and the per-pod egress NetworkPolicy would bind to nothing. See
	// docs/executors/k8s-agent-sandbox.md.
	if podName != name {
		return nil, rollback(fmt.Errorf(
			"sandbox %s/%s resolved to backing pod %q: warm-pool adoption is not supported by the agent sandbox executor (its per-pod egress policy would not bind)",
			cfg.Namespace, name, clusterText(podName),
		))
	}

	return &AgentSandboxExecutor{
		podExecCore: podExecCore{
			clientset:       clientset,
			restConfig:      restCfg,
			namespace:       cfg.Namespace,
			podName:         podName,
			network:         cfg.Network,
			Security:        cfg.Security,
			logger:          logger,
			sandboxIdentity: cfg.SandboxIdentityToken,
		},
		dynamicClient:     dyn,
		sandboxName:       name,
		networkPolicyName: policy.Name,
		runID:             cfg.Lifecycle.RunID,
		timing:            timing,
	}, nil
}

// Close deletes the Sandbox, waits until a read finds it gone and then until
// a read finds no backing Pod under its name, and only then deletes the
// egress NetworkPolicy, on bounded contexts independent of the caller's.
// The Pod read does not rely on the controller's cascade order. Otherwise
// the policy is retained and the error names it; see
// docs/executors/k8s-agent-sandbox.md.
func (e *AgentSandboxExecutor) Close() error {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	if e.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.effectiveTiming().teardownTimeout)
	defer cancel()

	err := e.terminateSandbox(ctx)
	if err == nil {
		err = e.backingPod().waitForTermination(ctx)
	}
	if err != nil {
		err = e.egress().retain(err)
	} else {
		err = e.egress().removePolicy()
	}
	e.closed = err == nil
	return err
}

func (e *AgentSandboxExecutor) effectiveTiming() k8sTiming {
	if e.timing == (k8sTiming{}) {
		return defaultK8sTiming
	}
	return e.timing
}

// backingPod is the teardown view of the controller-created Pod. Its UID is
// not recorded, so only a read finding no Pod under the name confirms it.
func (e *AgentSandboxExecutor) backingPod() podTeardown {
	return podTeardown{
		clientset: e.clientset,
		namespace: e.namespace,
		podName:   e.podName,
		timing:    e.effectiveTiming(),
	}
}

func (e *AgentSandboxExecutor) egress() sandboxEgress {
	return sandboxEgress{
		clientset:  e.clientset,
		kind:       "sandbox",
		namespace:  e.namespace,
		name:       e.sandboxName,
		policyName: e.networkPolicyName,
		runID:      e.runID,
		logger:     e.logger,
		timing:     e.effectiveTiming(),
	}
}

// terminateSandbox issues the foreground delete, retrying transient failures,
// and waits until a read finds the Sandbox gone. A delete response alone is
// not taken as proof. Forbidden and Unauthorized stop at once.
func (e *AgentSandboxExecutor) terminateSandbox(ctx context.Context) error {
	timing := e.effectiveTiming()
	sandboxes := e.dynamicClient.Resource(sandboxGVR).Namespace(e.namespace)
	foreground := metav1.DeletePropagationForeground
	for {
		err := sandboxes.Delete(ctx, e.sandboxName, metav1.DeleteOptions{PropagationPolicy: &foreground})
		if err == nil || apierrors.IsNotFound(err) {
			break
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return fmt.Errorf("delete sandbox: %w", boundClusterErr(err))
		}
		if !sleepCtx(ctx, timing.pollPeriod) {
			return fmt.Errorf("delete sandbox: %w (gave up within the %s teardown bound)", boundClusterErr(err), timing.teardownTimeout)
		}
	}

	var lastErr error
	for {
		_, err := sandboxes.Get(ctx, e.sandboxName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return fmt.Errorf("confirm sandbox deletion: %w", boundClusterErr(err))
		case err != nil:
			lastErr = err
		default:
			lastErr = nil
		}
		if !sleepCtx(ctx, timing.pollPeriod) {
			if lastErr != nil {
				return fmt.Errorf("sandbox deletion not confirmed within the %s teardown bound: %w", timing.teardownTimeout, boundClusterErr(lastErr))
			}
			return fmt.Errorf("sandbox deletion not confirmed within the %s teardown bound", timing.teardownTimeout)
		}
	}
}

// reconcileFailedCreate applies the k8s executor's create-error rule to the
// Sandbox: a Sandbox found under the name with every submitted label is torn
// down through Close; one without them is left alone and the policy
// retained; with none found the policy is removed only after a definitive
// rejection.
func (e *AgentSandboxExecutor) reconcileFailedCreate(submitted map[string]string, createErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.effectiveTiming().teardownTimeout)
	defer cancel()

	sandbox, err := e.lookUpSandbox(ctx)
	if err != nil {
		return e.egress().retain(fmt.Errorf("look up sandbox after a failed create: %w", err))
	}
	if sandbox == nil {
		if createRejectedDefinitively(createErr) {
			return e.egress().removePolicy()
		}
		return e.egress().retain(fmt.Errorf("sandbox %s/%s was not found after a create whose outcome is unknown; a create persisted later starts under the retained policy", e.namespace, e.sandboxName))
	}
	labels := sandbox.GetLabels()
	keys := make([]string, 0, len(submitted))
	for k := range submitted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got, ok := labels[k]; !ok || got != submitted[k] {
			return e.egress().retain(fmt.Errorf("sandbox %s/%s exists but does not carry this run's label %s=%s, so it is not deleted", e.namespace, e.sandboxName, k, submitted[k]))
		}
	}
	return e.Close()
}

// lookUpSandbox reads the Sandbox by name, retrying transient errors until
// ctx ends; Forbidden and Unauthorized stop at once. It returns nil when the
// Sandbox does not exist.
func (e *AgentSandboxExecutor) lookUpSandbox(ctx context.Context) (*metav1unstructured.Unstructured, error) {
	timing := e.effectiveTiming()
	for {
		sandbox, err := e.dynamicClient.Resource(sandboxGVR).Namespace(e.namespace).Get(ctx, e.sandboxName, metav1.GetOptions{})
		switch {
		case err == nil:
			return sandbox, nil
		case apierrors.IsNotFound(err):
			return nil, nil
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return nil, boundClusterErr(err)
		}
		if !sleepCtx(ctx, timing.pollPeriod) {
			return nil, boundClusterErr(err)
		}
	}
}

// checkAgentSandboxRuntime rejects every runtime but gVisor, before any
// cluster call: the Agent Sandbox path runs only under gVisor, so another
// value would otherwise run the workload under a runtime the caller did not
// name.
func checkAgentSandboxRuntime(runtime string) error {
	if runtime == "" || runtime == gkeSandboxRuntimeValue {
		return nil
	}
	return fmt.Errorf("agent sandbox executor: executor.runtime must be %q or empty (got %q); the Agent Sandbox path is gVisor-only", gkeSandboxRuntimeValue, runtime)
}

var _ Executor = (*AgentSandboxExecutor)(nil)
