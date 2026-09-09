package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const apiVersion = "v1.47"

var (
	// containerDialTimeout bounds the initial Unix-socket connection. A var
	// (not const) so tests can shrink it to exercise the timeout path.
	containerDialTimeout = 10 * time.Second
	// containerResponseHeaderTimeout bounds only the wait for response
	// headers, never the body, so it cannot kill a long-running exec output
	// stream or archive transfer once headers have arrived. Connection-level
	// backstop for a daemon that accepts but never answers.
	containerResponseHeaderTimeout = 30 * time.Second
	// containerControlPlaneTimeout bounds a single short Docker Engine API
	// call (create/start/stop/remove/ping/exec-create/exec-inspect).
	// Deliberately NOT applied to startExec or the archive endpoints, which
	// are long-lived streams bounded by the caller's own deadline instead.
	containerControlPlaneTimeout = 30 * time.Second
)

// containerAPIClient is a thin client for the Docker Engine API (also
// compatible with Podman) that communicates over a Unix socket. It uses only
// the Go standard library — no external SDK dependencies.
type containerAPIClient struct {
	client     *http.Client
	host       string // display-only; all requests go to http://localhost
	socketPath string // raw dial target for hijacked (stdin-carrying) exec streams
}

// newContainerAPIClient creates a client connected to the given Unix
// socket. It carries no blanket http.Client.Timeout — see the timeout
// tiering rationale in docs/architecture.md.
func newContainerAPIClient(socketPath string) *containerAPIClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialContainerSocket(ctx, socketPath)
		},
		ResponseHeaderTimeout: containerResponseHeaderTimeout,
	}
	return &containerAPIClient{
		client:     &http.Client{Transport: transport},
		host:       "unix://" + socketPath,
		socketPath: socketPath,
	}
}

func dialContainerSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	return (&net.Dialer{Timeout: containerDialTimeout}).DialContext(ctx, "unix", socketPath)
}

// classifyControlPlaneErr reclassifies a failed short Docker Engine API
// call through the shared ErrTimeout sentinel when containerControlPlaneTimeout
// elapsed, so callers matching errors.Is(err, ErrTimeout) see it identically
// to a command timeout. See docs/architecture.md.
func classifyControlPlaneErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return classifyExecCtxErr(ctx, containerControlPlaneTimeout)
	}
	return err
}

// detectSocket probes for a container runtime socket in priority order:
//  1. DOCKER_HOST env var (parsed as unix:// path)
//  2. /var/run/docker.sock
//  3. $XDG_RUNTIME_DIR/podman/podman.sock (rootless Podman)
//  4. /var/run/podman/podman.sock (rootful Podman)
func detectSocket() (string, error) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		path := strings.TrimPrefix(host, "unix://")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("DOCKER_HOST socket not found: %s", path)
	}

	candidates := []string{"/var/run/docker.sock"}

	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "podman", "podman.sock"))
	}
	candidates = append(candidates, "/var/run/podman/podman.sock")

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no container runtime socket found (tried Docker and Podman)")
}

// url constructs an API URL for the given endpoint path.
func (c *containerAPIClient) url(path string) string {
	return fmt.Sprintf("http://localhost/%s%s", apiVersion, path)
}

// apiError is the JSON error structure returned by the Engine API.
type apiError struct {
	Message string `json:"message"`
}

// doRequest executes a request and returns the response. The caller is
// responsible for closing the body. If the status code indicates an error
// (>= 400), the body is read, parsed, and returned as a Go error.
func (c *containerAPIClient) doRequest(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker API request %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr apiError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			return nil, fmt.Errorf("docker API %s %s: %s (HTTP %d)", req.Method, req.URL.Path, apiErr.Message, resp.StatusCode)
		}
		return nil, fmt.Errorf("docker API %s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, string(body))
	}
	return resp, nil
}

// containerCreateRequest is the JSON body for POST /containers/create.
type containerCreateRequest struct {
	Image      string   `json:"Image"`
	Cmd        []string `json:"Cmd"`
	WorkingDir string   `json:"WorkingDir"`
	// Env propagates HTTP_PROXY/HTTPS_PROXY/NO_PROXY when an egress proxy
	// fronts the container.
	Env        []string    `json:"Env,omitempty"`
	HostConfig *hostConfig `json:"HostConfig"`
	// User runs the container's main process as uid[:gid]; see
	// docs/security.md for the hardened default.
	User string `json:"User,omitempty"`
}

