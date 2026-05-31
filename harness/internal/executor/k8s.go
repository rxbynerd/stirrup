package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
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
	// k8sMaxOutput caps both exec output and file I/O at 10 MB, mirroring the
	// container executor's maxDockerFrameSize / maxFileSize discipline.
	k8sMaxOutput int64 = 10 * 1024 * 1024
)

// errK8sOutputCap is returned when exec output or a file payload exceeds
// k8sMaxOutput. It is a sentinel so callers (and tests) can match on it
// without string comparison.
var errK8sOutputCap = errors.New("output exceeded 10 MB cap")

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
}

// K8sExecutor implements Executor by running operations inside a sandbox
// Pod. The Pod is created on construction and deleted on Close().
//
// Command execution and file I/O both ride the pods/exec subresource:
// Exec runs `/bin/sh -c`, while ReadFile/WriteFile stream a tar archive
// over exec and ListDirectory runs `ls`. The image must therefore ship a
// shell, tar, and ls — see K8sExecutorConfig.Image.
type K8sExecutor struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
	podName    string
	network    *types.NetworkConfig
	logger     *slog.Logger
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
		clientset:  clientset,
		restConfig: restCfg,
		namespace:  cfg.Namespace,
		podName:    created.Name,
		network:    cfg.Network,
		logger:     logger,
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

// ReadFile streams the file out of the Pod with `tar -cf - <path>` over the
// exec subresource and reads the single archived entry. A missing file maps
// to fs.ErrNotExist; a directory target is rejected. Content is capped at
// 10 MB.
func (e *K8sExecutor) ReadFile(ctx context.Context, filePath string) (string, error) {
	resolved, err := e.ResolvePath(filePath)
	if err != nil {
		return "", err
	}

	var stdout writeCapBuffer
	stdout.limit = k8sMaxOutput
	var stderr bytes.Buffer

	// `tar -C / -cf - <abs-path-without-leading-slash>` archives the single
	// file. Stripping the leading slash keeps tar from warning about
	// "removing leading /" on stderr and yields a predictable archive name.
	arcPath := strings.TrimPrefix(resolved, "/")
	execErr := e.streamExec(ctx, []string{"tar", "-C", "/", "-cf", "-", arcPath}, nil, &stdout, &stderr)
	if stdout.exceeded {
		return "", errK8sOutputCap
	}
	if execErr != nil {
		code, ok := extractExitCode(execErr)
		if ok && code != 0 {
			return "", classifyTarError(filePath, stderr.String())
		}
		return "", execErr
	}

	tr := tar.NewReader(bytes.NewReader(stdout.Bytes()))
	header, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read file %s: %w", filePath, fs.ErrNotExist)
	}
	if err != nil {
		return "", fmt.Errorf("read tar header: %w", err)
	}
	if header.Typeflag == tar.TypeDir {
		return "", fmt.Errorf("read file %s: is a directory", filePath)
	}
	if header.Size > k8sMaxOutput {
		return "", errK8sOutputCap
	}

	data, err := io.ReadAll(io.LimitReader(tr, k8sMaxOutput+1))
	if err != nil {
		return "", fmt.Errorf("read file from tar: %w", err)
	}
	if int64(len(data)) > k8sMaxOutput {
		return "", errK8sOutputCap
	}
	return string(data), nil
}

