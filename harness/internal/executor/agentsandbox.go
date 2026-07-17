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
	// gkeSandboxRuntimeKey / gkeSandboxRuntimeValue are the nodeSelector and
	// toleration key/value GKE's secure-sandbox-policy ValidatingAdmissionPolicy
	// requires on every Sandbox: the workload must be pinned to gVisor nodes and
	// tolerate their NoSchedule taint. Without both, the admission policy rejects
	// the Sandbox CR before the controller ever materialises a Pod.
	gkeSandboxRuntimeKey   = "sandbox.gke.io/runtime"
	gkeSandboxRuntimeValue = "gvisor"

	// agentSandboxMaxTTL is the controller-owned GC backstop written into the
	// Sandbox's spec.shutdownTime so a crashed orchestrator does not leak a
	// sandbox: the controller deletes the Sandbox (and cascades to the Pod) once
	// this absolute time passes. Close() is the normal teardown; this TTL only
	// fires if Close() never runs. It is deliberately generous so it never cuts a
	// live run short — a run that legitimately approaches 12h is far rarer than
	// an orchestrator crash, and the cost of a too-short TTL (killing a working
	// run) is worse than the cost of a too-long one (a leaked sandbox lingering a
	// few extra hours).
	agentSandboxMaxTTL = 12 * time.Hour
)

// agentSandboxDefaultCPU / agentSandboxDefaultMemory are the per-container
// limit+request defaults applied when the caller did not pin resources. GKE's
// secure-sandbox-policy requires explicit CPU and memory limits on every
// container in the Sandbox CR, so applyAgentSandboxAdmissionDeltas fills any gap
// with these values rather than letting the Sandbox be rejected for an absent
// limit. They are package vars (not consts) because resource.Quantity is not a
// constant-expressible type.
var (
	agentSandboxDefaultCPU    = resource.MustParse("500m")
	agentSandboxDefaultMemory = resource.MustParse("512Mi")
)

// AgentSandboxExecutor implements Executor by provisioning the sandbox Pod
// through GKE's Agent Sandbox controller: it creates an
// agents.x-k8s.io/v1alpha1 Sandbox custom resource and the controller
// materialises the backing Pod, rather than the executor creating a raw Pod
// itself (the K8sExecutor path). Once the Sandbox is Ready, command execution
// and file I/O ride the same pods/exec machinery as K8sExecutor via the
// embedded podExecCore.
//
// The two executors share everything below the provisioning layer: the
// hardened Pod spec (buildSandboxPodSpec), the per-Pod egress NetworkPolicy
// (egressPolicyFor / podLabels), and the exec/file-I/O core. They differ only
// in how the Pod comes into being — direct Pod create vs. the Sandbox CRD.
type AgentSandboxExecutor struct {
	podExecCore
	// dynamicClient drives the agents.x-k8s.io Sandbox CR (create / get / delete);
	// the clientset embedded in podExecCore handles pods/exec and the
	// NetworkPolicy.
	dynamicClient dynamic.Interface
	// sandboxName is the Sandbox CR's name. On the cold-create path it is also
	// the backing Pod's name (the controller names the Pod after the Sandbox);
	// the actual Pod name lives in podExecCore.podName, resolved at construction.
	sandboxName string
	// networkPolicyName is the egress NetworkPolicy installed alongside the
	// Sandbox. Close() deletes it best-effort after the Sandbox.
	networkPolicyName string
}