type hostConfig struct {
	Binds       []string `json:"Binds,omitempty"`
	NetworkMode string   `json:"NetworkMode,omitempty"`
	NanoCPUs    int64    `json:"NanoCpus,omitempty"`
	Memory      int64    `json:"Memory,omitempty"`
	PidsLimit   *int64   `json:"PidsLimit,omitempty"`
	CapDrop     []string `json:"CapDrop,omitempty"`
	SecurityOpt []string `json:"SecurityOpt,omitempty"`
	// No omitempty: a bool with omitempty would drop false from the wire,
	// silently defaulting to a writable rootfs. See docs/security.md.
	ReadonlyRootfs bool `json:"ReadonlyRootfs"`
	// Tmpfs maps an in-container path to a comma-separated mount-option
	// string (e.g. "rw,nosuid,nodev,noexec,size=268435456"); see
	// docs/security.md. The size rides in the option string rather than a
	// separate ShmSize field so it and noexec travel together on one mount.
	Tmpfs map[string]string `json:"Tmpfs,omitempty"`
	// Runtime selects the OCI runtime (e.g. "runsc" for gVisor). Empty is
	// omitted from the wire so the engine picks its default (runc).
	Runtime string `json:"Runtime,omitempty"`
	// ExtraHosts injects "host.docker.internal:host-gateway" so the
	// container can reach the host's egress proxy.
	ExtraHosts []string `json:"ExtraHosts,omitempty"`
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

func (c *containerAPIClient) createContainer(ctx context.Context, cfg containerCreateRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	body, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal container config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/containers/create"), strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return "", classifyControlPlaneErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result containerCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	return result.ID, nil
}

// ping issues GET /_ping against the engine socket. A 2xx (the engine
// answers "OK") confirms the daemon is reachable; doRequest turns any
// >=400 into an error. Used by the dry-run preflight probe so an
// unreachable or stopped daemon surfaces before the run commits to
// creating a container.
func (c *containerAPIClient) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/_ping"), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return classifyControlPlaneErr(ctx, err)
	}
	_ = resp.Body.Close()
	return nil
}

// imageExistsLocally reports whether the named image is present in the
// engine's local store via GET /images/{name}/json. A 404 is translated
// to (false, nil) — the image is simply not pulled — so the preflight can
// distinguish "engine unreachable" (an error) from "image absent" (a
// warning the operator can act on without a failed run). The probe never
// pulls: that is a deliberate cost/latency decision left to the run
// itself. The image reference is path-escaped because it may contain a
// registry host, a tag, or an "@sha256:" digest with reserved bytes.
func (c *containerAPIClient) imageExistsLocally(ctx context.Context, image string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/images/"+url.PathEscape(image)+"/json"), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, classifyControlPlaneErr(ctx, fmt.Errorf("docker API request GET /images/%q/json: %w", image, err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr apiError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			return false, fmt.Errorf("docker API GET /images/%q/json: %s (HTTP %d)", image, apiErr.Message, resp.StatusCode)
		}
		return false, fmt.Errorf("docker API GET /images/%q/json: HTTP %d", image, resp.StatusCode)
	default:
		return true, nil
	}
}

func (c *containerAPIClient) startContainer(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(fmt.Sprintf("/containers/%s/start", id)), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return classifyControlPlaneErr(ctx, err)
	}
	_ = resp.Body.Close()
	return nil
}

func (c *containerAPIClient) stopContainer(ctx context.Context, id string, timeout int) error {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	url := fmt.Sprintf("/containers/%s/stop?t=%d", id, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(url), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		// 304 means already stopped, which is fine.
		if strings.Contains(err.Error(), "HTTP 304") {
			return nil
		}
		return classifyControlPlaneErr(ctx, err)
	}
	_ = resp.Body.Close()
	return nil
}

func (c *containerAPIClient) removeContainer(ctx context.Context, id string, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	url := fmt.Sprintf("/containers/%s?force=%t", id, force)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(url), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return classifyControlPlaneErr(ctx, err)
	}
	_ = resp.Body.Close()
	return nil
}

type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

// createExec registers the exec instance but does not run it, so it gets
// containerControlPlaneTimeout rather than the caller's command timeout.
func (c *containerAPIClient) createExec(ctx context.Context, containerID string, cmd []string, workdir string) (string, error) {
	return c.createExecAttach(ctx, containerID, cmd, workdir, false)
}

// createExecStdin is createExec for an instance that will be started with
// startExecWithStdin; the daemon only wires a stdin pipe when the instance
// was created with AttachStdin.
func (c *containerAPIClient) createExecStdin(ctx context.Context, containerID string, cmd []string, workdir string) (string, error) {
	return c.createExecAttach(ctx, containerID, cmd, workdir, true)
}