// WriteFile streams a one-entry tar archive into the Pod via
// `tar -C <dir> -xf -`, creating the file at filePath with mode 0644.
// Parent directories are created first with `mkdir -p`. Content is capped
// at 10 MB.
func (e *K8sExecutor) WriteFile(ctx context.Context, filePath string, content string) error {
	if int64(len(content)) > k8sMaxOutput {
		return errK8sOutputCap
	}

	resolved, err := e.ResolvePath(filePath)
	if err != nil {
		return err
	}

	dir := path.Dir(resolved)
	var mkOut, mkErr bytes.Buffer
	if mkErrRun := e.streamExec(ctx, []string{"mkdir", "-p", dir}, nil, &mkOut, &mkErr); mkErrRun != nil {
		if code, ok := extractExitCode(mkErrRun); ok && code != 0 {
			return fmt.Errorf("create parent directory %s: %s", dir, strings.TrimSpace(mkErr.String()))
		}
		return fmt.Errorf("create parent directory %s: %w", dir, mkErrRun)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{
		Name: path.Base(resolved),
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	var xOut, xErr bytes.Buffer
	if err := e.streamExec(ctx, []string{"tar", "-C", dir, "-xf", "-"}, &archive, &xOut, &xErr); err != nil {
		if code, ok := extractExitCode(err); ok && code != 0 {
			return fmt.Errorf("write file %s: %s", filePath, strings.TrimSpace(xErr.String()))
		}
		return fmt.Errorf("write file %s: %w", filePath, err)
	}
	return nil
}

// ListDirectory lists directory entries inside the Pod with `ls -A1`, which
// emits one name per line excluding "." and "..". A missing directory maps
// to fs.ErrNotExist.
func (e *K8sExecutor) ListDirectory(ctx context.Context, dirPath string) ([]string, error) {
	resolved, err := e.ResolvePath(dirPath)
	if err != nil {
		return nil, err
	}

	var stdout writeCapBuffer
	stdout.limit = k8sMaxOutput
	var stderr bytes.Buffer

	execErr := e.streamExec(ctx, []string{"ls", "-A1", resolved}, nil, &stdout, &stderr)
	if stdout.exceeded {
		return nil, errK8sOutputCap
	}
	if execErr != nil {
		if code, ok := extractExitCode(execErr); ok && code != 0 {
			return nil, classifyTarError(dirPath, stderr.String())
		}
		return nil, execErr
	}

	var entries []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "." || line == ".." {
			continue
		}
		entries = append(entries, line)
	}
	return entries, nil
}

// Exec runs `command` via `/bin/sh -c` inside the agent container over the
// pods/exec subresource. stdout and stderr are captured into separate
// 10 MB-capped buffers. A zero timeout uses the default; timeouts are
// clamped to MaxTimeout. On deadline the underlying context error is
// returned verbatim. The exit code is extracted from the remotecommand
// CodeExitError; a clean exit yields code 0.
func (e *K8sExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr writeCapBuffer
	stdout.limit = k8sMaxOutput
	stderr.limit = k8sMaxOutput

	err := e.streamExec(ctx, []string{"/bin/sh", "-c", command}, nil, &stdout, &stderr)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errK8sOutputCap
	}

	exitCode := 0
	if err != nil {
		code, ok := extractExitCode(err)
		if !ok {
			return nil, fmt.Errorf("exec: %w", err)
		}
		exitCode = code
	}

	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// streamExec builds and runs a pods/exec request against the agent
// container, wiring the supplied stdin/stdout/stderr streams. It is the
// single SPDY/remotecommand chokepoint shared by Exec and the tar-based
// file I/O methods. A nil stdin omits the stdin stream from the request.
func (e *K8sExecutor) streamExec(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := e.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(e.podName).
		Namespace(e.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: k8sAgentContainer,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build spdy executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// extractExitCode pulls the process exit code out of an error returned by
// remotecommand.StreamWithContext. A non-zero command exit surfaces as a
// utilexec.CodeExitError (matched here via the exported ExitError interface
// to stay robust across client-go versions and value/pointer wrapping). The
// boolean reports whether the error carried an exit status at all; a
// transport-level error (no exit status) returns (0, false). A nil error is
// treated as a clean exit (0, true). This helper is pure and unit-testable
// without a cluster.
func extractExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), true
	}
	return 0, false
}

// classifyTarError maps the stderr of a failed tar/ls invocation to a
// structured error. "No such file or directory" becomes fs.ErrNotExist so
// callers can branch with errors.Is; anything else is surfaced verbatim.
func classifyTarError(targetPath, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if strings.Contains(trimmed, "No such file or directory") || strings.Contains(trimmed, "not found") {
		return fmt.Errorf("%s: %w", targetPath, fs.ErrNotExist)
	}
	if trimmed == "" {
		return fmt.Errorf("operation on %s failed", targetPath)
	}
	return fmt.Errorf("operation on %s failed: %s", targetPath, trimmed)
}

// writeCapBuffer is an io.Writer that buffers up to limit bytes. Once the
// limit is reached it stops appending and sets exceeded, so a hostile or
// runaway command cannot grow the buffer without bound. The mirror of the
// container executor's frame-size cap, adapted to the streaming Writer that
// remotecommand expects.
type writeCapBuffer struct {
	buf      bytes.Buffer
	limit    int64
	exceeded bool
}

func (w *writeCapBuffer) Write(p []byte) (int, error) {
	if w.exceeded {
		// Claim the whole slice as written so the SPDY stream keeps
		// draining rather than erroring mid-flight; the cap is reported
		// to the caller via the exceeded flag after the stream closes.
		return len(p), nil
	}
	remaining := w.limit - int64(w.buf.Len())
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.exceeded = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *writeCapBuffer) Bytes() []byte  { return w.buf.Bytes() }
func (w *writeCapBuffer) String() string { return w.buf.String() }

// TODO(#178): CanNetwork's accuracy depends on a NetworkPolicy
// being enforced for the Pod. Until that wiring lands, a Pod with
// cfg.Network == nil reports CanNetwork=false but has cluster-default
// egress in reality. Tracked in #178.
//
// Capabilities advertises the executor's capabilities. CanNetwork is
// derived from the held NetworkConfig so callers can branch on the
// declared policy even while egress enforcement is deferred.
//
// MaxTimeout deliberately mirrors container.go and local.go (maxTimeout =
// 5 min). Returning 0 would silently disable timeout clamping in callers
// that compare against MaxTimeout — and Exec clamps against it. The cap is
// identical across executors so a caller written against the Executor
// interface clamps uniformly regardless of which implementation is active.
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