// applyAgentSandboxAdmissionDeltas mutates a base Pod spec (as produced by
// buildSandboxPodSpec) so the Sandbox CR it is embedded in satisfies GKE's
// secure-sandbox-policy ValidatingAdmissionPolicy. The base hardened spec
// already covers drop-ALL, runAsNonRoot, automountServiceAccountToken=false and
// the no-hostPath/hostNetwork/privileged requirements; this fills the four
// gvisor-specific gaps the policy additionally mandates:
//
//   - spec.runtimeClassName == "gvisor" (forced unconditionally — the Sandbox
//     path is gVisor-only, so any caller-set runtime is overridden here).
//   - explicit CPU and memory limits on every container. A container missing
//     either limit has BOTH that limit and the matching request filled with the
//     package default; a caller-set value is preserved (only gaps are filled).
//   - nodeSelector["sandbox.gke.io/runtime"] == "gvisor" (merged into any
//     existing selector, never clobbering caller entries).
//   - a toleration for the gVisor NoSchedule taint (appended idempotently).
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
		// DeepCopy the package-default Quantity before storing it: resource.Quantity
		// carries an internal *big.Int that a plain struct copy would alias, so a
		// later mutation of a stored limit could corrupt the shared default for the
		// next sandbox. Limits and Requests share one copy per container, matching
		// resourcesToPodResources.
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

	// Append the gVisor toleration only if an equivalent one is not already
	// present. A flag (rather than an early return) keeps this delta independent
	// of any future delta added after it.
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
// resource that the GKE Agent Sandbox controller reconciles into a Pod. The
// typed Pod spec is converted to the unstructured map the CR's
// spec.podTemplate.spec expects; the controller propagates podTemplate labels
// to the Pod (stripping only agents.x-k8s.io/* keys), so the per-pod
// stirrup.dev/pod label set by podLabels lands on the controller-created Pod
// and the egress NetworkPolicy binds to it.
//
// shutdownPolicy "Delete" + shutdownTime is the controller's GC backstop: it
// deletes the Sandbox at shutdownTime if Close() never runs (see
// agentSandboxMaxTTL).
func buildSandboxObject(namespace, name string, podSpec corev1.PodSpec, shutdownTime time.Time) (*metav1unstructured.Unstructured, error) {
	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podSpec)
	if err != nil {
		return nil, fmt.Errorf("convert pod spec to unstructured: %w", err)
	}

	// podLabels is the single source of truth for the per-pod label set
	// (stirrup-sandbox + stirrup.dev/pod). The unstructured tree wants
	// map[string]any, so widen the map[string]string here rather than rebuild
	// the labels inline and risk drift from the NetworkPolicy's selector.
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
	// Fail-closed: a nil Network leaves egress posture undefined. Rejecting it
	// here forces the caller to declare intent, matching NewK8sExecutor and
	// keeping CanNetwork honest against the installed policy.
	if cfg.Network == nil {
		return nil, fmt.Errorf("agent sandbox executor requires a network config (set executor.network.mode to \"none\" or \"allowlist\")")
	}

	// Compute the proxy env and validate the allowlist→URL requirement BEFORE
	// creating any cluster objects, so a misconfigured allowlist run fails
	// without leaving an orphaned Sandbox behind.
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

	// gVisor is the only runtime the Agent Sandbox path supports (GKE's
	// secure-sandbox-policy admits nothing else, and applyAgentSandboxAdmissionDeltas
	// forces it). Warn if the caller asked for something else so the override is
	// visible rather than silent.
	if cfg.RuntimeClassName != "" && cfg.RuntimeClassName != gkeSandboxRuntimeValue {
		logger.Warn(
			"agent sandbox executor: overriding executor.runtime with gvisor — the Agent Sandbox path is gVisor-only",
			"requested", cfg.RuntimeClassName,
		)
	}

	// The Sandbox name doubles as the cold-path Pod name: the controller names
	// the backing Pod after the Sandbox, and the per-pod egress NetworkPolicy /
	// podLabels select on that name.
	name, err := generatePodName()
	if err != nil {
		return nil, fmt.Errorf("generate sandbox name: %w", err)
	}

	spec := buildSandboxPodSpec(cfg, proxyEnv, cfg.ExtraEnv)
	applyAgentSandboxAdmissionDeltas(&spec)

	// Install the egress NetworkPolicy BEFORE creating the Sandbox, for the same
	// reason NewK8sExecutor does: the Pod name is client-side deterministic
	// (cold-path Pod name == Sandbox name) and the policy selects on
	// stirrup.dev/pod=<name>, so the policy can be in place before the
	// controller materialises the Pod. Installing after readiness would leave a
	// Running Pod with cluster-default egress in the Ready→policy window. A
	// failure to install is fatal.
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
			// Fail fast only on errors that will not resolve by retrying: an RBAC
			// denial means the orchestrator cannot read the Sandbox at all. Every
			// other class — a NotFound from read-after-write lag right after the
			// create, or a transient 5xx/throttle during the controller's
			// reconcile flurry (image pull, scheduling) — is retried until the
			// readiness deadline. The Sandbox path has a wider readiness window
			// than a raw Pod (controller watch + materialise), so a transient get
			// error is meaningfully more likely here than in waitForPodReady.
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
		// The duration is intentionally omitted: readiness can end on either
		// k8sReadyTimeout or a shorter caller-context deadline, and a hardcoded
		// "after 1m0s" would misreport the latter. The wrapped error carries the
		// cause.
		return nil, fmt.Errorf("sandbox %s/%s not ready: %w", cfg.Namespace, name, err)
	}

	podName := resolveSandboxPodName(ready)

	// v1 supports only the cold-create path, where the backing Pod is named after
	// the Sandbox and so carries the stirrup.dev/pod=<name> label the egress
	// NetworkPolicy selects on. A warm-pool-adopted Sandbox backs a Pod with a
	// different (random) name and without that label, so the policy would bind to
	// nothing and the Pod would run with unconfined egress — a silent fail-open.
	// Refuse it until warm-pool support installs the policy against the resolved
	// Pod name (or the controller-guaranteed sandbox-name-hash label).
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

