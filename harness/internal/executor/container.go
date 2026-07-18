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

	// hardenedUser is the conventional nobody:nogroup pair, so a container
	// escape lands on an unprivileged identity.
	hardenedUser = "65534:65534"

	// tmpfsTmpSize and shmSize size the two writable scratch mounts the
	// read-only rootfs requires. Both carry nosuid,nodev,noexec (tmpfsMountOpts)
	// so a dropped binary cannot be executed from them.
	tmpfsTmpSize = 256 * 1024 * 1024 // 256 MiB
	shmSize      = 64 * 1024 * 1024  // 64 MiB

	tmpfsMountOpts = "rw,nosuid,nodev,noexec"

	// hostGatewayHost resolves to the host's address via Docker/Podman's
	// magic "host-gateway" ExtraHosts value (Docker >=20.10, Podman >=4.0).
	hostGatewayHost = "host.docker.internal"
)

var (
	// containerFileIOTimeout bounds a single ReadFile/WriteFile operation.
	// The Docker API client's transport has no blanket timeout of its own
	// (see container_api.go), so a wedged daemon would otherwise hang the
	// calling goroutine indefinitely. A var, not const, so tests can shrink it.
	containerFileIOTimeout = 60 * time.Second
	// containerMkdirTimeout bounds WriteFile's "mkdir -p" exec call
	// specifically, snappier than the file-transfer budget above.
	containerMkdirTimeout = 10 * time.Second
)

// ContainerExecutorConfig holds the configuration for creating a ContainerExecutor.
type ContainerExecutorConfig struct {
	Image      string
	HostDir    string // host directory to bind-mount at /workspace
	Network    *types.NetworkConfig
	Resources  *types.ResourceLimits
	SocketPath string // override auto-detection; empty = auto-detect
	// RegistryAllowlist is a set of globs over the normalised image
	// reference (registry host + repo path, digest/tag stripped). An empty
	// list falls back to defaultRegistryAllowlist.
	RegistryAllowlist []string
	// Runtime selects the OCI runtime (e.g. "runsc", "kata-qemu"). Empty
	// omits the field from the create-container request, yielding runc.
	Runtime string
	// EgressSecurity, when non-nil, is wired into the egress proxy so
	// per-request events flow through the same SecurityLogger the executor
	// uses for path/file events.
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
	image       string // requested image, retained for the dry-run probe
	Security    SecurityEventEmitter
	// proxy, when non-nil, is the in-process egress proxy started for the
	// allowlist network mode. Close() stops it.
	proxy *egressproxy.Proxy
}

// Probe checks the container runtime for a dry-run preflight: it pings
// the engine socket (GET /_ping) and verifies the requested image is
// present locally (GET /images/{image}/json) without pulling. An absent
// image is reported as an error carrying a remediation hint rather than
// silently pulling at run time, which would burn the run's wall-clock on
// a multi-hundred-megabyte download the operator did not anticipate.
func (e *ContainerExecutor) Probe(ctx context.Context) error {
	return probeContainerEngine(ctx, e.api, e.image)
}

// ProbeContainerEngine performs the dry-run preflight check for a
// container executor WITHOUT constructing or starting a container: it
// pings the daemon and verifies the image is present locally. Unlike
// NewContainerExecutorWithContext, it never creates a container or starts
// the egress proxy, so a `--dry-run` stays read-only.
func ProbeContainerEngine(ctx context.Context, cfg ContainerExecutorConfig) error {
	if cfg.Image != "" {
		if err := checkImageAllowed(cfg.Image, cfg.RegistryAllowlist); err != nil {
			return err
		}
	}
	socketPath := cfg.SocketPath
	if socketPath == "" {
		var err error
		socketPath, err = detectSocket()
		if err != nil {
			return fmt.Errorf("detect container socket: %w", err)
		}
	}
	return probeContainerEngine(ctx, newContainerAPIClient(socketPath), cfg.Image)
}

