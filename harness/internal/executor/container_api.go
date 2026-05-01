package executor

import (
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
)

const apiVersion = "v1.47"

// containerAPIClient is a thin client for the Docker Engine API (also
// compatible with Podman) that communicates over a Unix socket. It uses only
// the Go standard library — no external SDK dependencies.
type containerAPIClient struct {
	client *http.Client
	host   string // display-only; all requests go to http://localhost
}

// newContainerAPIClient creates a client connected to the given Unix socket.
func newContainerAPIClient(socketPath string) *containerAPIClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &containerAPIClient{
		client: &http.Client{Transport: transport},
		host:   "unix://" + socketPath,
	}
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

// --- Container lifecycle ---

// containerCreateRequest is the JSON body for POST /containers/create.
type containerCreateRequest struct {
	Image      string   `json:"Image"`
	Cmd        []string `json:"Cmd"`
	WorkingDir string   `json:"WorkingDir"`
	// Env is the list of "KEY=value" environment-variable pairs to set on
	// the container. Used to propagate HTTP_PROXY / HTTPS_PROXY / NO_PROXY
	// when an egress proxy is in front of the container.
	Env        []string    `json:"Env,omitempty"`
	HostConfig *hostConfig `json:"HostConfig"`
}

type hostConfig struct {
	Binds       []string `json:"Binds,omitempty"`
	NetworkMode string   `json:"NetworkMode,omitempty"`
	NanoCPUs    int64    `json:"NanoCpus,omitempty"`
	Memory      int64    `json:"Memory,omitempty"`
	PidsLimit   *int64   `json:"PidsLimit,omitempty"`
	CapDrop     []string `json:"CapDrop,omitempty"`
	SecurityOpt []string `json:"SecurityOpt,omitempty"`
	// Runtime selects the OCI runtime (e.g. "runsc" for gVisor,
	// "kata-qemu" for Kata Containers). Empty means "use the engine
	// default", in which case the field is omitted from the wire so the
	// engine picks runc.
	Runtime string `json:"Runtime,omitempty"`
	// ExtraHosts adds entries to the container's /etc/hosts. Used to
	// inject "host.docker.internal:host-gateway" so the container can
	// reach the host's egress proxy on Docker Engine >=20.10 and
	// Podman >=4.0.
	ExtraHosts []string `json:"ExtraHosts,omitempty"`
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

func (c *containerAPIClient) createContainer(ctx context.Context, cfg containerCreateRequest) (string, error) {
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
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var result containerCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	return result.ID, nil
}

func (c *containerAPIClient) startContainer(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(fmt.Sprintf("/containers/%s/start", id)), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *containerAPIClient) stopContainer(ctx context.Context, id string, timeout int) error {
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
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *containerAPIClient) removeContainer(ctx context.Context, id string, force bool) error {
	url := fmt.Sprintf("/containers/%s?force=%t", id, force)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(url), nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// --- Exec ---

type execCreateRequest struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

func (c *containerAPIClient) createExec(ctx context.Context, containerID string, cmd []string, workdir string) (string, error) {
	body, err := json.Marshal(execCreateRequest{
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
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var result execCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode exec create response: %w", err)
	}
	return result.ID, nil
}

// startExec starts an exec instance and returns the multiplexed output stream.
// The caller must close the returned ReadCloser.
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

type execInspectResponse struct {
	ExitCode int  `json:"ExitCode"`
	Running  bool `json:"Running"`
}

func (c *containerAPIClient) inspectExec(ctx context.Context, execID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.url(fmt.Sprintf("/exec/%s/json", execID)),
		nil)
	if err != nil {
		return -1, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return -1, err
	}
	defer func() { _ = resp.Body.Close() }()

	var result execInspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return -1, fmt.Errorf("decode exec inspect response: %w", err)
	}
	return result.ExitCode, nil
}

// --- Archive (file transfer) ---

// putArchive uploads a tar archive to a path inside the container.
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

// getArchive downloads a tar archive of a path from inside the container.
// The caller must close the returned ReadCloser.
func (c *containerAPIClient) getArchive(ctx context.Context, containerID, srcPath string) (io.ReadCloser, error) {
	url := fmt.Sprintf("/containers/%s/archive?path=%s", containerID, url.QueryEscape(srcPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(url), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