func (c *containerAPIClient) createExecAttach(ctx context.Context, containerID string, cmd []string, workdir string, attachStdin bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	body, err := json.Marshal(execCreateRequest{
		AttachStdin:  attachStdin,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
		WorkingDir:   workdir,
	})
	if err != nil {
		return "", fmt.Errorf("marshal exec config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url(fmt.Sprintf("/containers/%s/exec", containerID)),
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return "", classifyControlPlaneErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result execCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode exec create response: %w", err)
	}
	return result.ID, nil
}

// startExec starts an exec instance and returns the multiplexed output
// stream. The caller must close the returned ReadCloser. Deliberately does
// NOT apply containerControlPlaneTimeout — bounded by ctx only, which the
// caller has already bound to the command's own timeout.
func (c *containerAPIClient) startExec(ctx context.Context, execID string) (io.ReadCloser, error) {
	body := `{"Detach":false,"Tty":false}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url(fmt.Sprintf("/exec/%s/start", execID)),
		strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// hijackedExecStream is the raw connection left behind once the daemon
// switches protocols on an exec start: reads drain the multiplexed
// stdout/stderr frames (the buffered reader may already hold the first
// ones), and Close tears the connection down, which also ends the stdin
// copy.
type hijackedExecStream struct {
	reader *bufio.Reader
	conn   net.Conn
	stop   func() bool

	copyDone chan struct{}
	copyErr  error
}

func (s *hijackedExecStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *hijackedExecStream) Close() error {
	s.stop()
	return s.conn.Close()
}

// stdinErr reports how the stdin copy ended once it has finished: nil for
// a complete copy, otherwise the reader's or the connection's error. It
// waits for the copy, which the process's exit unblocks by closing the
// daemon's side of the connection.
func (s *hijackedExecStream) stdinErr(ctx context.Context) error {
	select {
	case <-s.copyDone:
		return s.copyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startExecWithStdin starts an exec instance over a hijacked connection
// (Connection: Upgrade / Upgrade: tcp), copies stdin to the process, and
// half-closes the write side at EOF so the process observes end-of-input.
// The returned stream carries the same multiplexed frames startExec does.
// A daemon that answers anything but 101 has not taken the connection
// over, so stdin could not be delivered and the call fails rather than
// running the command without its input. Bounded by ctx only, like
// startExec.
func (c *containerAPIClient) startExecWithStdin(ctx context.Context, execID string, stdin io.Reader) (*hijackedExecStream, error) {
	conn, err := dialContainerSocket(ctx, c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial container socket: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (*hijackedExecStream, error) {
		stop()
		_ = conn.Close()
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url(fmt.Sprintf("/exec/%s/start", execID)),
		strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		return fail(fmt.Errorf("docker API request %s %s: %w", req.Method, req.URL.Path, err))
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return fail(fmt.Errorf("docker API request %s %s: %w", req.Method, req.URL.Path, err))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return fail(fmt.Errorf("docker API %s %s: expected HTTP 101 for a stdin-attached exec, got HTTP %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body))))
	}

	stream := &hijackedExecStream{reader: reader, conn: conn, stop: stop, copyDone: make(chan struct{})}
	go func() {
		defer close(stream.copyDone)
		_, stream.copyErr = io.Copy(conn, stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	return stream, nil
}

type execInspectResponse struct {
	ExitCode int  `json:"ExitCode"`
	Running  bool `json:"Running"`
}

var (
	// execInspectRetries and execInspectRetryDelay bound how long inspectExec
	// waits for the daemon to record an exit: the output stream can end a
	// moment before the daemon marks the instance finished, and an exit code
	// read while Running is still true is a placeholder, not a result.
	execInspectRetries    = 20
	execInspectRetryDelay = 50 * time.Millisecond
)

// inspectExec fetches the exit code after the command finished streaming,
// retrying briefly while the daemon still reports the instance as running.
func (c *containerAPIClient) inspectExec(ctx context.Context, execID string) (int, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.inspectExecOnce(ctx, execID)
		if err != nil {
			return -1, err
		}
		if !result.Running {
			return result.ExitCode, nil
		}
		if attempt >= execInspectRetries {
			return -1, fmt.Errorf("exec %s still running after its output ended", execID)
		}
		select {
		case <-ctx.Done():
			return -1, classifyControlPlaneErr(ctx, ctx.Err())
		case <-time.After(execInspectRetryDelay):
		}
	}
}

// inspectExecOnce is a single short metadata read, so it gets the
// control-plane timeout.
func (c *containerAPIClient) inspectExecOnce(ctx context.Context, execID string) (execInspectResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.url(fmt.Sprintf("/exec/%s/json", execID)),
		nil)
	if err != nil {
		return execInspectResponse{}, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return execInspectResponse{}, classifyControlPlaneErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result execInspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return execInspectResponse{}, fmt.Errorf("decode exec inspect response: %w", err)
	}
	return result, nil
}

// putArchive uploads a tar archive to a path inside the container.
// Long-lived streaming call bounded only by ctx (the caller applies
// containerFileIOTimeout), same as startExec.
func (c *containerAPIClient) putArchive(ctx context.Context, containerID, destPath string, tarReader io.Reader) error {
	url := fmt.Sprintf("/containers/%s/archive?path=%s", containerID, url.QueryEscape(destPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(url), tarReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
