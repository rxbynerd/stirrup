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

	"github.com/rxbynerd/stirrup/types"
)

const (
	containerWorkspace = "/workspace"
)

// ContainerExecutorConfig holds the configuration for creating a ContainerExecutor.
type ContainerExecutorConfig struct {
	Image      string
	HostDir    string              // host directory to bind-mount at /workspace
	Network    *types.NetworkConfig
	Resources  *types.ResourceLimits
	SocketPath string              // override auto-detection; empty = auto-detect
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
}

// NewContainerExecutor creates and starts a container, returning an executor
// that runs all operations inside it. Call Close() when done to destroy the
// container.
func NewContainerExecutor(cfg ContainerExecutorConfig) (*ContainerExecutor, error) {
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

	networkMode := "none"
	if cfg.Network != nil && cfg.Network.Mode == "allowlist" {
		// Phase 1: allowlist uses bridge networking. A future phase could
		// create a custom network with iptables rules for the allowlist.
		networkMode = "bridge"
	}

	hc := &hostConfig{
		Binds:       []string{fmt.Sprintf("%s:%s", cfg.HostDir, containerWorkspace)},
		NetworkMode: networkMode,
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
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

	ctx := context.Background()

	containerID, err := api.createContainer(ctx, containerCreateRequest{
		Image:      cfg.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: containerWorkspace,
		HostConfig: hc,
	})
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	if err := api.startContainer(ctx, containerID); err != nil {
		// Best-effort cleanup on start failure.
		_ = api.removeContainer(ctx, containerID, true)
		return nil, fmt.Errorf("start container: %w", err)
	}

	return &ContainerExecutor{
		api:         api,
		containerID: containerID,
		workspace:   containerWorkspace,
		hostDir:     cfg.HostDir,
		networkMode: networkMode,
		Security:    nil,
	}, nil
}

// Close stops and removes the container. It should be called via defer after
// creating the executor.
func (e *ContainerExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop with a short grace period, then force-remove.
	_ = e.api.stopContainer(ctx, e.containerID, 5)
	if err := e.api.removeContainer(ctx, e.containerID, true); err != nil {
		return fmt.Errorf("remove container: %w", err)
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
	defer tarStream.Close()

	tr := tar.NewReader(tarStream)
	for {
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
	defer stream.Close()

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
