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
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen on unix socket: %v", err)
	}

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
	deadline := time.Now().Add(timeout)
	for {
		if got := c.stdinSeen(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d stdin deliveries, saw %d", n, len(c.stdinSeen()))
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvWiring is the
// end-to-end factory test for executor.sandboxIdentity +
// executor.gitProxy on the container executor: the harness requests a
// sandbox_token_request, the created container's env carries the token
// var, its _FILE companion, and the four GIT_CONFIG_* pairs with a
// credential helper that reads the token file, the private token tmpfs is
// mounted, and the initial token is delivered over exec stdin — never in
// argv — before the loop is returned.
func TestBuildLoopWithTransport_SandboxIdentity_ContainerEnvWiring(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{respondToken: "the-jwt-token"}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-integration-test", server.URL)

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	env := capture.envMap()
	if got := env["HAYBALE_TOKEN"]; got != "the-jwt-token" {
		t.Errorf("HAYBALE_TOKEN = %q, want %q", got, "the-jwt-token")
	}
	if got := env["HAYBALE_TOKEN_FILE"]; got != executor.SandboxIdentityTokenPath {
		t.Errorf("HAYBALE_TOKEN_FILE = %q, want %q", got, executor.SandboxIdentityTokenPath)
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
	wantCredValue := `!f() { echo username=x-access-token; echo "password=$(cat ` + executor.SandboxIdentityTokenPath + `)"; }; f`
	if credValue != wantCredValue {
		t.Errorf("credential helper = %q, want %q", credValue, wantCredValue)
	}
	if strings.Contains(credValue, "the-jwt-token") || strings.Contains(credValue, "$HAYBALE_TOKEN") {
		t.Errorf("credential helper %q must read the token file, not the env var", credValue)
	}

	if _, mounted := capture.HostConfig.Tmpfs[executor.SandboxIdentityTokenDir]; !mounted {
		t.Errorf("container created without the token tmpfs at %s (Tmpfs=%v)", executor.SandboxIdentityTokenDir, capture.HostConfig.Tmpfs)
	}

	// The initial token reached the sandbox over exec stdin, exactly once,
	// and appeared in no exec argv.
	if got := capture.stdinSeen(); len(got) != 1 || got[0] != "the-jwt-token" {
		t.Errorf("exec stdin deliveries = %q, want exactly one carrying the token", got)
	}
	for _, argv := range capture.argvSeen() {
		for _, arg := range argv {
			if strings.Contains(arg, "the-jwt-token") {
				t.Errorf("token appeared in exec argv: %q", argv)
			}
		}
	}

	// Without an expiry the control plane sees exactly one request,
	// carrying the configured audience, and no refresh is scheduled.
	requests := tp.emittedOfType("sandbox_token_request")
	if len(requests) != 1 {
		t.Fatalf("expected exactly one sandbox_token_request, got %d", len(requests))
	}
	if requests[0].Audience != "https://haybale.internal" {
		t.Errorf("Audience = %q, want https://haybale.internal", requests[0].Audience)
	}
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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}

	deliveries := capture.waitForStdin(t, 2, 5*time.Second)
	if err := loop.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}

	if deliveries[0] != "jwt-1" || deliveries[1] != "jwt-2" {
		t.Errorf("stdin deliveries = %q, want the initial token followed by the refreshed one", deliveries)
	}
	if got := len(tp.emittedOfType("sandbox_token_request")); got < 2 {
		t.Errorf("expected at least 2 sandbox_token_requests, got %d", got)
	}
	if warnings := tp.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("unexpected warning(s) on a successful refresh: %+v", warnings)
	}

	tp.mu.Lock()
	emitted := append([]types.HarnessEvent(nil), tp.emitted...)
	tp.mu.Unlock()
	for _, e := range emitted {
		raw, _ := json.Marshal(e)
		for _, tok := range deliveries {
			if strings.Contains(string(raw), tok) {
				t.Errorf("emitted event carries a sandbox identity token: %s", raw)
			}
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
	if err == nil {
		t.Fatal("expected an error when sandboxIdentity is set but no transport was pre-established")
	}
	if !strings.Contains(err.Error(), "sandboxIdentity") {
		t.Errorf("error should reference sandboxIdentity, got: %v", err)
	}
}

// sandboxIdentityFailClosedConfig builds the shared RunConfig for the
// fail-closed integration tests below: a container executor (so
// fakeDockerEngine can observe whether /containers/create was ever hit) with
// executor.sandboxIdentity configured and no gitProxy (irrelevant to these
// failure modes, all of which abort before ComposeEnv runs). The provider
// BaseURL is a closed local port — never dialed, since every case here fails
// before the loop is otherwise assembled — mirroring
// TestBuildLoopWithTransport_SandboxIdentity_NilTransportFailsClosed above.
func sandboxIdentityFailClosedConfig(t *testing.T, runID string) *types.RunConfig {
	t.Helper()
	config := sandboxIdentityContainerConfig(t, runID, "http://127.0.0.1:1")
	config.Executor.GitProxy = nil
	return config
}

// TestBuildLoopWithTransport_SandboxIdentity_DeclineNoContainerCreate
// covers a control plane responding with IsError: true. A unit test on
// sandboxidentity.Exchange already proves Exchange itself fails closed on
// a decline; this test proves the *factory* honours that error before
// reaching the executor layer — a regression that swapped step order,
// dropped the error check, or logged-and-continued would pass every other
// test but must fail this one, because the fake Docker Engine below would
// then observe a real /containers/create call.
func TestBuildLoopWithTransport_SandboxIdentity_DeclineNoContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{respondIsError: true, respondReason: "no issuer configured"}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-decline-test")

	_, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err == nil {
		t.Fatal("expected an error when the control plane declines the sandbox identity exchange")
	}
	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("error should name the decline failure mode, got: %v", err)
	}
	if got := capture.createCallCount(); got != 0 {
		t.Errorf("fake Docker Engine recorded %d /containers/create call(s), want 0 (sandbox must not be created before a declined exchange)", got)
	}
}

