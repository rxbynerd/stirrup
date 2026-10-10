package cmd

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/transport/tlstest"
)

// startTLSControlPlane serves srv over TLS on a loopback listener and
// points CONTROL_PLANE_ADDR at it.
func startTLSControlPlane(t *testing.T, srv pb.HarnessServiceServer, serverTLS *tls.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	pb.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	t.Setenv("CONTROL_PLANE_ADDR", lis.Addr().String())
	return lis.Addr().String()
}

// countingListener accepts and immediately closes connections, counting
// them, so a test can prove nothing dialled it.
func countingListener(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = lis.Close() })
	return lis.Addr().String(), &accepted
}

func setControlPlaneTLSEnv(t *testing.T, cfg transport.TLSConfig) {
	t.Helper()
	t.Setenv(envControlPlaneTLSCAFile, cfg.CAFile)
	t.Setenv(envControlPlaneTLSCertFile, cfg.CertFile)
	t.Setenv(envControlPlaneTLSKeyFile, cfg.KeyFile)
	t.Setenv(envControlPlaneTLSServerName, cfg.ServerName)
}

func captureJobStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	jobCmd.SetErr(&buf)
	t.Cleanup(func() { jobCmd.SetErr(nil) })
	return &buf
}

func TestRunJob_TLSSettingErrorsFailBeforeDial(t *testing.T) {
	pki := tlstest.New(t)
	missing := filepath.Join(t.TempDir(), "absent-ca.pem")

	cases := []struct {
		name    string
		cfg     transport.TLSConfig
		wantErr string
	}{
		{"missing CA file", transport.TLSConfig{CAFile: missing}, "absent-ca.pem"},
		{"missing client certificate", transport.TLSConfig{CertFile: missing, KeyFile: pki.ClientKeyFile}, "absent-ca.pem"},
		{"certificate without key", transport.TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile}, "certificate and key must be set together"},
		{"key without certificate", transport.TLSConfig{KeyFile: pki.ClientKeyFile}, "certificate and key must be set together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTempMarkerPaths(t)
			addr, accepted := countingListener(t)
			t.Setenv("CONTROL_PLANE_ADDR", addr)
			setControlPlaneTLSEnv(t, tc.cfg)
			stderr := captureJobStderr(t)

			err := runJob(jobCmd, nil)
			if err == nil {
				t.Fatal("runJob succeeded, want a TLS configuration error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), envControlPlaneTLSCAFile) {
				t.Errorf("error = %q, want it to contain %q and name the TLS variables", err, tc.wantErr)
			}
			time.Sleep(50 * time.Millisecond)
			if n := accepted.Load(); n != 0 {
				t.Errorf("control plane accepted %d connections, want none before the TLS settings load", n)
			}
			if strings.Contains(stderr.String(), "control plane transport") {
				t.Errorf("startup mode was logged despite the TLS error: %q", stderr.String())
			}
		})
	}
}

func TestRunJob_MutualTLSReachesControlPlane(t *testing.T) {
	useTempMarkerPaths(t)
	pki := tlstest.New(t)
	srv := &fakeControlPlane{
		readyRecv: make(chan struct{}),
		task:      &pb.RunConfig{Mode: "bogus-mode"},
	}
	startTLSControlPlane(t, srv, pki.ServerTLSConfig(true))
	setControlPlaneTLSEnv(t, transport.TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	})
	stderr := captureJobStderr(t)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(eventWaitBudget):
		t.Fatal("control plane never received the ready event over mutual TLS")
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "building harness") {
			t.Fatalf("runJob() error = %v, want the building-harness failure from the assigned invalid RunConfig", err)
		}
	case <-time.After(eventWaitBudget):
		t.Fatal("runJob did not return after the task assignment")
	}

	log := stderr.String()
	if strings.Count(log, "control plane transport") != 1 || !strings.Contains(log, "mode=mutual-tls") {
		t.Errorf("stderr = %q, want exactly one startup line naming mode=mutual-tls", log)
	}
	assertNoPEMMaterial(t, log, pki)
}

