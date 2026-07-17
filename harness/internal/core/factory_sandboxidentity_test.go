package core

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// fakeControlPlaneTransport is a minimal transport.Transport fake standing
// in for the control plane's gRPC stream. It auto-responds to a
// sandbox_token_request with a configurable sandbox_token_response,
// mirroring permission/askupstream_test.go's mockTransport — the wire-level
// proto round-trip for the new sandbox_token_request/response fields
// (Audience, Token, ExpiresAt, IsError) is already covered by PR A's
// harness/internal/transport/grpc_translate_test.go, so this integration
// test exercises the seam BuildLoopWithTransport actually consumes
// (transport.Transport) rather than re-dialing a real gRPC connection.
type fakeControlPlaneTransport struct {
	mu       sync.Mutex
	handlers []func(types.ControlEvent)
	emitted  []types.HarnessEvent

	respondToken     string
	respondExpiresAt *int64
}

func (f *fakeControlPlaneTransport) Emit(event types.HarnessEvent) error {
	f.mu.Lock()
	f.emitted = append(f.emitted, event)
	f.mu.Unlock()

	if event.Type == "sandbox_token_request" {
		f.deliver(types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: event.RequestID,
			Token:     f.respondToken,
			ExpiresAt: f.respondExpiresAt,
		})
	}
	return nil
}

func (f *fakeControlPlaneTransport) OnControl(handler func(types.ControlEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, handler)
}

func (f *fakeControlPlaneTransport) Close() error { return nil }

func (f *fakeControlPlaneTransport) deliver(event types.ControlEvent) {
	f.mu.Lock()
	handlers := make([]func(types.ControlEvent), len(f.handlers))
	copy(handlers, f.handlers)
	f.mu.Unlock()
	for _, h := range handlers {
		h(event)
	}
}

// fakeDockerEngine stands up a minimal fake Docker Engine API server on a
// temporary Unix socket, capturing the body of the /containers/create
// request. Mirrors executor.mockEngineServer, duplicated here because that
// helper is unexported test-only code in a different package.
func fakeDockerEngine(t *testing.T) (socketPath string, createBody *containerCreateCapture, cleanup func()) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "sbid-de-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen on unix socket: %v", err)
	}

	capture := &containerCreateCapture{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiPath := r.URL.Path
		if idx := strings.Index(apiPath[1:], "/"); idx >= 0 {
			apiPath = apiPath[idx+1:]
		}

		switch {
		case r.Method == http.MethodPost && apiPath == "/containers/create":
			_ = json.NewDecoder(r.Body).Decode(capture)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"test-container-id"}`))
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/containers/") && strings.HasSuffix(apiPath, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/containers/") && strings.HasSuffix(apiPath, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(apiPath, "/containers/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()

	return sock, capture, func() {
		_ = srv.Close()
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	}
}

// containerCreateCapture decodes only the fields this test needs from the
// Docker Engine API's POST /containers/create request body.
type containerCreateCapture struct {
	Env []string `json:"Env"`
}

func (c *containerCreateCapture) envMap() map[string]string {
	out := map[string]string{}
	for _, kv := range c.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

// TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvWiring is the
// issue #516 acceptance integration test: given executor.sandboxIdentity +
// executor.gitProxy, transport=grpc, executor=container, the harness
// requests a sandbox_token_request after the fake control plane's
// task_assignment-equivalent (BuildLoopWithTransport is invoked directly
// with the RunConfig, matching how stirrup job calls it post-assignment),
// and the created container's env carries the token var plus the four
// GIT_CONFIG_* pairs the issue's canonical example specifies.
func TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvWiring(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{respondToken: "the-jwt-token"}

	timeout := 30
	config := &types.RunConfig{
		RunID:           "sandboxidentity-integration-test",
		Mode:            "execution",
		Prompt:          "hello",
		Provider:        types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window"},
		Executor: types.ExecutorConfig{
			Type:      "container",
			Image:     "ubuntu:26.04",
			Workspace: t.TempDir(),
			Network:   &types.NetworkConfig{Mode: "none"},
			SandboxIdentity: &types.SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
				EnvVar:   "HAYBALE_TOKEN",
			},
			GitProxy: &types.GitProxyConfig{
				URL:         "http://haybale.internal:8466",
				Hosts:       []string{"github.com"},
				RewriteSsh:  true,
				TokenEnvVar: "HAYBALE_TOKEN",
			},
		},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "grpc"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	env := capture.envMap()
	if got := env["HAYBALE_TOKEN"]; got != "the-jwt-token" {
		t.Errorf("HAYBALE_TOKEN = %q, want %q", got, "the-jwt-token")
	}
	if got := env["GIT_CONFIG_COUNT"]; got != "4" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want %q", got, "4")
	}

	wantInsteadOfKey := "url.http://haybale.internal:8466/github.com/.insteadOf"
	wantValues := map[string]bool{
		"https://github.com/":   false,
		"git@github.com:":       false,
		"ssh://git@github.com/": false,
	}
	credKey := "credential.http://haybale.internal:8466/.helper"
	var credValue string
	for i := 0; i < 4; i++ {
		key := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
		value := env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
		switch key {
		case wantInsteadOfKey:
			wantValues[value] = true
		case credKey:
			credValue = value
		default:
			t.Errorf("unexpected GIT_CONFIG_KEY_%d %q", i, key)
		}
	}
	for v, seen := range wantValues {
		if !seen {
			t.Errorf("missing insteadOf value %q", v)
		}
	}
	wantCredValue := `!f() { echo username=x-access-token; echo "password=$HAYBALE_TOKEN"; }; f`
	if credValue != wantCredValue {
		t.Errorf("credential helper = %q, want %q", credValue, wantCredValue)
	}

	// The control plane must have seen exactly one sandbox_token_request,
	// carrying the configured audience.
	var requests []types.HarnessEvent
	for _, e := range tp.emitted {
		if e.Type == "sandbox_token_request" {
			requests = append(requests, e)
		}
	}
	if len(requests) != 1 {
		t.Fatalf("expected exactly one sandbox_token_request, got %d", len(requests))
	}
	if requests[0].Audience != "https://haybale.internal" {
		t.Errorf("Audience = %q, want https://haybale.internal", requests[0].Audience)
	}
}

// TestBuildLoopWithTransport_SandboxIdentity_NilTransportFailsClosed pins
// the defensive guard: sandboxIdentity is only usable against a
// pre-established transport (the control-plane job entrypoint). A nil tp
// must fail closed before any sandbox is created, even though
// ValidateRunConfig has already accepted transport.type=grpc.
func TestBuildLoopWithTransport_SandboxIdentity_NilTransportFailsClosed(t *testing.T) {
	timeout := 30
	config := &types.RunConfig{
		RunID:           "sandboxidentity-nil-transport-test",
		Mode:            "execution",
		Prompt:          "hello",
		Provider:        types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: "http://127.0.0.1:1"},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window"},
		Executor: types.ExecutorConfig{
			Type:      "container",
			Image:     "ubuntu:26.04",
			Workspace: t.TempDir(),
			Network:   &types.NetworkConfig{Mode: "none"},
			SandboxIdentity: &types.SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
			},
		},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "grpc", Address: "127.0.0.1:1"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	_, err := BuildLoopWithTransport(context.Background(), config, nil)
	if err == nil {
		t.Fatal("expected an error when sandboxIdentity is set but no transport was pre-established")
	}
	if !strings.Contains(err.Error(), "sandboxIdentity") {
		t.Errorf("error should reference sandboxIdentity, got: %v", err)
	}
}