// probeContainerEngine is the shared ping + image-presence check used by
// both ContainerExecutor.Probe (already-constructed executor) and
// ProbeContainerEngine (preflight, no container). An empty image skips the
// image check — the engine ping alone still confirms reachability.
func probeContainerEngine(ctx context.Context, api *containerAPIClient, image string) error {
	if err := api.ping(ctx); err != nil {
		return fmt.Errorf("container engine unreachable: %w", err)
	}
	if image == "" {
		return nil
	}
	present, err := api.imageExistsLocally(ctx, image)
	if err != nil {
		return fmt.Errorf("inspect image %q: %w", image, err)
	}
	if !present {
		return fmt.Errorf("image %q is not present locally; pull it before the run (e.g. `docker pull %q`)", image, image)
	}
	return nil
}

// NewContainerExecutor creates and starts a container, returning an executor
// that runs all operations inside it. Call Close() when done to destroy the
// container.
//
// Deprecated: use NewContainerExecutorWithContext to ensure the egress proxy
// goroutine is bounded by the caller's lifetime.
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
	if err := checkImageAllowed(cfg.Image, cfg.RegistryAllowlist); err != nil {
		return nil, err
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
		Binds:          []string{fmt.Sprintf("%s:%s", cfg.HostDir, containerWorkspace)},
		NetworkMode:    "none",
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Runtime:        cfg.Runtime,
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":     fmt.Sprintf("%s,size=%d", tmpfsMountOpts, tmpfsTmpSize),
			"/dev/shm": fmt.Sprintf("%s,size=%d", tmpfsMountOpts, shmSize),
		},
	}

	var (
		proxy *egressproxy.Proxy
		env   []string
	)
	if cfg.Network != nil && cfg.Network.Mode == "allowlist" {
		var err error

		proxy, err = egressproxy.Start(ctx, egressproxy.Config{
			Allowlist: cfg.Network.Allowlist,
			Security:  cfg.EgressSecurity,
		})
		if err != nil {
			return nil, fmt.Errorf("start egress proxy: %w", err)
		}
		// The container reaches the host-bound listener via the bridge
		// gateway, not loopback, so the listen host is replaced with the
		// magic host.docker.internal name (resolved by ExtraHosts below).
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
		// TODO(#42 follow-up): fail-closed depends on the in-container
		// client honouring HTTP_PROXY/HTTPS_PROXY; a raw-TCP client can
		// still bypass the proxy since the bridge network has unrestricted
		// egress. Full fail-closed needs a host iptables/nftables drop,
		// which is privilege-sensitive and not portable to macOS Docker
		// Desktop.
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
		User:       hardenedUser,
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
		image:       cfg.Image,
		Security:    nil,
		proxy:       proxy,
	}, nil
}

// stopProxyBounded shuts the egress proxy down with a 5-second deadline so
// a hung Engine cannot wedge an executor build's error-path cleanup, and a
// listener is never leaked if Stop never returns.
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

	_ = e.api.stopContainer(ctx, e.containerID, 5)
	removeErr := e.api.removeContainer(ctx, e.containerID, true)

	// Always attempt to stop the proxy, even if container removal failed,
	// to avoid leaking a listening socket on the host.
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

	if resolved != e.workspace && !strings.HasPrefix(resolved, e.workspace+"/") {
		if e.Security != nil {
			e.Security.PathTraversalBlocked(relativePath, e.workspace)
		}
		return "", fmt.Errorf("path escapes workspace: %s", relativePath)
	}
	return resolved, nil
}

// ReadFile reads a file from inside the container using the archive API.
// The file must be within /workspace and no larger than 10 MB. The whole
// operation is bounded by containerFileIOTimeout since the archive
// endpoints have no timeout of their own (see container_api.go).
func (e *ContainerExecutor) ReadFile(ctx context.Context, filePath string) (string, error) {
	resolved, err := e.ResolvePath(filePath)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, containerFileIOTimeout)
	defer cancel()

	tarStream, err := e.api.getArchive(ctx, e.containerID, resolved)
	if err != nil {
		return "", classifyFileIOCtxErr(ctx, "get archive", err)
	}
	defer func() { _ = tarStream.Close() }()

	tr := tar.NewReader(tarStream)
	header, err := tr.Next()
	if err == io.EOF {
		return "", fmt.Errorf("file not found in archive: %s", filePath)
	}
	if err != nil {
		return "", classifyFileIOCtxErr(ctx, "read tar header", err)
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
		return "", classifyFileIOCtxErr(ctx, "read file from tar", err)
	}
	return string(data), nil
}

