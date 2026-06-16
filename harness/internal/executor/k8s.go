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
	// k8sRunAsUserUID is the non-root UID enforced for the agent container.
	// 65532 matches the distroless "nonroot" convention and lets RunAsNonRoot
	// be enforced without requiring the image to declare its own USER.
	k8sRunAsUserUID int64 = 65532
	// k8sMaxOutput caps both exec output and file I/O at 10 MB. This mirrors
	// the container executor (maxDockerFrameSize / maxFileSize), NOT the
	// local executor's tighter 1 MB limit: a sandboxed Pod, like a
	// container, is the boundary that justifies the larger budget, whereas
	// the local executor shares the host filesystem and stays conservative.
	k8sMaxOutput int64 = 10 * 1024 * 1024
	// k8sFileIOTimeout bounds a single ReadFile/WriteFile/ListDirectory
	// operation. Exec clamps its own deadline; file I/O has no caller-supplied
	// timeout, and an established SPDY stream is not bounded by
	// rest.Config.Timeout, so a hung Pod would otherwise wedge a goroutine
	// indefinitely.
	k8sFileIOTimeout = 60 * time.Second
)

// errK8sOutputCap is returned when exec output or a file payload exceeds
// k8sMaxOutput. It is a sentinel so callers (and tests) can match on it
// without string comparison.
var errK8sOutputCap = errors.New("output exceeded 10 MB cap")

// K8sExecutorConfig configures a K8sExecutor. The type is wired through
// the executor factory under ExecutorConfig.Type == "k8s": ValidateRunConfig
// enforces the required Image and Namespace and rejects a non-empty
// workspace, and buildExecutor maps the run's ExecutorConfig onto these
// fields. RuntimeClassName maps from the shared Runtime field and Resources
// is applied to the Pod container (CPU/memory requests+limits, ephemeral
// storage limit; see resourcesToPodResources).
//
// Network is REQUIRED (fail-closed): NewK8sExecutor rejects a nil Network
// at construction, mirroring the container executor's explicit network
// posture. Mode=="none" installs a deny-all egress NetworkPolicy alongside
// the Pod; Mode=="allowlist" installs a policy permitting egress only to the
// egress proxy (plus DNS) and injects HTTP_PROXY/HTTPS_PROXY/NO_PROXY pointing
// at EgressProxyURL. Capabilities().CanNetwork reflects the installed policy.
//
// CAVEAT: NetworkPolicy enforcement depends on the cluster CNI. kindnet (the
// default kind CNI) accepts the policy object but does NOT enforce it, so on
// a stock kind cluster the deny/allowlist is inert and the Pod retains
// cluster-default egress. A NetworkPolicy-enforcing CNI (Cilium, Calico) is
// required for the posture to hold — see examples/k8s/egress-proxy/README.md.
// This is the k8s analogue of the container executor's honest fail-open note
// around host.docker.internal.
type K8sExecutorConfig struct {
	// Image is the container image for the agent Pod. It must ship a
	// POSIX shell at /bin/sh plus the `tar` and `ls` utilities on PATH:
	// command execution runs `/bin/sh -c`, and file I/O streams `tar`
	// over the pods/exec subresource (directory listings use `ls -A1`).
	// Distroless static images without a shell will not work.
	Image              string
	Namespace          string
	Kubeconfig         string
	NodeSelector       map[string]string
	RuntimeClassName   string
	ServiceAccountName string
	Resources          *types.ResourceLimits
	Network            *types.NetworkConfig
	// EgressProxyURL is the URL the sandbox container's HTTP_PROXY /
	// HTTPS_PROXY point at when Network.Mode == "allowlist". It is required
	// in that mode (a Pod with an enforced allowlist NetworkPolicy can reach
	// the network only via the proxy) and ignored for Mode == "none". The
	// proxy itself runs as a separate Deployment — see
	// examples/k8s/egress-proxy/ and the `stirrup egress-proxy` subcommand.
	//
	// The egress proxy Deployment MUST run in the same namespace as the
	// sandbox Pod: the allowlist NetworkPolicy selects the proxy by
	// PodSelector with no NamespaceSelector, so a cross-namespace proxy is
	// denied (more restrictive, not a bypass) and a confusing misconfig.
	EgressProxyURL string
	// Security, when non-nil, receives structured security events
	// (path-traversal blocks, file-size-limit and output-cap hits) so they
	// reach the same monitoring surface as the container and local
	// executors. The factory (issue #16) passes the run's emitter through.
	Security SecurityEventEmitter
}

