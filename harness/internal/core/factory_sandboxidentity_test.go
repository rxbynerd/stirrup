package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/sandboxidentity"
	"github.com/rxbynerd/stirrup/types"
)

func boolPtr(b bool) *bool { return &b }

// fakeControlPlaneTransport is a minimal transport.Transport fake standing
// in for the control plane's gRPC stream. It auto-responds to every
// sandbox_token_request with a configurable sandbox_token_response,
// mirroring permission/askupstream_test.go's mockTransport — the wire-level
// proto round-trip for the sandbox_token_request/response fields is covered
// by harness/internal/transport/grpc_translate_test.go, so these
// integration tests exercise the seam BuildLoopWithTransport actually
// consumes (transport.Transport) rather than re-dialing a real gRPC
// connection.
type fakeControlPlaneTransport struct {
	mu              sync.Mutex
	handlers        []func(types.ControlEvent)
	emitted         []types.HarnessEvent
	requests        int
	closed          bool
	emitsAfterClose int

	respondToken string
	// respondTokenPerRequest, when true, suffixes respondToken with the
	// request ordinal so each refresh is issued a distinguishable token.
	respondTokenPerRequest bool
	// respondTTL, when positive, sets expires_at to now+respondTTL on every
	// response; zero leaves expires_at unset.
	respondTTL time.Duration

	// respondIsError/respondReason, when respondIsError is true, deliver a
	// control-plane decline instead of a success response.
	respondIsError bool
	respondReason  string
	// noRespond, when true, never delivers a response at all — the fake
	// control plane emits no sandbox_token_response, simulating a hung or
	// unreachable control plane so the caller's ctx/timeout is what ends
	// the wait.
	noRespond bool
}

func (f *fakeControlPlaneTransport) Emit(event types.HarnessEvent) error {
	f.mu.Lock()
	if f.closed {
		f.emitsAfterClose++
		f.mu.Unlock()
		return fmt.Errorf("transport closed")
	}
	f.emitted = append(f.emitted, event)
	noRespond := f.noRespond
	if event.Type == "sandbox_token_request" {
		f.requests++
	}
	ordinal := f.requests
	f.mu.Unlock()

	if event.Type == "sandbox_token_request" && !noRespond {
		resp := types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: event.RequestID,
			Token:     f.tokenFor(ordinal),
		}
		if f.respondTTL > 0 {
			exp := time.Now().Add(f.respondTTL).Unix()
			resp.ExpiresAt = &exp
		}
		if f.respondIsError {
			resp.IsError = boolPtr(true)
			resp.Reason = f.respondReason
		}
		f.deliver(resp)
	}
	return nil
}

func (f *fakeControlPlaneTransport) tokenFor(ordinal int) string {
	if f.respondTokenPerRequest {
		return fmt.Sprintf("%s-%d", f.respondToken, ordinal)
	}
	return f.respondToken
}

func (f *fakeControlPlaneTransport) OnControl(handler func(types.ControlEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, handler)
}

func (f *fakeControlPlaneTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeControlPlaneTransport) emittedAfterClose() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emitsAfterClose
}

func (f *fakeControlPlaneTransport) handlerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handlers)
}

func (f *fakeControlPlaneTransport) deliver(event types.ControlEvent) {
	f.mu.Lock()
	handlers := make([]func(types.ControlEvent), len(f.handlers))
	copy(handlers, f.handlers)
	f.mu.Unlock()
	for _, h := range handlers {
		h(event)
	}
}

