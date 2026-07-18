package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

const (
	k8sWorkspace        = "/workspace"
	k8sWorkspaceVolume  = "workspace"
	k8sAgentContainer   = "agent"
	k8sReadyTimeout     = 60 * time.Second
	k8sReadyPollPeriod  = 1 * time.Second
	k8sCloseTimeout     = 30 * time.Second
	k8sAPITimeout       = 30 * time.Second
	k8sPodNameRandBytes = 6
	// k8sRunAsUserUID matches the distroless "nonroot" convention (65532),
	// letting RunAsNonRoot be enforced without the image declaring its own USER.
	k8sRunAsUserUID int64 = 65532
	// k8sMaxOutput mirrors the container executor's 10 MB cap, not the
	// local executor's tighter 1 MB limit: a sandboxed Pod justifies the
	// larger budget the way a container does.
	k8sMaxOutput int64 = 10 * 1024 * 1024
	// k8sFileIOTimeout bounds ReadFile/WriteFile/ListDirectory: an established
	// SPDY stream is not bounded by rest.Config.Timeout, so a hung Pod would
	// otherwise wedge a goroutine indefinitely.
	k8sFileIOTimeout = 60 * time.Second
)

// errK8sOutputCap is returned when exec output or a file payload exceeds
// k8sMaxOutput. It is a sentinel so callers (and tests) can match on it
// without string comparison.
var errK8sOutputCap = errors.New("output exceeded 10 MB cap")

// K8sExecutorConfig configures a K8sExecutor, wired through the executor
// factory under ExecutorConfig.Type == "k8s". See docs/executors/k8s.md for
// the network/egress posture and the kindnet enforcement caveat.
type K8sExecutorConfig struct {
	// Image must ship a POSIX shell at /bin/sh plus `tar` and `ls` on PATH:
	// command execution runs `/bin/sh -c`, and file I/O streams `tar` over
	// the pods/exec subresource.
	Image              string
	Namespace          string
	Kubeconfig         string
	NodeSelector       map[string]string
	RuntimeClassName   string
	ServiceAccountName string
	Resources          *types.ResourceLimits
	Network            *types.NetworkConfig
	// EgressProxyURL is required when Network.Mode == "allowlist" and
	// ignored otherwise. The proxy Deployment MUST run in the same
	// namespace as the sandbox Pod (see docs/executors/k8s.md).
	EgressProxyURL string
	// Security, when non-nil, receives structured security events so they
	// reach the same monitoring surface as the container and local
	// executors.
	Security SecurityEventEmitter
}

// K8sExecutor implements Executor by running operations inside a sandbox
// Pod, created on construction and deleted on Close(). Command execution
// and file I/O both ride the pods/exec subresource — see
// K8sExecutorConfig.Image for the required shell/tar/ls.
type K8sExecutor struct {
	podExecCore
	// networkPolicyName is empty only on a zero-value executor (unit
	// tests); NewK8sExecutor always installs one. Close() deletes it
	// best-effort.
	networkPolicyName string
}

// NewK8sExecutor builds a kubernetes clientset, installs the egress
// NetworkPolicy, creates a sandbox Pod, and waits for it to become Ready.
// The policy is installed BEFORE the Pod so the Pod never runs with
// unrestricted egress. On any failure the partially created objects are
// deleted best-effort before the error is returned.
func NewK8sExecutor(ctx context.Context, cfg K8sExecutorConfig) (*K8sExecutor, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("k8s executor requires an image")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("k8s executor requires a namespace")
	}
	// Fail-closed: a nil Network leaves egress posture undefined.
	if cfg.Network == nil {
		return nil, fmt.Errorf("k8s executor requires a network config (set executor.network.mode to \"none\" or \"allowlist\")")
	}
	// Validate the allowlist→URL requirement before creating any cluster
	// objects, so a misconfigured run fails without an orphaned Pod.
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

	logger := slog.Default()

	// An empty RuntimeClassName selects the cluster-default RuntimeClass,
	// which may be plain runc with no sandbox isolation. Warn so the
	// opt-out is visible.
	if cfg.RuntimeClassName == "" {
		logger.Warn(
			"k8s executor running with the cluster-default RuntimeClass; sandbox isolation is not guaranteed — set executor.runtime (e.g. gvisor) for a sandboxed RuntimeClass",
			"namespace", cfg.Namespace,
		)
	}

	podName, err := generatePodName()
	if err != nil {
		return nil, fmt.Errorf("generate pod name: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cfg.Namespace,
			Labels:    podLabels(podName),
		},
		Spec: buildSandboxPodSpec(cfg, proxyEnv),
	}

	// Install the egress NetworkPolicy BEFORE creating the Pod: installing
	// after readiness would leave a Running Pod with unrestricted egress in
	// the window between creation and policy install, defeating the
	// deny-all/allowlist posture. A failure to install is fatal.
	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, podName)
	if err != nil {
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("install egress NetworkPolicy for pod %s/%s: %w", cfg.Namespace, podName, err)
	}

	created, err := clientset.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {

		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		return nil, classifyPodCreateError(cfg.RuntimeClassName, err)
	}

	if err := waitForPodReady(ctx, clientset, cfg.Namespace, created.Name); err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)

		return nil, fmt.Errorf("pod %s/%s not ready: %w", cfg.Namespace, created.Name, err)
	}

	return &K8sExecutor{
		podExecCore: podExecCore{
			clientset:  clientset,
			restConfig: restCfg,
			namespace:  cfg.Namespace,
			podName:    created.Name,
			network:    cfg.Network,
			Security:   cfg.Security,
			logger:     logger,
		},
		networkPolicyName: policy.Name,
	}, nil
}

