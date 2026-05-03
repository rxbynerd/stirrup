package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor/egressproxy"
	"github.com/rxbynerd/stirrup/types"
)

const (
	containerWorkspace = "/workspace"
	maxDockerFrameSize = 10 * 1024 * 1024 // 10 MB cap on Docker stream frames

	// hostGatewayHost is the DNS name we add to the container's /etc/hosts
	// (via ExtraHosts) that resolves to the host's address. Docker Engine
	// >=20.10 and Podman >=4.0 expand the magic value "host-gateway" into
	// the real bridge gateway IP. The harness does not support older
	// runtimes for the allowlist mode — see docs/safety-rings.md (Wave 4).
	hostGatewayHost = "host.docker.internal"
)

// ContainerExecutorConfig holds the configuration for creating a ContainerExecutor.
type ContainerExecutorConfig struct {
	Image      string
	HostDir    string // host directory to bind-mount at /workspace
	Network    *types.NetworkConfig
	Resources  *types.ResourceLimits
	SocketPath string // override auto-detection; empty = auto-detect
	// Runtime selects the OCI runtime for the container (e.g. "runsc",
	// "kata", "kata-qemu", "kata-fc"). Empty means "use the engine
	// default" — the field is omitted from the create-container request,
	// which yields runc on stock Docker. Validation of the closed set is
	// performed by types.ValidateRunConfig before the executor is built.
	Runtime string
	// EgressSecurity, when non-nil, is wired into the egress proxy so
	// per-request egress_allowed / egress_blocked events flow through the
	// same SecurityLogger the executor uses for path/file events.
	EgressSecurity egressproxy.SecurityEventEmitter
}

// ContainerExecutor implements Executor by running operations inside a
// container via the Docker Engine API (compatible with both Docker and Podman).
// The container is created on construction and destroyed on Close().
//
// File I/O uses the archive API (tar upload/download). Command execution uses
// the exec API. The workspace is bind-mounted from the host at /workspace.
type ContainerExecutor struct {
	api         *containerAPIClient
	containerID string
	workspace   string // always containerWorkspace ("/workspace")
	hostDir     string
	networkMode string
	Security    SecurityEventEmitter
	// proxy, when non-nil, is the in-process egress proxy started for the
	// allowlist network mode. Close() stops it.
	proxy *egressproxy.Proxy
}

// NewContainerExecutor creates and starts a container, returning an executor
// that runs all operations inside it. Call Close() when done to destroy the
// container.
//
// Deprecated: use NewContainerExecutorWithContext to ensure the egress proxy
// goroutine is bounded by the caller's lifetime. This wrapper preserves the
// pre-#42 signature for callers that have not migrated yet.
func NewContainerExecutor(cfg ContainerExecutorConfig) (*ContainerExecutor, error) {
	return NewContainerExecutorWithContext(context.Background(), cfg)
}

