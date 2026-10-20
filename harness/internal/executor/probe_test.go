package executor

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerExecutor_Probe_OK(t *testing.T) {
	must := require.New(t)
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		},
		"GET /images/*/json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))
		},
	})
	defer cleanup()

	exec := &ContainerExecutor{api: newContainerAPIClient(sock), image: "ubuntu:26.04"}
	must.NoError(exec.Probe(context.Background()))
}

func TestContainerExecutor_Probe_ImageAbsent(t *testing.T) {
	must := require.New(t)
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
		"GET /images/*/json": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such image"}`))
		},
	})
	defer cleanup()

	exec := &ContainerExecutor{api: newContainerAPIClient(sock), image: "missing:latest"}
	err := exec.Probe(context.Background())
	must.Error(err)
	must.ErrorContains(err, "not present locally", "error should include a pull hint")
	must.ErrorContains(err, "missing:latest", "error should name the missing image")
}

// TestProbeContainerEngine_NoContainerCreated asserts a container dry-run
// pings + inspects the image but never creates or starts a container, and
// never touches the egress proxy. The mock engine fails the test if it sees
// a create/start request.
func TestProbeContainerEngine_NoContainerCreated(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var createHits, startHits atomic.Int64
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		},
		"GET /images/*/json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))
		},
		"POST /containers/create": func(w http.ResponseWriter, _ *http.Request) {
			createHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"should-not-happen"}`))
		},
		"POST /containers/*/start": func(w http.ResponseWriter, _ *http.Request) {
			startHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	err := ProbeContainerEngine(context.Background(), ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		SocketPath: sock,
	})
	must.NoError(err)
	check.Zero(createHits.Load(), "a dry-run must not create a container")
	check.Zero(startHits.Load(), "a dry-run must not start a container")
}

func TestProbeContainerEngine_ImageAbsent(t *testing.T) {
	must := require.New(t)
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
		"GET /images/*/json": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such image"}`))
		},
	})
	defer cleanup()

	err := ProbeContainerEngine(context.Background(), ContainerExecutorConfig{
		Image:      "missing:latest",
		SocketPath: sock,
	})
	must.Error(err)
	must.ErrorContains(err, "not present locally")
}

// TestProbeContainerEngine_DisallowedImage asserts the dry-run preflight
// enforces the registry allowlist too, and fails fast before pinging the
// engine — so a misconfigured image is caught without a false-clean dry-run.
func TestProbeContainerEngine_DisallowedImage(t *testing.T) {
	must := require.New(t)
	var pingHits atomic.Int64
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			pingHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		},
		"GET /images/*/json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))
		},
	})
	defer cleanup()

	err := ProbeContainerEngine(context.Background(), ContainerExecutorConfig{
		Image:      "evil.example.com/malware:latest",
		SocketPath: sock,
	})
	must.Error(err)
	must.ErrorContains(err, "registry allowlist")
	must.Zero(pingHits.Load(), "a disallowed image must be rejected before any engine call")
}

func TestContainerExecutor_Probe_EngineUnreachable(t *testing.T) {
	must := require.New(t)
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"daemon error"}`))
		},
	})
	defer cleanup()

	exec := &ContainerExecutor{api: newContainerAPIClient(sock), image: "ubuntu:26.04"}
	err := exec.Probe(context.Background())
	must.Error(err)
	must.ErrorContains(err, "unreachable")
}