func TestRunJob_TLSRefusesUntrustedControlPlanes(t *testing.T) {
	trusted := tlstest.New(t)
	impostor := tlstest.New(t)
	clientTLS := transport.TLSConfig{
		CAFile:   trusted.CAFile,
		CertFile: trusted.ClientCertFile,
		KeyFile:  trusted.ClientKeyFile,
	}

	cases := []struct {
		name    string
		start   func(t *testing.T, srv pb.HarnessServiceServer)
		wantErr string
	}{
		{
			name:    "plaintext server",
			start:   startControlPlane,
			wantErr: "first record does not look like a TLS handshake",
		},
		{
			name: "server from another CA",
			start: func(t *testing.T, srv pb.HarnessServiceServer) {
				startTLSControlPlane(t, srv, impostor.ServerTLSConfig(false))
			},
			wantErr: "certificate signed by unknown authority",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTempMarkerPaths(t)
			srv := &fakeControlPlane{readyRecv: make(chan struct{}), task: &pb.RunConfig{Mode: "bogus-mode"}}
			tc.start(t, srv)
			setControlPlaneTLSEnv(t, clientTLS)
			_ = captureJobStderr(t)

			err := runJob(jobCmd, nil)
			if err == nil {
				t.Fatal("runJob connected to an untrusted control plane")
			}
			if !strings.Contains(err.Error(), "failed to connect to control plane") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want a connect failure containing %q", err, tc.wantErr)
			}
			select {
			case <-srv.readyRecv:
				t.Error("control plane received a ready event over a refused connection")
			default:
			}
		})
	}
}

func TestRunJob_PlaintextDefaultLogsMode(t *testing.T) {
	useTempMarkerPaths(t)
	srv := &fakeControlPlane{readyRecv: make(chan struct{}), task: &pb.RunConfig{Mode: "bogus-mode"}}
	startControlPlane(t, srv)
	setControlPlaneTLSEnv(t, transport.TLSConfig{})
	stderr := captureJobStderr(t)

	if err := runJob(jobCmd, nil); err == nil || !strings.Contains(err.Error(), "building harness") {
		t.Fatalf("runJob() error = %v, want the building-harness failure from the assigned invalid RunConfig", err)
	}
	if !strings.Contains(stderr.String(), "control plane transport mode=plaintext") {
		t.Errorf("stderr = %q, want the plaintext startup line", stderr.String())
	}
}

