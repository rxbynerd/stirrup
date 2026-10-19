package transport

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
		{"ca only is tls", TLSConfig{CAFile: "ca.pem"}, TLSModeTLS},
		{"server name only is tls", TLSConfig{ServerName: "cp.internal"}, TLSModeTLS},
		{"cert and key is mutual tls", TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}, TLSModeMutualTLS},
		{"ca with cert and key is mutual tls", TLSConfig{CAFile: "ca.pem", CertFile: "c.pem", KeyFile: "k.pem"}, TLSModeMutualTLS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.cfg.Mode())
			require.Equal(t, tc.want != TLSModePlaintext, tc.cfg.Enabled())
		})
	}
}

func TestTLSConfig_ClientTLSConfig_RequiresTLS13(t *testing.T) {
	pki := tlstest.New(t)
	cases := []struct {
		name string
		cfg  TLSConfig
	}{
		{"ca only", TLSConfig{CAFile: pki.CAFile}},
		{"server name only", TLSConfig{ServerName: "localhost"}},
		{"mutual tls", TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg, err := tc.cfg.ClientTLSConfig()
			require.NoError(t, err)
			require.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion)
		})
	}
}

func TestTLSConfig_ClientTLSConfig_LoadsMaterial(t *testing.T) {
	pki := tlstest.New(t)

	serverAuth, err := TLSConfig{CAFile: pki.CAFile, ServerName: "cp.internal"}.ClientTLSConfig()
	require.NoError(t, err)
	require.NotNil(t, serverAuth.RootCAs, "the CA file's pool should be loaded")
	require.Equal(t, "cp.internal", serverAuth.ServerName)
	require.Empty(t, serverAuth.Certificates, "no client certificate is configured")

	clientCert, err := TLSConfig{CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}.ClientTLSConfig()
	require.NoError(t, err)
	require.Nil(t, clientCert.RootCAs, "system roots must verify the server")
	require.Len(t, clientCert.Certificates, 1)
}

func TestTLSConfig_Credentials_RejectsInvalidConfig(t *testing.T) {
	pki := tlstest.New(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	notPEM := filepath.Join(dir, "not.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))

	cases := []struct {
		name    string
		cfg     TLSConfig
		wantErr string
	}{
		{"disabled", TLSConfig{}, "TLS is not configured"},
		{"cert without key", TLSConfig{CertFile: pki.ClientCertFile}, "certificate and key must be set together"},
		{"key without cert", TLSConfig{CAFile: pki.CAFile, KeyFile: pki.ClientKeyFile}, "certificate and key must be set together"},
		{"missing ca file", TLSConfig{CAFile: missing}, "missing.pem"},
		{"ca file without pem", TLSConfig{CAFile: notPEM}, "contains no PEM certificates"},
		{"missing cert file", TLSConfig{CertFile: missing, KeyFile: pki.ClientKeyFile}, "missing.pem"},
		{"missing key file", TLSConfig{CertFile: pki.ClientCertFile, KeyFile: missing}, "missing.pem"},
		{"mismatched cert and key", TLSConfig{CertFile: pki.ClientCertFile, KeyFile: pki.ServerKeyFile}, "load client certificate and key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Credentials()
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestTLSConfig_SummaryOmitsFileContents(t *testing.T) {
	pki := tlstest.New(t)
	cases := []struct {
		name string
		cfg  TLSConfig
		want []string
	}{
		{"empty config", TLSConfig{}, []string{"mode=plaintext"}},
		{"server name only", TLSConfig{ServerName: "cp.internal"}, []string{"mode=tls", "ca=system-roots", "server-name=cp.internal"}},
		{"mutual tls", TLSConfig{CAFile: pki.CAFile, CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}, []string{"mode=mutual-tls", "ca=" + pki.CAFile}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary := tc.cfg.Summary()
			for _, want := range tc.want {
				require.Contains(t, summary, want)
			}
			require.NotContains(t, summary, "BEGIN", "the summary must not carry PEM content")
			require.NotContains(t, summary, pki.ClientKeyFile, "the summary must not carry the key path")
		})
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
	require.NoError(t, err)
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
	require.NoError(t, err)
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
	require.NoError(t, err)
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "ready"}))
	require.NoError(t, tr.Close())

	require.Eventually(t, func() bool {
		_, received := srv.snapshot()
		return len(received) == 1
	}, 5*time.Second, 5*time.Millisecond)

	clientCN, received := srv.snapshot()
	require.Len(t, received, 1)
	require.Equal(t, "ready", received[0].Type)
	require.Equal(t, tlstest.ClientCommonName, clientCN)
}

func TestGRPCTransport_ServerAuthenticatedTLSSucceeds(t *testing.T) {
	pki := tlstest.New(t)
	addr := startTCPServer(t, &peerRecordingServer{}, pki.ServerTLSConfig(false))

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	require.NoError(t, err)
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
	}
	require.ErrorContains(t, err, "handshake", "NewGRPCTransport must fail against a plaintext server")
	t.Logf("refused: %v", err)
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
	}
	require.ErrorContains(t, err, "certificate signed by unknown authority", "NewGRPCTransport must reject a server certificate from an untrusted CA")
	t.Logf("refused: %v", err)
}

func TestGRPCTransport_TLSRefusesServerBelowTLS13(t *testing.T) {
	pki := tlstest.New(t)
	serverTLS := pki.ServerTLSConfig(false)
	serverTLS.MaxVersion = tls.VersionTLS12
	addr := startTCPServer(t, &peerRecordingServer{}, serverTLS)

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	if err == nil {
		_ = tr.Close()
	}
	require.ErrorContains(t, err, "protocol version", "NewGRPCTransport must require TLS 1.3 from the server")
	t.Logf("refused: %v", err)
}