func (f *fakeControlPlaneTransport) emittedOfType(eventType string) []types.HarnessEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.HarnessEvent
	for _, e := range f.emitted {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// fakeDockerEngine stands up a minimal fake Docker Engine API server on a
// temporary Unix socket, capturing the body of the /containers/create
// request and every exec's argv and hijacked stdin. Mirrors
// executor.mockEngineServer and its stdin exec handlers, duplicated here
// because those helpers are unexported test-only code in a different
// package.
func fakeDockerEngine(t *testing.T) (socketPath string, capture *engineCapture, cleanup func()) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "sbid-de-")
	require.NoError(t, err, "create temp dir")
	sock := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
	}
	require.NoError(t, err, "listen on unix socket")

	capture = &engineCapture{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiPath := r.URL.Path
		if idx := strings.Index(apiPath[1:], "/"); idx >= 0 {
			apiPath = apiPath[idx+1:]
		}

		switch {
		case r.Method == http.MethodPost && apiPath == "/containers/create":
			capture.recordCreateCall()
			_ = json.NewDecoder(r.Body).Decode(capture)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"test-container-id"}`))
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/containers/") && strings.HasSuffix(apiPath, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/containers/") && strings.HasSuffix(apiPath, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(apiPath, "/containers/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/containers/") && strings.HasSuffix(apiPath, "/exec"):
			var req struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			capture.recordExecArgv(req.Cmd)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-id"}`))
		case r.Method == http.MethodPost && strings.HasPrefix(apiPath, "/exec/") && strings.HasSuffix(apiPath, "/start"):
			_, _ = io.ReadAll(r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok || r.Header.Get("Upgrade") != "tcp" {
				http.Error(w, `{"message":"expected upgrade"}`, http.StatusBadRequest)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = rw.Flush()
			in, _ := io.ReadAll(rw)
			capture.recordStdin(string(in))
			_ = conn.Close()
		case r.Method == http.MethodGet && strings.HasPrefix(apiPath, "/exec/") && strings.HasSuffix(apiPath, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ExitCode":0,"Running":false}`))
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

// engineCapture decodes only the fields these tests need from the Docker
// Engine API's POST /containers/create request body and records the exec
// traffic. createCalls (guarded by mu, since the fake server's handler
// runs on its own goroutine) counts how many times /containers/create was
// hit — the fail-closed integration tests assert this stays zero.
type engineCapture struct {
	Env        []string `json:"Env"`
	HostConfig struct {
		Tmpfs map[string]string `json:"Tmpfs"`
	} `json:"HostConfig"`

	mu          sync.Mutex
	createCalls int
	execArgv    [][]string
	stdin       []string
}

func (c *engineCapture) envMap() map[string]string {
	out := map[string]string{}
	for _, kv := range c.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func (c *engineCapture) recordCreateCall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createCalls++
}

func (c *engineCapture) createCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createCalls
}

func (c *engineCapture) recordExecArgv(argv []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execArgv = append(c.execArgv, argv)
}

func (c *engineCapture) recordStdin(in string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdin = append(c.stdin, in)
}

func (c *engineCapture) stdinSeen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stdin...)
}

func (c *engineCapture) argvSeen() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]string(nil), c.execArgv...)
}

// waitForStdin blocks until the engine has seen n stdin deliveries.
func (c *engineCapture) waitForStdin(t *testing.T, n int, timeout time.Duration) []string {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.GreaterOrEqual(collect, len(c.stdinSeen()), n, "stdin deliveries")
	}, timeout, 10*time.Millisecond)
	return c.stdinSeen()
}

func sandboxIdentityContainerConfig(t *testing.T, runID, providerURL string) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	timeout := 30
	return &types.RunConfig{
		RunID:           runID,
		Mode:            "execution",
		Prompt:          "hello",
		Provider:        types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: providerURL},
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
}

// TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvAndGitConfig is the
// end-to-end factory test for sandboxIdentity and gitProxy on the container
// executor. The token reaches the container as env vars and a token file, the
// git config rewrites use a credential helper that reads that file, the
// token tmpfs is mounted, and the initial token arrives over exec stdin and
// never in exec argv.
func TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvAndGitConfig(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{respondToken: "the-jwt-token"}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-integration-test", server.URL)

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	env := capture.envMap()

	t.Run("token env vars", func(t *testing.T) {
		require.Equal(t, "the-jwt-token", env["HAYBALE_TOKEN"])
		require.Equal(t, executor.SandboxIdentityTokenPath, env["HAYBALE_TOKEN_FILE"])
	})

	t.Run("git config rewrites and credential helper", func(t *testing.T) {
		insteadOfKey := "url.http://haybale.internal:8466/github.com/.insteadOf"
		credKey := "credential.http://haybale.internal:8466/.helper"
		wantCredValue := `!f() { echo username=x-access-token; echo "password=$(cat ` + executor.SandboxIdentityTokenPath + `)"; }; f`

		require.Equal(t, "4", env["GIT_CONFIG_COUNT"])

		var insteadOfValues []string
		var credValue string
		for i := 0; i < 4; i++ {
			key := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
			value := env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
			switch key {
			case insteadOfKey:
				insteadOfValues = append(insteadOfValues, value)
			case credKey:
				credValue = value
			default:
				require.Failf(t, "unexpected git config key", "GIT_CONFIG_KEY_%d = %q", i, key)
			}
		}

		require.Subset(t, insteadOfValues, []string{"https://github.com/", "git@github.com:", "ssh://git@github.com/"})
		require.Equal(t, wantCredValue, credValue)
		require.NotContains(t, credValue, "the-jwt-token", "credential helper must read the token file, not the env var")
		require.NotContains(t, credValue, "$HAYBALE_TOKEN", "credential helper must read the token file, not the env var")
	})

	t.Run("token tmpfs mounted", func(t *testing.T) {
		require.Contains(t, capture.HostConfig.Tmpfs, executor.SandboxIdentityTokenDir, "container created without the token tmpfs")
	})

	t.Run("token delivered over stdin, not argv", func(t *testing.T) {
		require.Equal(t, []string{"the-jwt-token"}, capture.stdinSeen(), "exec stdin deliveries")
		for _, argv := range capture.argvSeen() {
			for _, arg := range argv {
				require.NotContains(t, arg, "the-jwt-token", "token appeared in exec argv")
			}
		}
	})

	t.Run("single token request with audience", func(t *testing.T) {
		// Without an expiry the control plane sees exactly one request and no refresh is scheduled.
		requests := tp.emittedOfType("sandbox_token_request")
		require.Len(t, requests, 1)
		require.Equal(t, "https://haybale.internal", requests[0].Audience)
	})
}

