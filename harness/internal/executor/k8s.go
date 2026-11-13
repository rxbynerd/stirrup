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
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	k8sWorkspace       = "/workspace"
	k8sWorkspaceVolume = "workspace"
	k8sAgentContainer  = "agent"
	// k8sSandboxIdentityVolume backs SandboxIdentityTokenDir when
	// K8sExecutorConfig.SandboxIdentityToken is set; the size limit mirrors
	// the container executor's tmpfs and counts against the Pod's memory.
	k8sSandboxIdentityVolume     = "sandbox-identity"
	k8sSandboxIdentityVolumeSize = "64Ki"
	// k8sReadyTimeout covers a Pod that waits for an autoscaler to provision
	// a node, still capped by the caller's context.
	k8sReadyTimeout     = 5 * time.Minute
	k8sReadyPollPeriod  = 1 * time.Second
	k8sCloseTimeout     = 30 * time.Second
	k8sAPITimeout       = 30 * time.Second
	k8sPodNameRandBytes = 6
	// k8sTerminationGracePeriod is the sandbox Pod's termination grace
	// period and the grace period the k8s executor deletes it with. It is
	// non-zero so the API object outlives the workload until the kubelet
	// reports the containers stopped, and short because the "sleep infinity"
	// entrypoint ignores SIGTERM and has nothing to flush.
	k8sTerminationGracePeriod int64 = 1
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
	// k8sClusterTextMax bounds a cluster-supplied string (a status message,
	// an API error) quoted in an error.
	k8sClusterTextMax = 256
)

// errK8sOutputCap is returned when exec output or a file payload exceeds
// k8sMaxOutput. It is a sentinel so callers (and tests) can match on it
// without string comparison.
var errK8sOutputCap = errors.New("output exceeded 10 MB cap")

// K8sExecutorConfig configures a K8sExecutor, wired through the executor
// factory under ExecutorConfig.Type == "k8s". See docs/executors/k8s.md for
// the network/egress posture and the kindnet enforcement caveat.
type K8sExecutorConfig struct {
	// Image must ship a POSIX shell at /bin/sh plus `tar`, `ls`, `mkdir`,
	// and `readlink` on PATH: command execution runs `/bin/sh -c`, file I/O
	// streams `tar` over the pods/exec subresource, and workspace reads and
	// writes resolve the real path with `readlink -f` first.
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
	// ExtraEnv is appended after the HTTP(S)_PROXY/NO_PROXY entries
	// (proxyEnv). Populated by the factory from a sandbox identity token
	// exchange; empty otherwise.
	ExtraEnv []EnvPair
	// SandboxIdentityToken adds the memory-backed volume behind
	// SandboxIdentityTokenDir so WriteSandboxIdentityToken has somewhere
	// outside the workspace to deliver tokens. Set by the factory alongside
	// ExtraEnv when executor.sandboxIdentity is configured.
	SandboxIdentityToken bool
	// Lifecycle stamps the run ID on the sandbox Pod and NetworkPolicy and,
	// for the k8s executor, sets the Pod's activeDeadlineSeconds from
	// MaxLifetime.
	Lifecycle Lifecycle
	// HarnessPod, when set and in Namespace, makes the sandbox Pod owned by
	// the harness's own pod so deleting that pod garbage-collects the
	// sandbox. Only the k8s executor honours it.
	HarnessPod *HarnessPod
}

// K8sExecutor implements Executor by running operations inside a sandbox
// Pod, created on construction and deleted on Close(). Command execution
// and file I/O both ride the pods/exec subresource — see
// K8sExecutorConfig.Image for the required shell/tar/ls.
type K8sExecutor struct {
	podExecCore
	podUID k8stypes.UID
	// networkPolicyName is empty only on a zero-value executor (unit
	// tests); NewK8sExecutor always installs one. Close() deletes it once
	// the Pod's termination is established.
	networkPolicyName string
	runID             string
	timing            k8sTiming

	// closeMu serialises Close; closed records a completed teardown so later
	// calls return without touching the cluster.
	closeMu sync.Mutex
	closed  bool
}

// NewK8sExecutor builds a kubernetes clientset, installs the egress
// NetworkPolicy, creates a sandbox Pod, checks what admission did to it, and
// waits for it to become Ready. The policy is installed BEFORE the Pod so the
// Pod never runs with unrestricted egress. Any failure after the Pod create
// tears down through the same contract as Close.
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

	return newK8sExecutor(ctx, cfg, proxyEnv, clientset, restCfg, defaultK8sTiming, slog.Default())
}