// NewContainerExecutorWithContext is the context-aware variant. The proxy's
// listener and server goroutine are released when ctx is cancelled, so the
// caller cannot leak a listening socket by forgetting to call Close on an
// early-return path.
func NewContainerExecutorWithContext(ctx context.Context, cfg ContainerExecutorConfig) (*ContainerExecutor, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("container executor requires an image")
	}
	if cfg.HostDir == "" {
		return nil, fmt.Errorf("container executor requires a host directory")
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		var err error
		socketPath, err = detectSocket()
		if err != nil {
			return nil, fmt.Errorf("detect container socket: %w", err)
		}
	}

	api := newContainerAPIClient(socketPath)

	hc := &hostConfig{
		Binds:       []string{fmt.Sprintf("%s:%s", cfg.HostDir, containerWorkspace)},
		NetworkMode: "none",
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Runtime:     cfg.Runtime,
	}

	var (
		proxy *egressproxy.Proxy
		env   []string
	)
	if cfg.Network != nil && cfg.Network.Mode == "allowlist" {
		var err error
		// Plumb the caller's ctx into Start so the proxy goroutine is
		// torn down when the build path is cancelled. Pre-#42 this was
		// context.Background(), which leaked listeners on slow boots and
		// on early-return failure paths (M4).
		proxy, err = egressproxy.Start(ctx, egressproxy.Config{
			Allowlist: cfg.Network.Allowlist,
			Security:  cfg.EgressSecurity,
		})
		if err != nil {
			return nil, fmt.Errorf("start egress proxy: %w", err)
		}
		// We need the host-side port; the host of the listener is on
		// 127.0.0.1 but the container reaches us via the bridge gateway,
		// not loopback, so we replace the listen host with the magic
		// host.docker.internal name (resolved by ExtraHosts below).
		_, port, splitErr := splitListenAddr(proxy.Addr())
		if splitErr != nil {
			stopProxyBounded(proxy)
			return nil, fmt.Errorf("parse proxy listen addr: %w", splitErr)
		}
		proxyURL := fmt.Sprintf("http://%s:%s", hostGatewayHost, port)

		hc.NetworkMode = "bridge"
		hc.ExtraHosts = []string{hostGatewayHost + ":host-gateway"}
		env = []string{
			"HTTP_PROXY=" + proxyURL,
			"HTTPS_PROXY=" + proxyURL,
			"NO_PROXY=localhost,127.0.0.1,::1",
		}
		// TODO(#42 follow-up): with this design, fail-closed depends on
		// the in-container client honouring HTTP_PROXY / HTTPS_PROXY. A
		// raw-TCP client (or one that does its own DNS) inside the
		// container can still bypass the proxy because the bridge
		// network has unrestricted egress. The full fail-closed posture
		// requires an iptables / nftables drop on the host that whitelists
		// only the proxy's listen address; that drop is privilege-sensitive
		// and not portable to macOS Docker Desktop. Tracked separately so
		// this v1 ships with the limitation honestly documented.
	}

	if cfg.Resources != nil {
		if cfg.Resources.CPUs > 0 {
			hc.NanoCPUs = int64(cfg.Resources.CPUs * 1e9)
		}
		if cfg.Resources.MemoryMB > 0 {
			hc.Memory = int64(cfg.Resources.MemoryMB) * 1024 * 1024
		}
		if cfg.Resources.PIDs > 0 {
			pids := int64(cfg.Resources.PIDs)
			hc.PidsLimit = &pids
		}
	}

	containerID, err := api.createContainer(ctx, containerCreateRequest{
		Image:      cfg.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: containerWorkspace,
		Env:        env,
		HostConfig: hc,
	})
	if err != nil {
		if proxy != nil {
			stopProxyBounded(proxy)
		}
		return nil, fmt.Errorf("create container: %w", err)
	}

	if err := api.startContainer(ctx, containerID); err != nil {
		// Best-effort cleanup on start failure.
		_ = api.removeContainer(ctx, containerID, true)
		if proxy != nil {
			stopProxyBounded(proxy)
		}
		return nil, fmt.Errorf("start container: %w", err)
	}

	return &ContainerExecutor{
		api:         api,
		containerID: containerID,
		workspace:   containerWorkspace,
		hostDir:     cfg.HostDir,
		networkMode: hc.NetworkMode,
		Security:    nil,
		proxy:       proxy,
	}, nil
}

// stopProxyBounded shuts the egress proxy down with a 5-second deadline.
// We never want a hung Engine on the host to wedge an executor build's
// error-path cleanup, and we never want to leak a listener if Stop never
// returns. The bounded timeout is the only correct knob here: passing
// context.Background() unbounded was the M4 leak risk.
func stopProxyBounded(p *egressproxy.Proxy) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Stop(stopCtx)
}

// splitListenAddr splits a host:port pair, accepting the special form
// "0.0.0.0:NNNN" that net.Listen returns when bound to all interfaces.
func splitListenAddr(addr string) (host, port string, err error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("addr %q has no port", addr)
	}
	return addr[:idx], addr[idx+1:], nil
}

