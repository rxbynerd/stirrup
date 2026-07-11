package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/rxbynerd/stirrup/types"
)

// podExecCore is the exec/file-I/O machinery shared by every executor that
// drives a sandbox Pod over the pods/exec subresource. Both K8sExecutor and
// the Agent Sandbox CRD executor embed it: they differ only in how the Pod is
// provisioned (direct Pod create vs. CRD), not in how commands and file I/O
// ride into the Pod once it is Ready.
//
// Command execution and file I/O both ride the pods/exec subresource: Exec
// runs `/bin/sh -c`, while ReadFile/WriteFile stream a tar archive over exec
// and ListDirectory runs `ls`. The image must therefore ship a shell, tar,
// and ls — see the embedding executor's config for the image requirement.
type podExecCore struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
	podName    string
	network    *types.NetworkConfig
	// Security, when non-nil, receives structured security events. It is
	// nil-checked at every call site so a zero-value core (used in
	// unit tests) emits nothing.
	Security SecurityEventEmitter
	logger   *slog.Logger
}

// ResolvePath validates that the given path does not escape the Pod
// workspace. The check is purely textual — there is no local filesystem
// to EvalSymlinks against, since the workspace lives inside the Pod.
func (e *podExecCore) ResolvePath(relativePath string) (string, error) {
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
func (e *podExecCore) resolveFilePath(relativePath string) (string, error) {
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
func (e *podExecCore) ReadFile(ctx context.Context, filePath string) (string, error) {
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
func (e *podExecCore) WriteFile(ctx context.Context, filePath string, content string) error {
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
func (e *podExecCore) ListDirectory(ctx context.Context, dirPath string) ([]string, error) {
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
// clamped to MaxTimeout. On deadline or cancellation, classifyExecCtxErr
// distinguishes the two (errors.Is against executor.ErrTimeout for a
// genuine deadline, plain context.Canceled otherwise) and whatever
// stdout/stderr streamExec had already captured is preserved on the
// returned result rather than discarded (#473) — mirroring local.go and
// container.go. The exit code is extracted from the remotecommand
// CodeExitError; a clean exit yields code 0.
func (e *podExecCore) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
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

	if ctx.Err() != nil {
		return &ExecResult{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
		}, classifyExecCtxErr(ctx, timeout)
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
func (e *podExecCore) streamExec(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
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

	exec, err := newRemoteExecutor(e.restConfig, req.URL())
	if err != nil {
		return fmt.Errorf("build exec streamer: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// newRemoteExecutor builds a remotecommand.Executor that negotiates the exec
// streaming protocol the way kubectl does: WebSocket first, falling back to
// SPDY only when the WebSocket upgrade fails. WebSocket exec is the modern
// default (the API server has served the v5.channel.k8s.io subprotocol since
// v1.29) and, unlike the legacy SPDY upgrade, survives an HTTP(S) proxy or the
// GKE Connect Gateway sitting in front of the API server — both reject the
// SPDY upgrade (the gateway returns a bare HTTP 400). SPDY is retained as the
// fallback so the executor keeps working against API servers that predate the
// WebSocket exec subprotocol. This mirrors k8s.io/kubectl's createExecutor.
//
// The WebSocket executor is constructed with method GET (it issues an HTTP GET
// upgrade), the SPDY executor with POST; the fallback predicate fires only on
// a genuine upgrade/proxy failure so a normal command error is not retried.
func newRemoteExecutor(config *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	spdyExec, err := remotecommand.NewSPDYExecutor(config, "POST", u)
	if err != nil {
		return nil, err
	}
	wsExec, err := remotecommand.NewWebSocketExecutor(config, "GET", u.String())
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
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
func (e *podExecCore) emitFileSizeLimit(filePath string, size int64) {
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
// A zero-value core (nil network, used in some unit tests) reports
// CanNetwork=false; NewK8sExecutor never produces one because it fails-closed
// on a nil network.
//
// MaxTimeout deliberately mirrors container.go and local.go (maxTimeout =
// 30 min, raised from 5 min for lifecycle hooks — #461). Returning 0 would
// silently disable timeout clamping in callers that compare against
// MaxTimeout — and Exec clamps against it. The cap is identical across
// executors so a caller written against the Executor interface clamps
// uniformly regardless of which implementation is active.
func (e *podExecCore) Capabilities() ExecutorCapabilities {
	canNetwork := e.network != nil && e.network.Mode != "none"
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: canNetwork,
		MaxTimeout: maxTimeout,
	}
}