// TestBuildLoopWithTransport_SandboxIdentity_RefreshesToken drives the
// factory-wired refresher: with a short expires_at the harness sends a
// second sandbox_token_request before expiry, delivers the new token over
// exec stdin, and never lets either token into an emitted event.
func TestBuildLoopWithTransport_SandboxIdentity_RefreshesToken(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{
		respondToken:           "jwt",
		respondTokenPerRequest: true,
		respondTTL:             2 * time.Second,
	}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-refresh-test", server.URL)

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	require.NoError(t, err)

	deliveries := capture.waitForStdin(t, 2, 5*time.Second)
	require.NoError(t, loop.Close())

	require.Equal(t, []string{"jwt-1", "jwt-2"}, deliveries[:2], "initial token followed by the refreshed token")
	require.GreaterOrEqual(t, len(tp.emittedOfType("sandbox_token_request")), 2)
	require.Empty(t, tp.emittedOfType("warning"), "no warning on a successful refresh")

	tp.mu.Lock()
	emitted := append([]types.HarnessEvent(nil), tp.emitted...)
	tp.mu.Unlock()
	for _, e := range emitted {
		raw, _ := json.Marshal(e)
		for _, tok := range deliveries {
			require.NotContains(t, string(raw), tok, "emitted event carries a sandbox identity token")
		}
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
	require.Error(t, err, "expected an error when sandboxIdentity is set but no transport was pre-established")
	require.ErrorContains(t, err, "sandboxIdentity")
}

// sandboxIdentityFailClosedConfig returns a container RunConfig with
// sandboxIdentity set and no gitProxy, for failure cases that abort before
// the loop is assembled. The provider BaseURL is a closed local port and is
// never dialed.
func sandboxIdentityFailClosedConfig(t *testing.T, runID string) *types.RunConfig {
	t.Helper()
	config := sandboxIdentityContainerConfig(t, runID, "http://127.0.0.1:1")
	config.Executor.GitProxy = nil
	return config
}

// TestBuildLoopWithTransport_SandboxIdentity_DeclineSkipsContainerCreate
// covers a control plane that answers with IsError: true. The factory must
// reject the decline before any sandbox is created; otherwise the fake Docker
// Engine would record a /containers/create call.
func TestBuildLoopWithTransport_SandboxIdentity_DeclineSkipsContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{respondIsError: true, respondReason: "no issuer configured"}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-decline-test")

	_, err := BuildLoopWithTransport(context.Background(), config, tp)
	require.Error(t, err, "expected an error when the control plane declines the sandbox identity exchange")
	require.ErrorContains(t, err, "declined")
	require.Zero(t, capture.createCallCount(), "sandbox must not be created before a declined exchange")
}

// TestBuildLoopWithTransport_SandboxIdentity_TimeoutSkipsContainerCreate
// covers a control plane that never responds. The test uses a short context
// deadline instead of sandboxidentity.DefaultTimeout so the wait ends quickly.
// Cancellation must abort before sandbox creation, as it would during a real
// control-plane outage.
func TestBuildLoopWithTransport_SandboxIdentity_TimeoutSkipsContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{noRespond: true}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-timeout-test")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := BuildLoopWithTransport(ctx, config, tp)
	require.Error(t, err, "expected an error when the control plane never responds")
	require.ErrorContains(t, err, "cancelled")
	require.Zero(t, capture.createCallCount(), "sandbox must not be created before a timed-out exchange")
}

// TestBuildLoopWithTransport_SandboxIdentity_OversizeTokenSkipsContainerCreate
// covers a control plane that responds successfully, but with a token
// exceeding sandboxidentity.MaxTokenBytes. Exchange treats this as a hard
// failure (never truncated-and-used); this test proves the factory aborts
// before sandbox creation rather than passing the oversized token through.
func TestBuildLoopWithTransport_SandboxIdentity_OversizeTokenSkipsContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{respondToken: strings.Repeat("a", sandboxidentity.MaxTokenBytes+1)}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-oversize-test")

	_, err := BuildLoopWithTransport(context.Background(), config, tp)
	require.Error(t, err, "expected an error for an oversized sandbox identity token")
	require.ErrorContains(t, err, "byte cap")
	require.Zero(t, capture.createCallCount(), "sandbox must not be created before an oversize-token failure")
}
