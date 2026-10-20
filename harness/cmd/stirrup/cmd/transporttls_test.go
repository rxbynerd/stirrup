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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err, "listen on loopback")
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
	must := require.New(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err, "listen on loopback")
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

// setControlPlaneTLSEnv sets each non-empty field and leaves the rest
// unset; an empty field is "not requested", not "requested but empty".
func setControlPlaneTLSEnv(t *testing.T, cfg transport.TLSConfig) {
	t.Helper()
	must := require.New(t)
	for name, value := range map[string]string{
		envControlPlaneTLSCAFile:     cfg.CAFile,
		envControlPlaneTLSCertFile:   cfg.CertFile,
		envControlPlaneTLSKeyFile:    cfg.KeyFile,
		envControlPlaneTLSServerName: cfg.ServerName,
	} {
		t.Setenv(name, value)
		if value == "" {
			must.NoError(os.Unsetenv(name), "unset %s", name)
		}
	}
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
		{"missing ca file", transport.TLSConfig{CAFile: missing}, "absent-ca.pem"},
		{"missing client certificate", transport.TLSConfig{CertFile: missing, KeyFile: pki.ClientKeyFile}, "absent-ca.pem"},
		{"certificate without key", transport.TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile}, "certificate and key must be set together"},
		{"key without certificate", transport.TLSConfig{KeyFile: pki.ClientKeyFile}, "certificate and key must be set together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			useTempMarkerPaths(t)
			addr, accepted := countingListener(t)
			t.Setenv("CONTROL_PLANE_ADDR", addr)
			setControlPlaneTLSEnv(t, tc.cfg)
			stderr := captureJobStderr(t)

			err := runJob(jobCmd, nil)
			must.Error(err, "runJob should fail with a TLS configuration error")
			must.ErrorContains(err, tc.wantErr)
			must.ErrorContains(err, envControlPlaneTLSCAFile, "error should name the TLS variables")
			time.Sleep(50 * time.Millisecond)
			must.Zero(accepted.Load(), "control plane should see no connection before the TLS settings load")
			must.NotContains(stderr.String(), "control plane transport", "startup mode should not be logged")
		})
	}
}

func TestRunJob_EmptyTLSVariableFailsBeforeDial(t *testing.T) {
	for _, name := range []string{
		envControlPlaneTLSCAFile,
		envControlPlaneTLSCertFile,
		envControlPlaneTLSKeyFile,
		envControlPlaneTLSServerName,
	} {
		t.Run("empty "+name, func(t *testing.T) {
			must := require.New(t)
			useTempMarkerPaths(t)
			addr, accepted := countingListener(t)
			t.Setenv("CONTROL_PLANE_ADDR", addr)
			setControlPlaneTLSEnv(t, transport.TLSConfig{})
			t.Setenv(name, "")
			stderr := captureJobStderr(t)

			err := runJob(jobCmd, nil)
			must.Error(err, "runJob should fail on an empty TLS variable")
			must.ErrorContains(err, name)
			time.Sleep(50 * time.Millisecond)
			must.Zero(accepted.Load(), "control plane should see no connection")
			must.NotContains(stderr.String(), "control plane transport", "startup mode should not be logged")
		})
	}
}

func TestRunJob_MutualTLSReachesControlPlane(t *testing.T) {
	must := require.New(t)
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
		must.Fail("control plane never received the ready event over mutual TLS")
	}
	select {
	case err := <-errCh:
		must.ErrorContains(err, "building harness", "runJob should fail on the assigned invalid RunConfig")
	case <-time.After(eventWaitBudget):
		must.Fail("runJob did not return after the task assignment")
	}

	log := stderr.String()
	must.Equal(1, strings.Count(log, "control plane transport"), "stderr should have exactly one startup line")
	must.Contains(log, "mode=mutual-tls")
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
			name: "server from another ca",
			start: func(t *testing.T, srv pb.HarnessServiceServer) {
				startTLSControlPlane(t, srv, impostor.ServerTLSConfig(false))
			},
			wantErr: "certificate signed by unknown authority",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			useTempMarkerPaths(t)
			srv := &fakeControlPlane{readyRecv: make(chan struct{}), task: &pb.RunConfig{Mode: "bogus-mode"}}
			tc.start(t, srv)
			setControlPlaneTLSEnv(t, clientTLS)
			_ = captureJobStderr(t)

			err := runJob(jobCmd, nil)
			must.Error(err, "runJob should refuse an untrusted control plane")
			must.ErrorContains(err, "failed to connect to control plane")
			must.ErrorContains(err, tc.wantErr)
			select {
			case <-srv.readyRecv:
				must.Fail("control plane received a ready event over a refused connection")
			default:
			}
		})
	}
}

func TestRunJob_PlaintextDefaultLogsMode(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	srv := &fakeControlPlane{readyRecv: make(chan struct{}), task: &pb.RunConfig{Mode: "bogus-mode"}}
	startControlPlane(t, srv)
	setControlPlaneTLSEnv(t, transport.TLSConfig{})
	stderr := captureJobStderr(t)

	err := runJob(jobCmd, nil)
	must.ErrorContains(err, "building harness", "runJob should fail on the assigned invalid RunConfig")
	must.Contains(stderr.String(), "control plane transport mode=plaintext")
}

