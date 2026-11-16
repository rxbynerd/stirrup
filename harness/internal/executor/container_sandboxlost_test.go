package executor

import (
	"context"
	"encoding/json"
	"net/http"
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
			check.NoError(exec.SandboxLost())
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