// Close stops and removes the container. It should be called via defer after
// creating the executor. If an egress proxy was started for this executor it
// is shut down too.
func (e *ContainerExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop with a short grace period, then force-remove.
	_ = e.api.stopContainer(ctx, e.containerID, 5)
	removeErr := e.api.removeContainer(ctx, e.containerID, true)

	// Always attempt to stop the proxy, even if container removal failed,
	// so we don't leak a listening socket on the host.
	if e.proxy != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.proxy.Stop(stopCtx)
		stopCancel()
		e.proxy = nil
	}

	if removeErr != nil {
		return fmt.Errorf("remove container: %w", removeErr)
	}
	return nil
}

// ResolvePath validates that the given path does not escape the container
// workspace (/workspace). Returns the absolute path inside the container.
func (e *ContainerExecutor) ResolvePath(relativePath string) (string, error) {
	var resolved string
	if path.IsAbs(relativePath) {
		resolved = path.Clean(relativePath)
	} else {
		resolved = path.Join(e.workspace, relativePath)
	}

	// Ensure the resolved path is within the workspace.
	if resolved != e.workspace && !strings.HasPrefix(resolved, e.workspace+"/") {
		if e.Security != nil {
			e.Security.PathTraversalBlocked(relativePath, e.workspace)
		}
		return "", fmt.Errorf("path escapes workspace: %s", relativePath)
	}
	return resolved, nil
}

// ReadFile reads a file from inside the container using the archive API.
// The file must be within /workspace and no larger than 10 MB.
func (e *ContainerExecutor) ReadFile(ctx context.Context, filePath string) (string, error) {
	resolved, err := e.ResolvePath(filePath)
	if err != nil {
		return "", err
	}

	tarStream, err := e.api.getArchive(ctx, e.containerID, resolved)
	if err != nil {
		return "", fmt.Errorf("get archive: %w", err)
	}
	defer func() { _ = tarStream.Close() }()

	tr := tar.NewReader(tarStream)
	header, err := tr.Next()
	if err == io.EOF {
		return "", fmt.Errorf("file not found in archive: %s", filePath)
	}
	if err != nil {
		return "", fmt.Errorf("read tar header: %w", err)
	}

	if header.Typeflag == tar.TypeDir {
		return "", fmt.Errorf("path is a directory, not a file: %s", filePath)
	}

	if header.Size > maxFileSize {
		if e.Security != nil {
			e.Security.FileSizeLimitExceeded(filePath, header.Size, maxFileSize)
		}
		return "", fmt.Errorf("file too large: %d bytes (max %d)", header.Size, maxFileSize)
	}

	data, err := io.ReadAll(io.LimitReader(tr, maxFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read file from tar: %w", err)
	}
	return string(data), nil
}

// WriteFile writes content to a file inside the container using the archive API.
// Parent directories must already exist in the container, or the archive
// extraction will create them. Content must not exceed 10 MB.
func (e *ContainerExecutor) WriteFile(ctx context.Context, filePath string, content string) error {
	if len(content) > maxFileSize {
		if e.Security != nil {
			e.Security.FileSizeLimitExceeded(filePath, int64(len(content)), maxFileSize)
		}
		return fmt.Errorf("content too large: %d bytes (max %d)", len(content), maxFileSize)
	}

	resolved, err := e.ResolvePath(filePath)
	if err != nil {
		return err
	}

	// Ensure parent directory exists inside the container.
	dir := path.Dir(resolved)
	if dir != e.workspace {
		_, mkdirErr := e.execInContainer(ctx, []string{"mkdir", "-p", dir}, e.workspace, 10*time.Second)
		if mkdirErr != nil {
			return fmt.Errorf("create parent directory: %w", mkdirErr)
		}
	}

	// Build a tar archive containing the single file.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// The archive path must be relative to the destination (the parent dir).
	archiveName := path.Base(resolved)
	if err := tw.WriteHeader(&tar.Header{
		Name: archiveName,
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

	// Upload to the parent directory so the file is placed correctly.
	if err := e.api.putArchive(ctx, e.containerID, dir, &buf); err != nil {
		return fmt.Errorf("put archive: %w", err)
	}
	return nil
}

// ListDirectory lists entries in a directory inside the container by running
// ls -1a via exec. This is simpler than parsing tar directory listings.
func (e *ContainerExecutor) ListDirectory(ctx context.Context, dirPath string) ([]string, error) {
	resolved, err := e.ResolvePath(dirPath)
	if err != nil {
		return nil, err
	}

	result, err := e.execInContainer(ctx, []string{"ls", "-1a", resolved}, e.workspace, defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("list directory: %s", strings.TrimSpace(result.Stderr))
	}

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	var entries []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "." || line == ".." {
			continue
		}
		entries = append(entries, line)
	}
	return entries, nil
}

// Exec runs a shell command inside the container with the given timeout.
// A zero timeout uses the default (30s). Output is truncated at 1 MB.
func (e *ContainerExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := e.execInContainer(ctx, []string{"sh", "-c", command}, e.workspace, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return &ExecResult{ExitCode: -1}, fmt.Errorf("command timed out after %s", timeout)
		}
		return nil, err
	}

	// Truncate output.
	originalStdoutLen := len(result.Stdout)
	originalStderrLen := len(result.Stderr)
	result.Stdout = truncate(result.Stdout, maxOutputSize)
	result.Stderr = truncate(result.Stderr, maxOutputSize)

	if e.Security != nil {
		combinedSize := originalStdoutLen + originalStderrLen
		if combinedSize > maxOutputSize {
			e.Security.OutputTruncated(command, combinedSize, maxOutputSize)
		}
	}

	return result, nil
}

// Capabilities returns the capabilities of the container executor.
func (e *ContainerExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: e.networkMode != "none",
		MaxTimeout: maxTimeout,
	}
}