func TestResolveTransportTLSFlags_PlaintextAndMutualTLS(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)

	creds, err := resolveTransportTLSFlags(transport.TLSConfig{}, "stdio")
	must.NoError(err)
	must.Nil(creds, "plaintext should return no credentials")

	creds, err = resolveTransportTLSFlags(transport.TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}, "grpc")
	must.NoError(err)
	must.NotNil(creds, "mutual TLS should return credentials")
}

func TestResolveTransportTLSFlags_RejectsInvalidSettings(t *testing.T) {
	pki := tlstest.New(t)
	missing := filepath.Join(t.TempDir(), "absent.pem")

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
		{"missing ca file", transport.TLSConfig{CAFile: missing}, "grpc", exitIO, "absent.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := resolveTransportTLSFlags(tc.cfg, tc.typ)
			must.Error(err, "resolveTransportTLSFlags should fail")
			must.Equal(tc.wantCode, classifyExitCode(err), "exit code for error: %v", err)
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestTransportTLSFromFlags_UnsetIsPlaintextEmptyIsUsageError(t *testing.T) {
	names := []string{flagTransportTLSCAFile, flagTransportTLSCertFile, flagTransportTLSKeyFile, flagTransportTLSServerName}

	t.Run("all unset is plaintext", func(t *testing.T) {
		must := require.New(t)
		f := newTestHarnessCommand().Flags()
		cfg, err := transportTLSFromFlags(f)
		must.NoError(err)
		must.False(cfg.Enabled(), "all unset flags should disable TLS")
	})

	for _, name := range names {
		t.Run("empty "+name, func(t *testing.T) {
			must := require.New(t)
			f := newTestHarnessCommand().Flags()
			must.NoError(f.Set(name, ""), "set --%s", name)
			_, err := transportTLSFromFlags(f)
			must.ErrorContains(err, "--"+name)
			must.Equal(exitUsage, classifyExitCode(err), "exit code")
		})
	}
}

func TestRunHarness_EmptyTransportTLSFlagIsRejected(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	mustSetFlags(t, cmd.Flags().Set, map[string]string{
		"prompt":               "hello",
		flagTransportTLSCAFile: "",
	})
	err := runHarness(cmd, nil)
	must.ErrorContains(err, "--"+flagTransportTLSCAFile)
	must.Equal(exitUsage, classifyExitCode(err), "exit code")
}

func TestTransportTLSFlags_HarnessOnly(t *testing.T) {
	for _, name := range []string{flagTransportTLSCAFile, flagTransportTLSCertFile, flagTransportTLSKeyFile, flagTransportTLSServerName} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.NotNil(harnessCmd.Flags().Lookup(name), "harness should register --%s", name)
			must.Nil(runConfigCmd.Flags().Lookup(name), "run-config registers --%s, but TLS settings have no RunConfig field", name)
		})
	}
}

func TestRunHarness_TransportTLSRequiresGRPC(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	mustSetFlags(t, cmd.Flags().Set, map[string]string{
		"prompt":               "hello",
		flagTransportTLSCAFile: "/nonexistent/ca.pem",
	})
	err := runHarness(cmd, nil)
	must.ErrorContains(err, "require --transport grpc")
	must.Equal(exitUsage, classifyExitCode(err), "exit code")
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
	must := require.New(t)
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

	must.NoError(runHarness(cmd, nil), "runHarness over mutual TLS")

	must.EventuallyWithT(func(c *assert.CollectT) {
		assert.Contains(c, srv.eventTypes(), "done")
	}, eventWaitBudget, 5*time.Millisecond)

	log := stderr.String()
	must.Equal(1, strings.Count(log, "control plane transport"), "stderr should have exactly one startup line")
	must.Contains(log, "mode=mutual-tls")
	assertNoPEMMaterial(t, log, pki)
}

func TestLogControlPlaneTransport_WritesStartupLine(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logControlPlaneTransport(&buf, "cp:443", transport.TLSConfig{ServerName: "cp.internal"})
	must.Equal("stirrup: control plane transport mode=tls ca=system-roots server-name=cp.internal addr=cp:443\n", buf.String())
}

func assertNoPEMMaterial(t *testing.T, log string, pki *tlstest.PKI) {
	t.Helper()
	must := require.New(t)
	must.NotContains(log, "-----BEGIN", "stderr should not contain PEM material")
	key, err := os.ReadFile(pki.ClientKeyFile)
	must.NoError(err)
	for _, line := range strings.Split(string(key), "\n") {
		if len(line) > 16 && !strings.HasPrefix(line, "-----") {
			must.False(strings.Contains(log, line), "stderr should not contain client key material") //nolint:testifylint // NotContains would print the key line in the failure output
		}
	}
}

func mustSetFlags(t *testing.T, set func(name, value string) error, values map[string]string) {
	t.Helper()
	must := require.New(t)
	for name, value := range values {
		must.NoError(set(name, value), "set --%s", name)
	}
}
