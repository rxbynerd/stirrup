package transport

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/transport/tlstest"
	"github.com/rxbynerd/stirrup/types"
)

func TestTLSConfig_Mode(t *testing.T) {
	cases := []struct {
		name string
		cfg  TLSConfig
		want TLSMode
	}{
		{"empty is plaintext", TLSConfig{}, TLSModePlaintext},
		{"CA only is TLS", TLSConfig{CAFile: "ca.pem"}, TLSModeTLS},
		{"server name only is TLS", TLSConfig{ServerName: "cp.internal"}, TLSModeTLS},
		{"cert and key is mutual TLS", TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}, TLSModeMutualTLS},
		{"CA, cert and key is mutual TLS", TLSConfig{CAFile: "ca.pem", CertFile: "c.pem", KeyFile: "k.pem"}, TLSModeMutualTLS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Mode(); got != tc.want {
				t.Errorf("Mode() = %q, want %q", got, tc.want)
			}
			if got, want := tc.cfg.Enabled(), tc.want != TLSModePlaintext; got != want {
				t.Errorf("Enabled() = %v, want %v", got, want)
			}
		})
	}
}

func TestTLSConfig_ClientTLSConfigRequiresTLS13(t *testing.T) {
	pki := tlstest.New(t)
	for _, cfg := range []TLSConfig{
		{CAFile: pki.CAFile},
		{ServerName: "localhost"},
		{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile},
	} {
		tc, err := cfg.ClientTLSConfig()
		if err != nil {
			t.Fatalf("ClientTLSConfig(%+v): %v", cfg, err)
		}
		if tc.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x for %+v, want TLS 1.3 (%#x)", tc.MinVersion, cfg, tls.VersionTLS13)
		}
	}
}

func TestTLSConfig_ClientTLSConfigLoadsMaterial(t *testing.T) {
	pki := tlstest.New(t)

	tc, err := TLSConfig{CAFile: pki.CAFile, ServerName: "cp.internal"}.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if tc.RootCAs == nil {
		t.Error("RootCAs is nil, want the CA file's pool")
	}
	if tc.ServerName != "cp.internal" {
		t.Errorf("ServerName = %q, want cp.internal", tc.ServerName)
	}
	if len(tc.Certificates) != 0 {
		t.Errorf("Certificates = %d, want none without a client certificate", len(tc.Certificates))
	}

	tc, err = TLSConfig{CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if tc.RootCAs != nil {
		t.Error("RootCAs is set, want nil so the system roots verify the server")
	}
	if len(tc.Certificates) != 1 {
		t.Errorf("Certificates = %d, want the client certificate", len(tc.Certificates))
	}
}

func TestTLSConfig_ClientTLSConfigErrors(t *testing.T) {
	pki := tlstest.New(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	notPEM := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cfg     TLSConfig
		wantErr string
	}{
		{"disabled", TLSConfig{}, "TLS is not configured"},
		{"cert without key", TLSConfig{CertFile: pki.ClientCertFile}, "certificate and key must be set together"},
		{"key without cert", TLSConfig{CAFile: pki.CAFile, KeyFile: pki.ClientKeyFile}, "certificate and key must be set together"},
		{"missing CA file", TLSConfig{CAFile: missing}, "missing.pem"},
		{"CA file without PEM", TLSConfig{CAFile: notPEM}, "contains no PEM certificates"},
		{"missing cert file", TLSConfig{CertFile: missing, KeyFile: pki.ClientKeyFile}, "missing.pem"},
		{"missing key file", TLSConfig{CertFile: pki.ClientCertFile, KeyFile: missing}, "missing.pem"},
		{"mismatched cert and key", TLSConfig{CertFile: pki.ClientCertFile, KeyFile: pki.ServerKeyFile}, "load client certificate and key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Credentials()
			if err == nil {
				t.Fatal("Credentials() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTLSConfig_SummaryOmitsFileContents(t *testing.T) {
	pki := tlstest.New(t)
	cases := []struct {
		cfg  TLSConfig
		want []string
	}{
		{TLSConfig{}, []string{"plaintext"}},
		{TLSConfig{ServerName: "cp.internal"}, []string{"mode=tls", "ca=system-roots", "server-name=cp.internal"}},
		{TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}, []string{"mode=mutual-tls", "ca=" + pki.CAFile}},
	}
	for _, tc := range cases {
		got := tc.cfg.Summary()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("Summary() = %q, want it to contain %q", got, w)
			}
		}
		if strings.Contains(got, "BEGIN") || strings.Contains(got, pki.ClientKeyFile) {
			t.Errorf("Summary() = %q, must not carry PEM content or the key path", got)
		}
	}
}

// peerRecordingServer records the client certificate common name the
// server saw on the stream and every event it receives.
type peerRecordingServer struct {
	pb.UnimplementedHarnessServiceServer

	mu       sync.Mutex
	clientCN string
	received []*pb.HarnessEvent
}

func (s *peerRecordingServer) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if p, ok := peer.FromContext(stream.Context()); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			s.mu.Lock()
			s.clientCN = info.State.PeerCertificates[0].Subject.CommonName
			s.mu.Unlock()
		}
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
	}
}