// execInContainer runs a command inside the container using the exec API.
// It handles the full create → start → read output → inspect flow.
func (e *ContainerExecutor) execInContainer(ctx context.Context, cmd []string, workdir string, _ time.Duration) (*ExecResult, error) {
	execID, err := e.api.createExec(ctx, e.containerID, cmd, workdir)
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	stream, err := e.api.startExec(ctx, execID)
	if err != nil {
		return nil, fmt.Errorf("start exec: %w", err)
	}
	defer func() { _ = stream.Close() }()

	stdout, stderr, err := demuxDockerStream(stream)
	if err != nil {
		return nil, fmt.Errorf("read exec output: %w", err)
	}

	exitCode, err := e.api.inspectExec(ctx, execID)
	if err != nil {
		return nil, fmt.Errorf("inspect exec: %w", err)
	}

	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   stderr,
	}, nil
}

// demuxDockerStream reads the Docker multiplexed stream format.
// Each frame has an 8-byte header: [stream_type(1), 0, 0, 0, size(4 big-endian)].
// Stream type 1 = stdout, 2 = stderr.
func demuxDockerStream(r io.Reader) (string, string, error) {
	var stdout, stderr bytes.Buffer
	header := make([]byte, 8)

	for {
		_, err := io.ReadFull(r, header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("read stream header: %w", err)
		}

		streamType := header[0]
		frameSize := binary.BigEndian.Uint32(header[4:8])

		if frameSize == 0 {
			continue
		}

		if frameSize > maxDockerFrameSize {
			return stdout.String(), stderr.String(), fmt.Errorf("docker stream frame exceeds %d byte limit: %d", maxDockerFrameSize, frameSize)
		}

		// Once accumulated output exceeds the cap, drain remaining frames
		// without buffering to prevent unbounded memory growth.
		if stdout.Len()+stderr.Len() > maxOutputSize {
			if _, err := io.CopyN(io.Discard, r, int64(frameSize)); err != nil {
				return stdout.String(), stderr.String(), fmt.Errorf("drain stream frame: %w", err)
			}
			continue
		}

		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(r, frame); err != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("read stream frame: %w", err)
		}

		switch streamType {
		case 1:
			stdout.Write(frame)
		case 2:
			stderr.Write(frame)
		}
	}

	return stdout.String(), stderr.String(), nil
}