// TestBuildLoopWithTransport_SandboxIdentity_TimeoutNoContainerCreate
// covers a control plane that never responds. BuildLoopWithTransport is
// invoked with a short-lived ctx (rather than waiting out
// sandboxidentity.DefaultTimeout's 60s) so the ctx branch of Exchange's
// underlying transport.Correlator.Await ends the wait quickly; the failure
// mode under test — cancellation/timeout aborting before sandbox creation —
// is the same one a real control-plane outage or network partition would
// trigger via the 60s default.
func TestBuildLoopWithTransport_SandboxIdentity_TimeoutNoContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{noRespond: true}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-timeout-test")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := BuildLoopWithTransport(ctx, config, tp)
	if err == nil {
		t.Fatal("expected an error when the control plane never responds")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should name the cancellation/timeout failure mode, got: %v", err)
	}
	if got := capture.createCallCount(); got != 0 {
		t.Errorf("fake Docker Engine recorded %d /containers/create call(s), want 0 (sandbox must not be created before a timed-out exchange)", got)
	}
}

// TestBuildLoopWithTransport_SandboxIdentity_OversizeTokenNoContainerCreate
// covers a control plane that responds successfully, but with a token
// exceeding sandboxidentity.MaxTokenBytes. Exchange treats this as a hard
// failure (never truncated-and-used); this test proves the factory aborts
// before sandbox creation rather than passing the oversized token through.
func TestBuildLoopWithTransport_SandboxIdentity_OversizeTokenNoContainerCreate(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	tp := &fakeControlPlaneTransport{respondToken: strings.Repeat("a", sandboxidentity.MaxTokenBytes+1)}
	config := sandboxIdentityFailClosedConfig(t, "sandboxidentity-oversize-test")

	_, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err == nil {
		t.Fatal("expected an error for an oversized sandbox identity token")
	}
	if !strings.Contains(err.Error(), "byte cap") {
		t.Errorf("error should name the oversize failure mode, got: %v", err)
	}
	if got := capture.createCallCount(); got != 0 {
		t.Errorf("fake Docker Engine recorded %d /containers/create call(s), want 0 (sandbox must not be created before an oversize-token failure)", got)
	}
}
