package executor

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestContainerExecutor_Probe_OK(t *testing.T) {
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
	if err := exec.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
}

func TestContainerExecutor_Probe_ImageAbsent(t *testing.T) {
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
	if err == nil {
		t.Fatal("Probe: expected error for absent image, got nil")
	}
	if !strings.Contains(err.Error(), "not present locally") || !strings.Contains(err.Error(), "missing:latest") {
		t.Errorf("error should name the missing image with a pull hint, got: %v", err)
	}
}

// TestProbeContainerEngine_NoContainerCreated asserts a container dry-run
// pings + inspects the image but never creates or starts a container, and
// never touches the egress proxy. The mock engine fails the test if it sees
// a create/start request.
func TestProbeContainerEngine_NoContainerCreated(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ProbeContainerEngine: unexpected error: %v", err)
	}
	if got := createHits.Load(); got != 0 {
		t.Errorf("container create hit %d times; a dry-run must not create a container", got)
	}
	if got := startHits.Load(); got != 0 {
		t.Errorf("container start hit %d times; a dry-run must not start a container", got)
	}
}

func TestProbeContainerEngine_ImageAbsent(t *testing.T) {
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
	if err == nil {
		t.Fatal("ProbeContainerEngine: expected error for absent image")
	}
	if !strings.Contains(err.Error(), "not present locally") {
		t.Errorf("error should report the absent image, got: %v", err)
	}
}

// TestProbeContainerEngine_DisallowedImage asserts the dry-run preflight
// enforces the registry allowlist too, and fails fast before pinging the
// engine — so a misconfigured image is caught without a false-clean dry-run.
func TestProbeContainerEngine_DisallowedImage(t *testing.T) {
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
	if err == nil {
		t.Fatal("ProbeContainerEngine: expected error for disallowed image")
	}
	if !strings.Contains(err.Error(), "registry allowlist") {
		t.Errorf("error should mention the registry allowlist, got: %v", err)
	}
	if got := pingHits.Load(); got != 0 {
		t.Errorf("engine pinged %d times; a disallowed image must be rejected before any engine call", got)
	}
}

func TestContainerExecutor_Probe_EngineUnreachable(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"daemon error"}`))
		},
	})
	defer cleanup()

	exec := &ContainerExecutor{api: newContainerAPIClient(sock), image: "ubuntu:26.04"}
	err := exec.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe: expected error for unreachable engine, got nil")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error should mention engine unreachable, got: %v", err)
	}
}