func TestResolveTransportTLSFlags(t *testing.T) {
	pki := tlstest.New(t)
	missing := filepath.Join(t.TempDir(), "absent.pem")

	creds, err := resolveTransportTLSFlags(transport.TLSConfig{}, "stdio")
	if err != nil || creds != nil {
		t.Fatalf("plaintext default = (%v, %v), want (nil, nil)", creds, err)
	}
	creds, err = resolveTransportTLSFlags(transport.TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}, "grpc")
	if err != nil || creds == nil {
		t.Fatalf("mutual TLS = (%v, %v), want credentials", creds, err)
	}

	cases := []struct {
		name     string
		cfg      transport.TLSConfig
		typ      string
		wantCode int
		wantErr  string
	}{
		{"stdio transport", transport.TLSConfig{CAFile: pki.CAFile}, "stdio", exitUsage, "require --transport grpc"},
		{"default transport", transport.TLSConfig{ServerName: "cp.internal"}, "", exitUsage, "require --transport grpc"},
		{"certificate without key", transport.TLSConfig{CertFile: pki.ClientCertFile}, "grpc", exitUsage, "must be set together"},
		{"key without certificate", transport.TLSConfig{KeyFile: pki.ClientKeyFile}, "grpc", exitUsage, "must be set together"},
		{"missing CA file", transport.TLSConfig{CAFile: missing}, "grpc", exitIO, "absent.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveTransportTLSFlags(tc.cfg, tc.typ)
			if err == nil {
				t.Fatal("resolveTransportTLSFlags succeeded, want an error")
			}
			if got := classifyExitCode(err); got != tc.wantCode {
				t.Errorf("exit code = %d, want %d (err %v)", got, tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTransportTLSFlags_HarnessOnly(t *testing.T) {
	for _, name := range []string{flagTransportTLSCAFile, flagTransportTLSCertFile, flagTransportTLSKeyFile, flagTransportTLSServerName} {
		if harnessCmd.Flags().Lookup(name) == nil {
			t.Errorf("harness is missing --%s", name)
		}
		if runConfigCmd.Flags().Lookup(name) != nil {
			t.Errorf("run-config registers --%s, but TLS settings have no RunConfig field", name)
		}
	}
}

func TestRunHarness_TransportTLSRequiresGRPC(t *testing.T) {
	cmd := newTestHarnessCommand()
	mustSetFlags(t, cmd.Flags().Set, map[string]string{
		"prompt":               "hello",
		flagTransportTLSCAFile: "/nonexistent/ca.pem",
	})
	err := runHarness(cmd, nil)
	if err == nil || classifyExitCode(err) != exitUsage || !strings.Contains(err.Error(), "require --transport grpc") {
		t.Fatalf("runHarness() error = %v, want a usage error naming --transport grpc", err)
	}
}

// recordingTLSControlPlane records the type of every event a `stirrup
// harness --transport grpc` run sends.
type recordingTLSControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	mu     sync.Mutex
	events []string
}

func (s *recordingTLSControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.events = append(s.events, ev.Type)
		s.mu.Unlock()
	}
}

func (s *recordingTLSControlPlane) eventTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestRunHarness_MutualTLSTransport(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+
			"\n\ndata: "+
			`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+
			"\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()

	pki := tlstest.New(t)
	srv := &recordingTLSControlPlane{}
	addr := startTLSControlPlane(t, srv, pki.ServerTLSConfig(true))

	cmd := newTestHarnessCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	mustSetFlags(t, cmd.Flags().Set, map[string]string{
		"prompt":                 "Say hello.",
		"provider":               "openai-compatible",
		"base-url":               provider.URL,
		"model":                  "gpt-4o-mini",
		"api-key-ref":            "secret://TEST_OPENAI_KEY",
		"workspace":              t.TempDir(),
		"max-turns":              "1",
		"timeout":                "30",
		"output":                 "none",
		"transport":              "grpc",
		"transport-addr":         addr,
		flagTransportTLSCAFile:   pki.CAFile,
		flagTransportTLSCertFile: pki.ClientCertFile,
		flagTransportTLSKeyFile:  pki.ClientKeyFile,
	})

	if err := runHarness(cmd, nil); err != nil {
		t.Fatalf("runHarness over mutual TLS: %v", err)
	}

	deadline := time.Now().Add(eventWaitBudget)
	for !slices.Contains(srv.eventTypes(), "done") {
		if time.Now().After(deadline) {
			t.Fatalf("control plane events = %v, want a done event over mutual TLS", srv.eventTypes())
		}
		time.Sleep(5 * time.Millisecond)
	}

	log := stderr.String()
	if strings.Count(log, "control plane transport") != 1 || !strings.Contains(log, "mode=mutual-tls") {
		t.Errorf("stderr = %q, want exactly one startup line naming mode=mutual-tls", log)
	}
	assertNoPEMMaterial(t, log, pki)
}

func TestLogControlPlaneTransport(t *testing.T) {
	var buf bytes.Buffer
	logControlPlaneTransport(&buf, "cp:443", transport.TLSConfig{ServerName: "cp.internal"})
	if got, want := buf.String(), "stirrup: control plane transport mode=tls ca=system-roots server-name=cp.internal addr=cp:443\n"; got != want {
		t.Errorf("log line = %q, want %q", got, want)
	}
}

func assertNoPEMMaterial(t *testing.T, log string, pki *tlstest.PKI) {
	t.Helper()
	if strings.Contains(log, "-----BEGIN") {
		t.Errorf("stderr contains PEM material: %q", log)
	}
	key, err := os.ReadFile(pki.ClientKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(key), "\n") {
		if len(line) > 16 && !strings.HasPrefix(line, "-----") && strings.Contains(log, line) {
			t.Errorf("stderr contains client key material")
		}
	}
}

func mustSetFlags(t *testing.T, set func(name, value string) error, values map[string]string) {
	t.Helper()
	for name, value := range values {
		if err := set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
}
