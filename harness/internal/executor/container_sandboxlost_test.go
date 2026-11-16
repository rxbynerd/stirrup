package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingExecHandlers answer exec create with status and container
// inspect with inspect, counting exec creates.
func refusingExecHandlers(status int, inspect http.HandlerFunc, creates *atomic.Int32) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, _ *http.Request) {
			creates.Add(1)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"container test-container-id is not running"}`))
		},
		"GET /containers/*/json": inspect,
	}
}

func inspectState(state containerState) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"State": state})
	}
}

func TestContainerExecutor_Exec_ReportsExitedContainerAsLost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var creates atomic.Int32
	exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusConflict,
		inspectState(containerState{Status: "exited", OOMKilled: true, ExitCode: 137}), &creates))
	defer cleanup()

	_, err := exec.Exec(context.Background(), "echo hi", time.Second)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	check.Equal("sandbox container test-contain has exited: OOMKilled (exit code 137)", lost.State)
	check.Contains(err.Error(), "is not running")
	must.Equal(lost, exec.SandboxLost())

	_, err = exec.ReadFile(context.Background(), "a.txt")
	must.ErrorAs(err, &lost)
	check.Equal(int32(1), creates.Load(), "a lost container is not contacted again")
}

func TestContainerExecutor_Exec_ReportsRemovedContainerAsLost(t *testing.T) {
	must := require.New(t)
	var creates atomic.Int32
	exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusNotFound,
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
		}, &creates))
	defer cleanup()

	_, err := exec.Exec(context.Background(), "true", time.Second)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	must.Equal("sandbox container test-contain no longer exists", lost.State)
}

func TestContainerExecutor_Exec_KeepsFailureWhenContainerIsNotEnded(t *testing.T) {
	cases := map[string]containerState{
		"running": {Status: "running", Running: true},
		"paused":  {Status: "paused", Running: true},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var creates atomic.Int32
			exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusConflict, inspectState(state), &creates))
			defer cleanup()

			for range 2 {
				_, err := exec.Exec(context.Background(), "true", time.Second)
				must.Error(err)
				var lost *SandboxLostError
				check.NotErrorAs(err, &lost)
			}
			must.NoError(exec.SandboxLost())
			check.Equal(int32(2), creates.Load())
		})
	}
}

func TestContainerExecutor_Exec_OtherEngineErrorsDoNotInspect(t *testing.T) {
	must := require.New(t)
	var creates, inspects atomic.Int32
	exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusInternalServerError,
		func(w http.ResponseWriter, _ *http.Request) {
			inspects.Add(1)
			inspectState(containerState{Status: "exited"})(w, nil)
		}, &creates))
	defer cleanup()

	_, err := exec.Exec(context.Background(), "true", time.Second)
	must.Error(err)
	must.NoError(exec.SandboxLost())
	must.Equal(int32(0), inspects.Load())
}

func TestContainerExecutor_Exec_ReportsDeadContainerAsLost(t *testing.T) {
	must := require.New(t)
	var creates atomic.Int32
	exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusConflict,
		inspectState(containerState{Status: "dead", ExitCode: 255}), &creates))
	defer cleanup()

	_, err := exec.Exec(context.Background(), "true", time.Second)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	must.Equal("sandbox container test-contain has exited (exit code 255)", lost.State)
}

func TestContainerExecutor_Exec_KeepsFailureWhenInspectFails(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"engine error": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"engine busy"}`, http.StatusInternalServerError)
		},
		"undecodable response": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		},
	}
	for name, inspect := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var creates atomic.Int32
			exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusConflict, inspect, &creates))
			defer cleanup()

			for range 2 {
				_, err := exec.Exec(context.Background(), "true", time.Second)
				must.ErrorContains(err, "is not running")
				var lost *SandboxLostError
				check.NotErrorAs(err, &lost)
			}
			must.NoError(exec.SandboxLost())
			check.Equal(int32(2), creates.Load(), "a failed inspect leaves the container to be contacted again")
		})
	}
}

func TestContainerExecutor_ExecStream_ReportsExitedContainerAsLost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var creates atomic.Int32
	exec, cleanup := newMockContainerExecutor(t, refusingExecHandlers(http.StatusConflict,
		inspectState(containerState{Status: "exited", OOMKilled: true, ExitCode: 137}), &creates))
	defer cleanup()

	_, err := exec.ExecStream(context.Background(), "echo hi", time.Second, io.Discard, io.Discard)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	check.Equal("sandbox container test-contain has exited: OOMKilled (exit code 137)", lost.State)
	must.Equal(lost, exec.SandboxLost())

	_, err = exec.ExecStream(context.Background(), "echo hi", time.Second, io.Discard, io.Discard)
	must.ErrorAs(err, &lost)
	check.Equal(int32(1), creates.Load(), "a lost container is not contacted again")
}

func TestContainerExecutor_ExecInContainer_EndedContextDoesNotInspect(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		timeout time.Duration
		want    string
	}{
		{name: "cancelled", cancel: true, timeout: time.Minute, want: "command cancelled"},
		{name: "timed out", timeout: 50 * time.Millisecond, want: ErrTimeout.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var inspects atomic.Int32
			// The engine answers 409 only once the context has ended, so the
			// refusal is received after the exec was already cancelled or
			// timed out and the container could not be inspected for it.
			engine := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					inspects.Add(1)
					return engineResponse(http.StatusOK, `{"State":{"Status":"exited","ExitCode":137}}`), nil
				}
				if tc.cancel {
					cancel()
				}
				<-req.Context().Done()
				return engineResponse(http.StatusConflict, `{"message":"container is not running"}`), nil
			})
			exec := &ContainerExecutor{
				api:         &containerAPIClient{client: &http.Client{Transport: engine}},
				containerID: "test-container-id",
				workspace:   containerWorkspace,
			}

			_, err := exec.execInContainer(ctx, []string{"true"}, exec.workspace, tc.timeout)
			must.ErrorContains(err, tc.want)
			var lost *SandboxLostError
			must.NotErrorAs(err, &lost)
			must.NoError(exec.SandboxLost())
			must.Equal(int32(0), inspects.Load())
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func engineResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