// Close tears down the sandbox with its own background context (so cleanup
// proceeds even if the caller's context is already cancelled).
//
// Only the Sandbox is deleted — NOT the Pod or Service. The controller owns them
// via owner references and cascade-GCs them; deleting the Pod here would race
// that cascade. The delete uses Foreground propagation, so the Sandbox object
// lingers (with a deletion finalizer) until its dependent Pod is gone — which
// makes "Sandbox gone" a reliable signal that the Pod is gone too.
//
// The egress NetworkPolicy is removed only AFTER the Sandbox (and, via the
// foreground cascade, its Pod) is confirmed gone, so egress is never reopened for
// a still-running Pod — the teardown analogue of the constructor's
// policy-before-Pod ordering. The posture is fail-closed in three ways:
//   - If the Sandbox delete fails (the Pod may still run), the policy is LEFT in
//     place and the error returned; the shutdownTime TTL backstop still GCs the
//     Sandbox eventually.
//   - If the Sandbox does not disappear within the close timeout, the policy is
//     LEFT in place (logged). A leftover policy is inert once the Pod is gone and
//     still confines it if it somehow lingers.
//   - A NotFound Sandbox is treated as already-gone (idempotent); the policy is
//     then safe to remove.
func (e *AgentSandboxExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()

	foreground := metav1.DeletePropagationForeground
	sbErr := e.dynamicClient.Resource(sandboxGVR).Namespace(e.namespace).Delete(ctx, e.sandboxName, metav1.DeleteOptions{
		PropagationPolicy: &foreground,
	})
	if sbErr != nil && !apierrors.IsNotFound(sbErr) {
		// The Sandbox (and its Pod) may still be running; keep the egress policy in
		// place so the Pod stays confined.
		return fmt.Errorf("delete sandbox: %w", sbErr)
	}

	if e.networkPolicyName == "" {
		return nil
	}

	// Lift egress confinement only once the Sandbox is actually gone. A NotFound
	// from the delete means it was already gone; otherwise poll until a Get
	// reports NotFound (foreground propagation removes the object only after its
	// dependent Pod is deleted). Transient get errors are ignored — the poll keeps
	// waiting until the deadline.
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

// deleteSandboxBestEffort attempts to delete the Sandbox CR, logging (but not
// returning) any error. Used on constructor error paths to undo a Sandbox whose
// readiness wait failed, so a failed construction leaves no orphaned Sandbox
// (and, via the controller cascade, no orphaned Pod) behind.
func deleteSandboxBestEffort(dyn dynamic.Interface, namespace, name string, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()
	err := dyn.Resource(sandboxGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		logger.Warn("agent sandbox executor cleanup sandbox delete failed", "namespace", namespace, "sandbox", name, "err", err)
	}
}

var _ Executor = (*AgentSandboxExecutor)(nil)
