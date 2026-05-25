package executor

import (
	"context"
	"net/http"
	"strings"
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