// newK8sExecutor is NewK8sExecutor after the clientset is built.
func newK8sExecutor(ctx context.Context, cfg K8sExecutorConfig, proxyEnv []corev1.EnvVar, clientset kubernetes.Interface, restCfg *rest.Config, timing k8sTiming, logger *slog.Logger) (*K8sExecutor, error) {

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

	pod := buildSandboxPod(cfg, podName, proxyEnv)

	// Install the egress NetworkPolicy BEFORE creating the Pod: installing
	// after readiness would leave a Running Pod with unrestricted egress in
	// the window between creation and policy install, defeating the
	// deny-all/allowlist posture. A failure to install is fatal.
	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, podName, cfg.Lifecycle.RunID)
	if err != nil {
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("install egress NetworkPolicy for pod %s/%s: %w", cfg.Namespace, podName, boundClusterErr(err))
	}

	teardown := podTeardown{
		clientset:  clientset,
		namespace:  cfg.Namespace,
		podName:    podName,
		policyName: policy.Name,
		runID:      cfg.Lifecycle.RunID,
		logger:     logger,
		timing:     timing,
	}

	created, err := clientset.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, withTeardownErr(classifyPodCreateError(cfg.RuntimeClassName, err), teardown.reconcileFailedCreate(pod.Labels, err))
	}
	teardown.podUID = created.UID

	admission := newPodAdmission(pod, created)
	if err := admission.checkCreate(); err != nil {
		return nil, withTeardownErr(err, teardown.run())
	}
	if cfg.RuntimeClassName == "" && created.Spec.RuntimeClassName != nil {
		logger.Info("k8s executor pod admitted with a cluster-assigned RuntimeClass",
			"namespace", cfg.Namespace, "pod", podName, "runtimeClassName", clusterText(*created.Spec.RuntimeClassName))
	}

	if _, err := waitForPodReady(ctx, clientset, cfg.Namespace, podName, admission, timing, logger); err != nil {
		return nil, withTeardownErr(fmt.Errorf("pod %s/%s: %w", cfg.Namespace, podName, err), teardown.run())
	}

	return &K8sExecutor{
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
		podUID:            created.UID,
		networkPolicyName: policy.Name,
		runID:             cfg.Lifecycle.RunID,
		timing:            timing,
	}, nil
}

// withTeardownErr appends a cleanup failure to the error that caused the
// cleanup, keeping both reachable through errors.Is and errors.As.
func withTeardownErr(cause, teardownErr error) error {
	if teardownErr == nil {
		return cause
	}
	return fmt.Errorf("%w; cleanup: %w", cause, teardownErr)
}

// buildSandboxPod builds the k8s executor's sandbox Pod. Beyond the shared
// spec it carries the run-ID label, the lifetime bound as
// activeDeadlineSeconds, and an owner reference to the harness pod when that
// pod shares the sandbox's namespace.
func buildSandboxPod(cfg K8sExecutorConfig, podName string, proxyEnv []corev1.EnvVar) *corev1.Pod {
	spec := buildSandboxPodSpec(cfg, proxyEnv, cfg.ExtraEnv)
	if secs, ok := cfg.Lifecycle.seconds(); ok {
		spec.ActiveDeadlineSeconds = &secs
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       cfg.Namespace,
			Labels:          podLabels(podName, cfg.Lifecycle.RunID),
			OwnerReferences: cfg.HarnessPod.ownerReferences(cfg.Namespace),
		},
		Spec: spec,
	}
}

