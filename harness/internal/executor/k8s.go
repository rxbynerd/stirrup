package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

// K8sExecutorConfig configures a K8sExecutor. The container runtime,
// resources, and network policy fields are accepted but not yet enforced
// in this scaffold — the full mapping arrives in follow-up issues. The
// scaffold's job is the create/wait/delete lifecycle only.
//
// This type is intentionally not wired into ExecutorConfig.Type or the
// executor factory yet; that wiring lands in the follow-up factory issue.
// When it does, ValidateRunConfig must grow a "k8s" arm that validates
// the required Image and Namespace fields (mirroring the "container" arm),
// and the factory must add a corresponding "k8s" Type case.
type K8sExecutorConfig struct {
	Image              string
	Namespace          string
	Kubeconfig         string
	NodeSelector       map[string]string
	RuntimeClassName   string
	ServiceAccountName string
	Resources          *types.ResourceLimits
	Network            *types.NetworkConfig
}

// K8sExecutor implements Executor by running operations inside a sandbox
// Pod. The Pod is created on construction and deleted on Close().
//
// File I/O and command exec are stubbed in this scaffold and return
// "not implemented"; later issues wire them through the Pod's exec
// subresource and file-archive endpoints.
type K8sExecutor struct {
	clientset kubernetes.Interface
	namespace string
	podName   string
	network   *types.NetworkConfig
	logger    *slog.Logger
}

// NewK8sExecutor builds a kubernetes clientset, creates a sandbox Pod, and
// waits for it to become Ready before returning. On any failure after the
// Pod is created (including readiness-poll failures) the Pod is deleted
// best-effort before the error is returned, so callers do not have to
// reason about partial state.
func NewK8sExecutor(ctx context.Context, cfg K8sExecutorConfig) (*K8sExecutor, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("k8s executor requires an image")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("k8s executor requires a namespace")
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

	podName, err := generatePodName()
	if err != nil {
		return nil, fmt.Errorf("generate pod name: %w", err)
	}

	serviceAccount := cfg.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = "default"
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cfg.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: serviceAccount,
			// AutomountServiceAccountToken is always false in the scaffold:
			// issue #77 forbids token automount until later issues introduce
			// an explicit opt-in. This applies even when ServiceAccountName
			// was caller-provided.
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 cfg.NodeSelector,
			RuntimeClassName:             optionalRuntimeClassName(cfg.RuntimeClassName),
			Containers: []corev1.Container{
				{
					Name:       k8sAgentContainer,
					Image:      cfg.Image,
					Command:    []string{"/bin/sh", "-c", "sleep infinity"},
					WorkingDir: k8sWorkspace,
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
		},
	}

	created, err := clientset.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}

	if err := waitForPodReady(ctx, clientset, cfg.Namespace, created.Name); err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		return nil, fmt.Errorf("pod %s/%s not ready after %s: %w", cfg.Namespace, created.Name, k8sReadyTimeout, err)
	}

	return &K8sExecutor{
		clientset: clientset,
		namespace: cfg.Namespace,
		podName:   created.Name,
		network:   cfg.Network,
		logger:    logger,
	}, nil
}

// Close deletes the sandbox Pod with zero grace period. It uses its own
// background context so cleanup proceeds even if the caller's context is
// already cancelled — matching the Docker executor's Close discipline.
// A NotFound result is treated as success (idempotent).
func (e *K8sExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()

	err := e.clientset.CoreV1().Pods(e.namespace).Delete(ctx, e.podName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			e.logger.Debug("k8s executor pod already gone", "namespace", e.namespace, "pod", e.podName)
			return nil
		}
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

// ResolvePath validates that the given path does not escape the Pod
// workspace. The check is purely textual — there is no local filesystem
// to evalsymlinks against, since the workspace lives inside the Pod.
func (e *K8sExecutor) ResolvePath(relativePath string) (string, error) {
	var resolved string
	if path.IsAbs(relativePath) {
		resolved = path.Clean(relativePath)
	} else {
		resolved = path.Join(k8sWorkspace, relativePath)
	}

	if resolved != k8sWorkspace && !strings.HasPrefix(resolved, k8sWorkspace+"/") {
		return "", fmt.Errorf("path escapes workspace: %s", relativePath)
	}
	return resolved, nil
}

// ReadFile is not yet implemented — see #77 follow-up issues.
// TODO: flip Capabilities().CanRead to false (and back to true here when
// implemented) once the file-I/O follow-up lands.
func (e *K8sExecutor) ReadFile(ctx context.Context, filePath string) (string, error) {
	return "", errors.New("not implemented")
}

// WriteFile is not yet implemented — see #77 follow-up issues.
// TODO: flip Capabilities().CanWrite to false (and back to true here when
// implemented) once the file-I/O follow-up lands.
func (e *K8sExecutor) WriteFile(ctx context.Context, filePath string, content string) error {
	return errors.New("not implemented")
}

// ListDirectory is not yet implemented — see #77 follow-up issues.
// TODO: flip Capabilities().CanRead to false (and back to true here when
// implemented) once the file-I/O follow-up lands.
func (e *K8sExecutor) ListDirectory(ctx context.Context, dirPath string) ([]string, error) {
	return nil, errors.New("not implemented")
}

// Exec is not yet implemented — see #77 follow-up issues.
// TODO: flip Capabilities().CanExec to false (and back to true here when
// implemented) once the exec-subresource follow-up lands.
func (e *K8sExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	return nil, errors.New("not implemented")
}

// TODO(#178): CanNetwork's accuracy depends on a NetworkPolicy
// being enforced for the Pod. Until that wiring lands, a Pod with
// cfg.Network == nil reports CanNetwork=false but has cluster-default
// egress in reality. Tracked in #178.
//
// Capabilities advertises the scaffold's intended capabilities. CanNetwork
// is derived from the held NetworkConfig so callers can branch on the
// declared policy even while egress enforcement is deferred.
//
// MaxTimeout deliberately mirrors container.go and local.go (maxTimeout =
// 5 min) instead of the value of 0 in the original scaffold spec. Returning
// 0 would silently disable timeout clamping in callers that compare against
// MaxTimeout — a footgun once Exec lands. The cap is identical across
// executors so a caller written against the Executor interface clamps
// uniformly regardless of which implementation is active.
func (e *K8sExecutor) Capabilities() ExecutorCapabilities {
	canNetwork := e.network != nil && e.network.Mode != "none"
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: canNetwork,
		MaxTimeout: maxTimeout,
	}
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

var _ Executor = (*K8sExecutor)(nil)