func (s *peerRecordingServer) snapshot() (string, []*pb.HarnessEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientCN, append([]*pb.HarnessEvent(nil), s.received...)
}

// startTCPServer serves srv on a loopback TCP listener; a nil serverTLS
// serves plaintext.
func startTCPServer(t *testing.T, srv pb.HarnessServiceServer, serverTLS *tls.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	var opts []grpc.ServerOption
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func dialWithTLS(t *testing.T, addr string, cfg TLSConfig) (*GRPCTransport, error) {
	t.Helper()
	creds, err := cfg.Credentials()
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return NewGRPCTransport(ctx, addr, WithTLSCredentials(creds))
}

func TestGRPCTransport_MutualTLSSucceeds(t *testing.T) {
	pki := tlstest.New(t)
	srv := &peerRecordingServer{}
	addr := startTCPServer(t, srv, pki.ServerTLSConfig(true))

	tr, err := dialWithTLS(t, addr, TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	})
	if err != nil {
		t.Fatalf("NewGRPCTransport over mutual TLS: %v", err)
	}
	if err := tr.Emit(types.HarnessEvent{Type: "ready"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		cn, received := srv.snapshot()
		if len(received) == 1 {
			if received[0].Type != "ready" {
				t.Errorf("received type %q, want ready", received[0].Type)
			}
			if cn != tlstest.ClientCommonName {
				t.Errorf("server saw client CN %q, want %q", cn, tlstest.ClientCommonName)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server received %d events, want 1", len(received))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGRPCTransport_ServerAuthenticatedTLSSucceeds(t *testing.T) {
	pki := tlstest.New(t)
	addr := startTCPServer(t, &peerRecordingServer{}, pki.ServerTLSConfig(false))

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	if err != nil {
		t.Fatalf("NewGRPCTransport over TLS: %v", err)
	}
	_ = tr.Close()
}

func TestGRPCTransport_TLSRefusesPlaintextServer(t *testing.T) {
	pki := tlstest.New(t)
	addr := startTCPServer(t, &peerRecordingServer{}, nil)

	tr, err := dialWithTLS(t, addr, TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	})
	if err == nil {
		_ = tr.Close()
		t.Fatal("NewGRPCTransport succeeded against a plaintext server, want a handshake failure")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("error = %q, want it to name the TLS handshake failure", err)
	}
}

func TestGRPCTransport_TLSRefusesServerFromOtherCA(t *testing.T) {
	trusted := tlstest.New(t)
	impostor := tlstest.New(t)
	addr := startTCPServer(t, &peerRecordingServer{}, impostor.ServerTLSConfig(false))

	tr, err := dialWithTLS(t, addr, TLSConfig{
		CAFile:   trusted.CAFile,
		CertFile: trusted.ClientCertFile,
		KeyFile:  trusted.ClientKeyFile,
	})
	if err == nil {
		_ = tr.Close()
		t.Fatal("NewGRPCTransport accepted a server certificate from an untrusted CA")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Errorf("error = %q, want it to name the untrusted certificate authority", err)
	}
}

func TestGRPCTransport_TLSRefusesServerBelowTLS13(t *testing.T) {
	pki := tlstest.New(t)
	serverTLS := pki.ServerTLSConfig(false)
	serverTLS.MaxVersion = tls.VersionTLS12
	addr := startTCPServer(t, &peerRecordingServer{}, serverTLS)

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	if err == nil {
		_ = tr.Close()
		t.Fatal("NewGRPCTransport negotiated with a TLS 1.2-only server, want TLS 1.3 minimum")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("error = %q, want a protocol version failure", err)
	}
}