// buildSandboxPodSpec builds the hardened agent Pod spec shared by the k8s and
// k8s-sandbox executors. proxyEnv is the already-computed HTTP(S)_PROXY env;
// extraEnv is appended after it: a sandbox identity token and, when
// configured, the git-proxy GIT_CONFIG_* pairs.
func buildSandboxPodSpec(cfg K8sExecutorConfig, proxyEnv []corev1.EnvVar, extraEnv []EnvPair) corev1.PodSpec {
	serviceAccount := cfg.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = "default"
	}

	env := proxyEnv
	if len(extraEnv) > 0 {
		env = make([]corev1.EnvVar, 0, len(proxyEnv)+len(extraEnv))
		env = append(env, proxyEnv...)
		for _, e := range extraEnv {
			env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
		}
	}

	volumes := []corev1.Volume{
		{
			Name:         k8sWorkspaceVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{{Name: k8sWorkspaceVolume, MountPath: k8sWorkspace}}
	if cfg.SandboxIdentityToken {
		// Memory medium keeps the token off node disk; FSGroup below makes
		// the directory writable by the non-root UID, as for /workspace.
		volumes = append(volumes, corev1.Volume{
			Name: k8sSandboxIdentityVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: ptr.To(resource.MustParse(k8sSandboxIdentityVolumeSize)),
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: k8sSandboxIdentityVolume, MountPath: SandboxIdentityTokenDir})
	}

	return corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: ptr.To(k8sTerminationGracePeriod),
		ServiceAccountName:            serviceAccount,
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
		Volumes: volumes,
		Containers: []corev1.Container{
			{
				Name:         k8sAgentContainer,
				Image:        cfg.Image,
				Command:      []string{"/bin/sh", "-c", "sleep infinity"},
				WorkingDir:   k8sWorkspace,
				Env:          env,
				Resources:    resourcesToPodResources(cfg.Resources),
				VolumeMounts: mounts,
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

// Close deletes the sandbox Pod gracefully under a UID precondition, waits
// until its termination is established, and only then deletes the egress
// NetworkPolicy. It uses its own bounded context, so cleanup proceeds even if
// the caller's context is cancelled. When termination cannot be established
// the policy is retained and the error names it; a later Close retries, and
// one after a successful teardown does nothing.
func (e *K8sExecutor) Close() error {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	if e.closed {
		return nil
	}
	timing := e.timing
	if timing == (k8sTiming{}) {
		timing = defaultK8sTiming
	}
	err := podTeardown{
		clientset:  e.clientset,
		namespace:  e.namespace,
		podName:    e.podName,
		podUID:     e.podUID,
		policyName: e.networkPolicyName,
		runID:      e.runID,
		logger:     e.logger,
		timing:     timing,
	}.run()
	e.closed = err == nil
	return err
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

// classifyPodCreateError wraps a Pod-create failure with its message
// length-capped, adding a RuntimeClass hint only when the response names the
// requested class (see createErrorNamesRuntimeClass). The original error stays
// reachable through errors.As.
func classifyPodCreateError(runtimeClass string, err error) error {
	if runtimeClass != "" && createErrorNamesRuntimeClass(runtimeClass, err) {
		return fmt.Errorf(
			"create pod: RuntimeClass %q was rejected by the cluster — confirm a RuntimeClass with that name is registered and permitted (kubectl get runtimeclass): %w",
			runtimeClass, boundClusterErr(err),
		)
	}
	return fmt.Errorf("create pod: %w", boundClusterErr(err))
}

// createErrorNamesRuntimeClass matches an Invalid cause on
// spec.runtimeClassName, or the RuntimeClass admission plugin's Forbidden
// "RuntimeClass %q not found"; Forbidden responses carry no field causes.
func createErrorNamesRuntimeClass(runtimeClass string, err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	st := status.Status()
	switch {
	case apierrors.IsInvalid(err):
		if st.Details == nil {
			return false
		}
		for _, cause := range st.Details.Causes {
			if cause.Field == "spec.runtimeClassName" {
				return true
			}
		}
		return false
	case apierrors.IsForbidden(err):
		return strings.Contains(st.Message, fmt.Sprintf("RuntimeClass %q not found", runtimeClass))
	}
	return false
}

// clusterError bounds the text of an API error while keeping the error
// itself reachable through errors.Is and errors.As.
type clusterError struct{ err error }

func (e clusterError) Error() string { return clusterText(e.err.Error()) }
func (e clusterError) Unwrap() error { return e.err }

// boundClusterErr wraps err in a clusterError; nil stays nil.
func boundClusterErr(err error) error {
	if err == nil {
		return nil
	}
	return clusterError{err: err}
}

// clusterText bounds a cluster-supplied string for inclusion in an error:
// secrets are scrubbed first, so truncation cannot cut one past recognition,
// then control characters are dropped and the length is capped.
func clusterText(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, security.Scrub(s))
	if runes := []rune(cleaned); len(runes) > k8sClusterTextMax {
		return string(runes[:k8sClusterTextMax]) + "…"
	}
	return cleaned
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

var _ Executor = (*K8sExecutor)(nil)