// K8sExecutor implements Executor by running operations inside a sandbox
// Pod. The Pod is created on construction and deleted on Close().
//
// Command execution and file I/O both ride the pods/exec subresource:
// Exec runs `/bin/sh -c`, while ReadFile/WriteFile stream a tar archive
// over exec and ListDirectory runs `ls`. The image must therefore ship a
// shell, tar, and ls — see K8sExecutorConfig.Image.
type K8sExecutor struct {
	podExecCore
	// networkPolicyName is the name of the egress NetworkPolicy installed
	// alongside the Pod. Empty only on a zero-value executor (unit tests);
	// NewK8sExecutor always installs one. Close() deletes it best-effort.
	networkPolicyName string
}

// NewK8sExecutor builds a kubernetes clientset, installs the egress
// NetworkPolicy, creates a sandbox Pod, and waits for it to become Ready
// before returning. The policy is installed BEFORE the Pod so the Pod never
// runs with unrestricted egress (the Pod name is client-side deterministic,
// so the policy can target it ahead of time). On any failure the partially
// created objects (policy and/or Pod) are deleted best-effort before the
// error is returned, so callers do not have to reason about partial state.
func NewK8sExecutor(ctx context.Context, cfg K8sExecutorConfig) (*K8sExecutor, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("k8s executor requires an image")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("k8s executor requires a namespace")
	}
	// Fail-closed: a nil Network leaves egress posture undefined. Rejecting
	// it here (rather than defaulting to "deny" or "open") forces the caller
	// to declare intent, matching the container executor's explicit network
	// handling and keeping CanNetwork honest against an installed policy.
	if cfg.Network == nil {
		return nil, fmt.Errorf("k8s executor requires a network config (set executor.network.mode to \"none\" or \"allowlist\")")
	}
	// Compute the proxy env and validate the allowlist→URL requirement BEFORE
	// creating any cluster objects, so a misconfigured allowlist run fails
	// without leaving an orphaned Pod behind.
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

	// An empty RuntimeClassName leaves RuntimeClassName=nil on the Pod,
	// which selects the cluster-default RuntimeClass. That default may be
	// plain runc with no user-space-kernel or VM isolation, so the operator
	// is silently running unsandboxed. Warn so the opt-out is visible; a
	// caller wanting isolation must set executor.runtime explicitly.
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

	// Install the egress NetworkPolicy BEFORE creating the Pod. The Pod name
	// is client-side deterministic (generatePodName sets ObjectMeta.Name, not
	// GenerateName) and the policy selects on the stirrup.dev/pod=<name> +
	// stirrup-sandbox=true labels, both known here — so the policy can be in
	// place before the Pod exists. Installing AFTER readiness would leave a
	// Running Pod with cluster-default (unrestricted) egress in the
	// Ready→policy window, defeating the deny-all/allowlist posture. A failure
	// to install is fatal: a Pod whose egress is meant to be denied/allowlisted
	// but is not must never be created.
	//
	// proxyEnvFor already validated the mode above, so egressPolicyFor cannot
	// hit its default arm here.
	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, podName)
	if err != nil {
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("install egress NetworkPolicy for pod %s/%s: %w", cfg.Namespace, podName, err)
	}

	created, err := clientset.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// The policy is already installed; remove it so a failed construction
		// leaves no orphaned policy behind.
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		return nil, classifyPodCreateError(cfg.RuntimeClassName, err)
	}

	if err := waitForPodReady(ctx, clientset, cfg.Namespace, created.Name); err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		deleteNetworkPolicyBestEffort(clientset, cfg.Namespace, policy.Name, logger)
		// The duration is intentionally omitted: readiness can end on either
		// k8sReadyTimeout or a shorter caller-context deadline, and a
		// hardcoded "after 1m0s" would misreport the latter. The wrapped
		// error (context.DeadlineExceeded or a Get failure) carries the cause.
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
		// AutomountServiceAccountToken is always false in the scaffold:
		// issue #77 forbids token automount until later issues introduce
		// an explicit opt-in. This applies even when ServiceAccountName
		// was caller-provided.
		AutomountServiceAccountToken: ptr.To(false),
		NodeSelector:                 cfg.NodeSelector,
		RuntimeClassName:             optionalRuntimeClassName(cfg.RuntimeClassName),
		// FSGroup makes the /workspace emptyDir (below) group-owned by and
		// group-writable for the non-root UID the agent runs as. Without
		// it, a workspace directory the kubelet/runtime auto-creates is
		// root-owned and the UID-65532 container cannot write to it — every
		// WriteFile/mkdir would fail with EACCES. This is the k8s analogue
		// of the container executor's writable host bind mount.
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup: ptr.To(k8sRunAsUserUID),
		},
		// An ephemeral, writable workspace mounted at /workspace. The
		// executor provides this rather than requiring every sandbox image
		// to pre-create a /workspace owned by UID 65532: a minimal image
		// (busybox, distroless-with-shell) has no such directory, and one
		// auto-created at the mount point would be root-owned. Mounting an
		// emptyDir (wiped per Pod, good sandbox hygiene) with FSGroup above
		// guarantees the workspace is writable for any image that merely
		// ships a shell. This mirrors the container executor, which
		// bind-mounts a writable host directory at /workspace — both hide
		// any content an image happens to bake at that path.
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
// NetworkPolicy. It uses its own background context so cleanup proceeds even
// if the caller's context is already cancelled — matching the Docker
// executor's Close discipline. A NotFound result is treated as success
// (idempotent).
//
// Ordering matters: the Pod is deleted FIRST so kubelet begins terminating it
// immediately, then the policy is removed. Deleting the policy first would
// reopen unrestricted egress for the still-running Pod during termination —
// the same Ready→policy window the constructor closes by installing the
// policy first. The policy delete is best-effort: a leftover policy is inert
// once the Pod it selects is gone (its podSelector matches nothing), so a
// failed policy delete must not surface as a Close error or block Pod removal.
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
// message when the API server rejects the spec AND a non-empty
// RuntimeClass was requested. Two rejection shapes are covered:
//   - apierrors.IsInvalid — the built-in admission check for an
//     unregistered RuntimeClass ("...RuntimeClass.node.k8s.io \"gvisor\"
//     not found...").
//   - apierrors.IsForbidden — an admission webhook (OPA/Gatekeeper, or a
//     RuntimeClass-restricting policy) returning 403 for a RuntimeClass
//     the operator may not use.
//
// Both raw texts are opaque to an operator who simply typed a runtime name
// the cluster does not have or does not allow. The original error is
// wrapped (not replaced) so callers can still errors.As / errors.Is it.
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
// Pod container's ResourceRequirements. A nil limits (or an all-zero one)
// yields an empty requirements block so the Pod inherits namespace
// defaults rather than pinning a tiny request.
//
// Mapping rationale:
//   - CPUs go on BOTH requests and limits so the scheduler reserves what
//     the workload may use (no burst beyond the declared share). Fractional
//     cores render as milli-CPU ("500m" for 0.5); whole cores render as a
//     plain integer ("2") for readability.
//   - MemoryMB goes on both requests and limits (Mi) for the same reason:
//     a memory limit without a matching request lets the scheduler
//     over-commit the node.
//   - DiskMB maps to limits.ephemeral-storage ONLY. A request would force
//     the scheduler to find a node with that much free scratch space even
//     when the workload never fills it; the limit alone is the eviction
//     ceiling, which is the behaviour operators expect from a disk cap.
//   - PIDs is intentionally NOT enforced here. Pod-level PID limits are a
//     kubelet setting (--pod-max-pids / SupportPodPidsLimit), not a
//     per-container resource field, so the value is logged-and-ignored
//     rather than silently dropped.
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

// cpuQuantity renders a CPU share as a Kubernetes quantity. Whole cores
// become a plain integer ("2") and fractional cores become milli-CPU
// ("500m" for 0.5) so the emitted Pod spec is readable rather than an
// opaque scaled value. The milli-CPU rounding matches Kubernetes' own
// internal CPU granularity (1m = 0.001 core).
//
// A positive but sub-millicore share (e.g. 0.0004 core) rounds to zero
// millis. "0m" is rejected by resource.MustParse (Kubernetes forbids an
// explicitly-zero milli quantity), which would panic, so any positive
// input is clamped to a 1m floor. Callers only reach this with cpus > 0
// (resourcesToPodResources guards the zero/negative case), so the floor
// turns a would-be panic into the smallest representable reservation.
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
