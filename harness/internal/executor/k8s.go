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
	"math"
	"os"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
	podName    string
	network    *types.NetworkConfig
	// networkPolicyName is the name of the egress NetworkPolicy installed
	// alongside the Pod. Empty only on a zero-value executor (unit tests);
	// NewK8sExecutor always installs one. Close() deletes it best-effort.
	networkPolicyName string
	logger            *slog.Logger
	// Security, when non-nil, receives structured security events. It is
	// nil-checked at every call site so a zero-value executor (used in
	// unit tests) emits nothing.
	Security SecurityEventEmitter
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

	serviceAccount := cfg.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = "default"
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cfg.Namespace,
			Labels:    podLabels(podName),
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
					Env:        proxyEnv,
					Resources:  resourcesToPodResources(cfg.Resources),
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
		return nil, classifyPodCreateError(cfg.RuntimeClassName, err)
	}

	if err := waitForPodReady(ctx, clientset, cfg.Namespace, created.Name); err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		// The duration is intentionally omitted: readiness can end on either
		// k8sReadyTimeout or a shorter caller-context deadline, and a
		// hardcoded "after 1m0s" would misreport the latter. The wrapped
		// error (context.DeadlineExceeded or a Get failure) carries the cause.
		return nil, fmt.Errorf("pod %s/%s not ready: %w", cfg.Namespace, created.Name, err)
	}

	// Install the egress NetworkPolicy that backs the declared network mode.
	// proxyEnvFor already validated the mode above, so egressPolicyFor cannot
	// hit its default arm here. A failure to install the policy is fatal: a
	// Pod whose egress is meant to be denied/allowlisted but is not must not
	// be handed back to the caller. Tear the Pod down before returning.
	policy, err := egressPolicyFor(cfg.Network, cfg.Namespace, created.Name)
	if err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		return nil, err
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		deletePodBestEffort(clientset, cfg.Namespace, created.Name, logger)
		return nil, fmt.Errorf("install egress NetworkPolicy for pod %s/%s: %w", cfg.Namespace, created.Name, err)
	}

	return &K8sExecutor{
		clientset:         clientset,
		restConfig:        restCfg,
		namespace:         cfg.Namespace,
		podName:           created.Name,
		network:           cfg.Network,
		networkPolicyName: policy.Name,
		logger:            logger,
		Security:          cfg.Security,
	}, nil
}

// Close deletes the sandbox Pod with zero grace period. It uses its own
// background context so cleanup proceeds even if the caller's context is
// already cancelled — matching the Docker executor's Close discipline.
// A NotFound result is treated as success (idempotent).
func (e *K8sExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCloseTimeout)
	defer cancel()

	// Delete the egress NetworkPolicy first, best-effort: a leftover policy
	// is harmless once the Pod it selects is gone (NetworkPolicy with a
	// podSelector that matches nothing is inert), but leaking one per run
	// clutters the namespace. A NotFound is success. Any other error is
	// logged but does not block Pod deletion — the Pod is the resource that
	// actually costs the operator, so its removal must not hinge on the
	// policy delete succeeding.
	if e.networkPolicyName != "" {
		npErr := e.clientset.NetworkingV1().NetworkPolicies(e.namespace).Delete(ctx, e.networkPolicyName, metav1.DeleteOptions{})
		if npErr != nil && !apierrors.IsNotFound(npErr) {
			e.logger.Warn("k8s executor networkpolicy delete failed", "namespace", e.namespace, "networkpolicy", e.networkPolicyName, "err", npErr)
		}
	}

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
		if e.Security != nil {
			e.Security.PathTraversalBlocked(relativePath, k8sWorkspace)
		}
		return "", fmt.Errorf("path escapes workspace: %s", relativePath)
	}
	return resolved, nil
}

// resolveFilePath is ResolvePath with the additional guard that the result
// is not the workspace root itself. ReadFile/WriteFile/ListDirectory all
// expect a path *inside* the workspace; an empty, ".", or "/workspace"
// argument would otherwise resolve to "/workspace" and drive
// `mkdir -p /` / `tar -C / ...` with member name "workspace" — confusing
// today and a latent overwrite-of-workspace risk under a looser image.
func (e *K8sExecutor) resolveFilePath(relativePath string) (string, error) {
	resolved, err := e.ResolvePath(relativePath)
	if err != nil {
		return "", err
	}
	if resolved == k8sWorkspace {
		return "", fmt.Errorf("path resolves to workspace root: %q", relativePath)
	}
	return resolved, nil
}