// classifyFileIOCtxErr wraps a ReadFile/WriteFile failure through the
// shared ErrTimeout sentinel when it happened because the containerFileIOTimeout
// deadline elapsed, so callers matching errors.Is(err, ErrTimeout) see this
// identically to an Exec command timeout.
func classifyFileIOCtxErr(ctx context.Context, verb string, err error) error {
	if ctx.Err() != nil {
		return classifyExecCtxErr(ctx, containerFileIOTimeout)
	}
	return fmt.Errorf("%s: %w", verb, err)
}

// WriteFile writes content to a file inside the container using the archive
// API. Parent directories must already exist in the container, or the
// archive extraction will create them. Content must not exceed 10 MB. The
// whole operation (mkdir -p plus the archive upload) is bounded by
// containerFileIOTimeout, for the same reason as ReadFile.
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

	ctx, cancel := context.WithTimeout(ctx, containerFileIOTimeout)
	defer cancel()

	dir := path.Dir(resolved)
	if dir != e.workspace {
		_, mkdirErr := e.execInContainer(ctx, []string{"mkdir", "-p", dir}, e.workspace, containerMkdirTimeout)
		if mkdirErr != nil {
			return fmt.Errorf("create parent directory: %w", mkdirErr)
		}
	}

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

	if err := e.api.putArchive(ctx, e.containerID, dir, &buf); err != nil {
		return classifyFileIOCtxErr(ctx, "put archive", err)
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
			// Preserve whatever output execInContainer captured before the
			// ctx ended, rather than discarding it (nil only if the command
			// never started).
			if result == nil {
				result = &ExecResult{}
			}
			result.ExitCode = -1
			return e.truncateAndReport(command, result), classifyExecCtxErr(ctx, timeout)
		}
		return nil, err
	}

	return e.truncateAndReport(command, result), nil
}

// truncateAndReport caps result's stdout/stderr at maxOutputSize and, when a
// SecurityEventEmitter is wired, reports OutputTruncated using the
// pre-truncation combined size. Shared by the success and timeout/cancel
// paths of Exec so partial output captured before a ctx ending is truncated
// and reported identically to a clean run's output.
func (e *ContainerExecutor) truncateAndReport(command string, result *ExecResult) *ExecResult {
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

	return result
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

// execInContainer runs a command inside the container using the exec API,
// handling the full create → start → read output → inspect flow bounded by
// timeout. On a failure partway through (most commonly the ctx ending while
// demuxDockerStream is blocked reading), the returned *ExecResult still
// carries whatever stdout/stderr was captured; only a failure before any
// output could exist (create/start exec) returns a nil result. A ctx-expiry
// failure is classified via classifyExecCtxErr so callers matching
// errors.Is(err, ErrTimeout) see this identically to a top-level Exec timeout.
func (e *ContainerExecutor) execInContainer(ctx context.Context, cmd []string, workdir string, timeout time.Duration) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execID, err := e.api.createExec(ctx, e.containerID, cmd, workdir)
	if err != nil {
		if ctx.Err() != nil {
			return nil, classifyExecCtxErr(ctx, timeout)
		}
		return nil, fmt.Errorf("create exec: %w", err)
	}

	stream, err := e.api.startExec(ctx, execID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, classifyExecCtxErr(ctx, timeout)
		}
		return nil, fmt.Errorf("start exec: %w", err)
	}
	defer func() { _ = stream.Close() }()

	stdout, stderr, err := demuxDockerStream(stream)
	result := &ExecResult{Stdout: stdout, Stderr: stderr}
	if err != nil {
		if ctx.Err() != nil {
			return result, classifyExecCtxErr(ctx, timeout)
		}
		return result, fmt.Errorf("read exec output: %w", err)
	}

	exitCode, err := e.api.inspectExec(ctx, execID)
	if err != nil {
		if ctx.Err() != nil {
			return result, classifyExecCtxErr(ctx, timeout)
		}
		return result, fmt.Errorf("inspect exec: %w", err)
	}
	result.ExitCode = exitCode

	return result, nil
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