// buildSandboxPodSpec builds the hardened agent Pod spec shared by the k8s and
// k8s-sandbox executors. proxyEnv is the already-computed HTTP(S)_PROXY env.
func buildSandboxPodSpec(cfg K8sExecutorConfig, proxyEnv []corev1.EnvVar) corev1.PodSpec {
	serviceAccount := cfg.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = "default"
	}

	return corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		ServiceAccountName: serviceAccount,
		// AutomountServiceAccountToken is always false, even when
		// ServiceAccountName is caller-provided.
		AutomountServiceAccountToken: ptr.To(false),
		NodeSelector:                 cfg.NodeSelector,
		RuntimeClassName:             optionalRuntimeClassName(cfg.RuntimeClassName),
		// FSGroup makes the /workspace emptyDir group-writable for the
		// non-root UID the agent runs as; otherwise a kubelet-auto-created
		// workspace dir is root-owned and every WriteFile/mkdir fails EACCES.
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup: ptr.To(k8sRunAsUserUID),
		},
		// An ephemeral, writable workspace: a minimal sandbox image has no
		// pre-created /workspace, and one auto-created at the mount point
		// would be root-owned without FSGroup above.
		Volumes: []corev1.Volume{
			{
				Name:         k8sWorkspaceVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
		Containers: []corev1.Container{
			{
				Name:         k8sAgentContainer,
				Image:        cfg.Image,
				Command:      []string{"/bin/sh", "-c", "sleep infinity"},
				WorkingDir:   k8sWorkspace,
				Env:          proxyEnv,
				Resources:    resourcesToPodResources(cfg.Resources),
				VolumeMounts: []corev1.VolumeMount{{Name: k8sWorkspaceVolume, MountPath: k8sWorkspace}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
					RunAsNonRoot: ptr.To(true),
					// An explicit non-root UID lets RunAsNonRoot be enforced
					// without requiring the image to declare its own non-root
					// USER. 65532 matches the distroless convention.
					RunAsUser:      ptr.To(k8sRunAsUserUID),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			},
		},
	}
}

// Close deletes the sandbox Pod with zero grace period, then the egress
// NetworkPolicy, using its own background context so cleanup proceeds even
// if the caller's context is already cancelled. A NotFound result is
// treated as success (idempotent).
//
// The Pod is deleted first so the policy isn't removed while it is still
// terminating (which would reopen unrestricted egress); the policy delete
// is best-effort and never blocks Pod removal.
func (e *K8sExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()

	podErr := e.clientset.CoreV1().Pods(e.namespace).Delete(ctx, e.podName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	})

	if e.networkPolicyName != "" {
		npErr := e.clientset.NetworkingV1().NetworkPolicies(e.namespace).Delete(ctx, e.networkPolicyName, metav1.DeleteOptions{})
		if npErr != nil && !apierrors.IsNotFound(npErr) {
			e.logger.Warn("k8s executor networkpolicy delete failed", "namespace", e.namespace, "networkpolicy", e.networkPolicyName, "err", npErr)
		}
	}

	if podErr != nil {
		if apierrors.IsNotFound(podErr) {
			e.logger.Debug("k8s executor pod already gone", "namespace", e.namespace, "pod", e.podName)
			return nil
		}
		return fmt.Errorf("delete pod: %w", podErr)
	}
	return nil
}

// buildRESTConfig resolves an in-cluster config when possible, falling back
// to the kubeconfig file at cfg.Kubeconfig (or $KUBECONFIG if unset).
// The returned *rest.Config always carries an explicit HTTP timeout so the
// underlying client never inherits an unbounded timeout — the project bans
// HTTP clients without a declared timeout.
func buildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		cfg.Timeout = k8sAPITimeout
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		cfg.Timeout = k8sAPITimeout
		return cfg, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, err
	}
	kube := os.Getenv("KUBECONFIG")
	if kube == "" {
		return nil, fmt.Errorf("k8s executor: not in cluster and KUBECONFIG is unset")
	}
	cfg, err = clientcmd.BuildConfigFromFlags("", kube)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = k8sAPITimeout
	return cfg, nil
}

