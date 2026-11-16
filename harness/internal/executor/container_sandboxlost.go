package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// containerState is the part of GET /containers/{id}/json's State object
// that decides whether the sandbox container has ended.
type containerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  int    `json:"ExitCode"`
}

// inspectContainerState reads the container's state; found is false when
// the engine no longer has the container.
func (c *containerAPIClient) inspectContainerState(ctx context.Context, id string) (state containerState, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, containerControlPlaneTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(fmt.Sprintf("/containers/%s/json", id)), nil)
	if err != nil {
		return containerState{}, false, err
	}
	resp, err := c.doRequest(req)
	var statusErr *apiStatusError
	if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
		return containerState{}, false, nil
	}
	if err != nil {
		return containerState{}, false, classifyControlPlaneErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		State containerState `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return containerState{}, false, fmt.Errorf("decode container inspect response: %w", err)
	}
	return body.State, true, nil
}

// SandboxLost reports the container's terminal state once an exec has
// found it ended; see SandboxLossReporter.
func (e *ContainerExecutor) SandboxLost() error {
	if lost := e.lost.Load(); lost != nil {
		return lost
	}
	return nil
}

var _ SandboxLossReporter = (*ContainerExecutor)(nil)

// sandboxLostAfter inspects the container once after the engine refused to
// create an exec with 404 (no such container) or 409 (not running). A
// container that is gone or has exited yields a *SandboxLostError, recorded
// so later execs fail without contacting the engine. Anything else,
// including a paused container or a failed inspect, returns execErr.
func (e *ContainerExecutor) sandboxLostAfter(ctx context.Context, execErr error) error {
	var statusErr *apiStatusError
	if !errors.As(execErr, &statusErr) || (statusErr.status != http.StatusNotFound && statusErr.status != http.StatusConflict) {
		return execErr
	}
	state, found, err := e.api.inspectContainerState(ctx, e.containerID)
	if err != nil {
		return execErr
	}
	subject := "sandbox container " + shortContainerID(e.containerID)
	var desc string
	switch {
	case !found:
		desc = subject + " no longer exists"
	case state.Running || (state.Status != "exited" && state.Status != "dead"):
		return execErr
	case state.OOMKilled:
		desc = fmt.Sprintf("%s has exited: OOMKilled (exit code %d)", subject, state.ExitCode)
	default:
		desc = fmt.Sprintf("%s has exited (exit code %d)", subject, state.ExitCode)
	}
	if e.lost.CompareAndSwap(nil, &SandboxLostError{State: desc, Err: execErr}) {
		slog.Warn("container executor sandbox has ended; later operations fail without contacting it", "container", shortContainerID(e.containerID), "state", desc)
	}
	return e.lost.Load()
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