// ReadFile streams the file out of the Pod with `tar -cf - <path>` over the
// exec subresource and reads the single archived entry. A missing file maps
// to fs.ErrNotExist; a directory target is rejected. Content is capped at
// 10 MB.
func (e *K8sExecutor) ReadFile(ctx context.Context, filePath string) (string, error) {
	resolved, err := e.resolveFilePath(filePath)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, k8sFileIOTimeout)
	defer cancel()

	var stdout writeCapBuffer
	stdout.limit = k8sMaxOutput
	var stderr bytes.Buffer

	// `tar -C / -cf - -- <abs-path-without-leading-slash>` archives the
	// single file. Stripping the leading slash keeps tar from warning about
	// "removing leading /" on stderr and yields a predictable archive name.
	// The `--` terminator stops a path that ever starts with `-` from being
	// parsed as a tar option (e.g. --checkpoint-action=exec).
	arcPath := strings.TrimPrefix(resolved, "/")
	execErr := e.streamExec(ctx, []string{"tar", "-C", "/", "-cf", "-", "--", arcPath}, nil, &stdout, &stderr)
	if stdout.exceeded {
		e.emitFileSizeLimit(filePath, k8sMaxOutput)
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
		e.emitFileSizeLimit(filePath, header.Size)
		return "", errK8sOutputCap
	}

	// Read one byte past the cap so an over-cap payload is detectable by
	// length (a file of exactly k8sMaxOutput bytes is still allowed). The
	// stdout streaming-cap branch above already caught the case where the
	// whole archive overflowed; this guards a file whose tar header
	// under-reported its size, where the read itself crosses the cap (and
	// may surface as io.ErrUnexpectedEOF on the truncated buffer). Either
	// signal maps to errK8sOutputCap so callers branch consistently with
	// the other cap paths instead of on an opaque "unexpected EOF".
	data, err := io.ReadAll(io.LimitReader(tr, k8sMaxOutput+1))
	if int64(len(data)) > k8sMaxOutput || (errors.Is(err, io.ErrUnexpectedEOF) && int64(len(data)) >= k8sMaxOutput) {
		e.emitFileSizeLimit(filePath, int64(len(data)))
		return "", errK8sOutputCap
	}
	if err != nil {
		return "", fmt.Errorf("read file from tar: %w", err)
	}
	return string(data), nil
}

// WriteFile streams a one-entry tar archive into the Pod via
// `tar -C <dir> -xf -`, creating the file at filePath with mode 0644.
// Parent directories are created first with `mkdir -p`. Content is capped
// at 10 MB.
func (e *K8sExecutor) WriteFile(ctx context.Context, filePath string, content string) error {
	if int64(len(content)) > k8sMaxOutput {
		e.emitFileSizeLimit(filePath, int64(len(content)))
		return errK8sOutputCap
	}

	resolved, err := e.resolveFilePath(filePath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, k8sFileIOTimeout)
	defer cancel()

	dir := path.Dir(resolved)
	var mkOut, mkErr bytes.Buffer
	if mkErrRun := e.streamExec(ctx, []string{"mkdir", "-p", "--", dir}, nil, &mkOut, &mkErr); mkErrRun != nil {
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
	// Unlike ReadFile/WriteFile, the workspace root is a legitimate listing
	// target (it is what an agent enumerates first), so this uses the plain
	// ResolvePath rather than resolveFilePath. This matches LocalExecutor,
	// which lists "/workspace" for an empty argument.
	resolved, err := e.ResolvePath(dirPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, k8sFileIOTimeout)
	defer cancel()

	var stdout writeCapBuffer
	stdout.limit = k8sMaxOutput
	var stderr bytes.Buffer

	execErr := e.streamExec(ctx, []string{"ls", "-A1", "--", resolved}, nil, &stdout, &stderr)
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
		if e.Security != nil {
			// The exact overflow size is unknown (the cap stops buffering),
			// so report the floor: cap+1 bytes were seen on the overflowing
			// stream. clampInt avoids an int64->int wrap on 32-bit builds.
			e.Security.OutputTruncated(command, clampInt(k8sMaxOutput+1), clampInt(k8sMaxOutput))
		}
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
//
// Limitation: the v1/v2 streaming protocols do not carry a structured exit
// status — a non-zero exit there arrives as a plain error string ("error
// executing remote command: ..."), which has no ExitError and so returns
// (0, false). Modern API servers negotiate v4/v5, which do carry the code;
// callers that get (0, false) treat it as a transport/protocol error.
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

// emitFileSizeLimit reports a file-size-limit hit to the security emitter
// when one is wired. size is the observed (or floor) byte count.
func (e *K8sExecutor) emitFileSizeLimit(filePath string, size int64) {
	if e.Security != nil {
		e.Security.FileSizeLimitExceeded(filePath, size, k8sMaxOutput)
	}
}

// clampInt narrows an int64 to int without wrapping on 32-bit platforms,
// saturating at math.MaxInt. The security emitter takes int sizes; the cap
// fits in an int on 64-bit but the conversion is guarded for portability.
func clampInt(v int64) int {
	if v > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(v)
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

// Capabilities advertises the executor's capabilities. CanNetwork reflects
// the egress NetworkPolicy installed alongside the Pod (#178): Mode=="none"
// installs a deny-all policy (CanNetwork=false) and Mode=="allowlist"
// installs a proxy-only egress policy (CanNetwork=true). The report is honest
// against the installed object — with the standing CNI caveat that kindnet
// accepts but does not enforce NetworkPolicy (see K8sExecutorConfig).
//
// A zero-value executor (nil network, used in some unit tests) reports
// CanNetwork=false; NewK8sExecutor never produces one because it fails-closed
// on a nil network.
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

var _ Executor = (*K8sExecutor)(nil)