// classifyPodCreateError wraps a Pod-create failure with a friendlier
// message when the API server rejects the spec (IsInvalid or IsForbidden)
// AND a non-empty RuntimeClass was requested — the raw admission error is
// opaque to an operator who mistyped a runtime name. The original error is
// wrapped, not replaced, so callers can still errors.As / errors.Is it.
func classifyPodCreateError(runtimeClass string, err error) error {
	if runtimeClass != "" && (apierrors.IsInvalid(err) || apierrors.IsForbidden(err)) {
		return fmt.Errorf(
			"create pod: RuntimeClass %q was rejected by the cluster — confirm a RuntimeClass with that name is registered and permitted (kubectl get runtimeclass): %w",
			runtimeClass, err,
		)
	}
	return fmt.Errorf("create pod: %w", err)
}

// generatePodName returns "stirrup-<12-hex-chars>". 6 random bytes give 48
// bits of entropy — ample for collision avoidance within a namespace.
func generatePodName() (string, error) {
	buf := make([]byte, k8sPodNameRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "stirrup-" + hex.EncodeToString(buf), nil
}

// optionalRuntimeClassName returns a pointer to name, or nil if name is
// empty. The Pod spec field is a *string, and a non-nil empty string is
// rejected by the API server as an invalid RuntimeClass reference.
func optionalRuntimeClassName(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// resourcesToPodResources maps the harness-neutral ResourceLimits onto a
// Pod container's ResourceRequirements; see docs/executors/k8s.md for the
// field-by-field mapping. A nil or all-zero limits yields an empty
// requirements block so the Pod inherits namespace defaults.
func resourcesToPodResources(limits *types.ResourceLimits) corev1.ResourceRequirements {
	req := corev1.ResourceRequirements{}
	if limits == nil {
		return req
	}

	requests := corev1.ResourceList{}
	caps := corev1.ResourceList{}

	if limits.CPUs > 0 {
		cpu := cpuQuantity(limits.CPUs)
		requests[corev1.ResourceCPU] = cpu
		caps[corev1.ResourceCPU] = cpu
	}
	if limits.MemoryMB > 0 {
		mem := resource.MustParse(fmt.Sprintf("%dMi", limits.MemoryMB))
		requests[corev1.ResourceMemory] = mem
		caps[corev1.ResourceMemory] = mem
	}
	if limits.DiskMB > 0 {
		caps[corev1.ResourceEphemeralStorage] = resource.MustParse(fmt.Sprintf("%dMi", limits.DiskMB))
	}
	if limits.PIDs > 0 {
		slog.Default().Debug(
			"k8s executor: resources.pids ignored",
			"pids", limits.PIDs,
			"reason", "per-Pod PID limits require the kubelet --pod-max-pids flag, not a container resource field",
		)
	}

	if len(requests) > 0 {
		req.Requests = requests
	}
	if len(caps) > 0 {
		req.Limits = caps
	}
	return req
}

// cpuQuantity renders a CPU share as a Kubernetes quantity: whole cores as
// a plain integer ("2"), fractional cores as milli-CPU ("500m" for 0.5). A
// positive sub-millicore share rounds to zero millis, which resource.MustParse
// rejects (panics on "0m"), so any positive input is clamped to a 1m floor.
func cpuQuantity(cpus float64) resource.Quantity {
	if cpus == math.Trunc(cpus) {
		return resource.MustParse(fmt.Sprintf("%d", int64(cpus)))
	}
	milli := int64(math.Round(cpus * 1000))
	if milli < 1 {
		milli = 1
	}
	return resource.MustParse(fmt.Sprintf("%dm", milli))
}

// waitForPodReady polls until the Pod's phase is Running AND the agent
// container's Ready condition is true, or returns an error on timeout or
// ctx cancellation.
func waitForPodReady(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, k8sReadyPollPeriod, k8sReadyTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == k8sAgentContainer && cs.Ready {
				return true, nil
			}
		}
		return false, nil
	})
}

// deletePodBestEffort attempts to delete a Pod with zero grace, logging
// (but not returning) any error. Used on error paths inside the
// constructor where the original error is what the caller cares about.
func deletePodBestEffort(clientset kubernetes.Interface, namespace, name string, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()
	err := clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		logger.Warn("k8s executor cleanup delete failed", "namespace", namespace, "pod", name, "err", err)
	}
}

// deleteNetworkPolicyBestEffort attempts to delete the egress NetworkPolicy,
// logging (but not returning) any error. Used on constructor error paths to
// undo a policy that was installed before the Pod-create / readiness step
// failed, so a failed construction leaves no orphaned policy behind.
func deleteNetworkPolicyBestEffort(clientset kubernetes.Interface, namespace, name string, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()
	err := clientset.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		logger.Warn("k8s executor cleanup networkpolicy delete failed", "namespace", namespace, "networkpolicy", name, "err", err)
	}
}

var _ Executor = (*K8sExecutor)(nil)
