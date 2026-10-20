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
			must := require.New(t)
			must.Equal(tc.want, tc.cfg.Mode())
			must.Equal(tc.want != TLSModePlaintext, tc.cfg.Enabled())
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
			must := require.New(t)
			tlsCfg, err := tc.cfg.ClientTLSConfig()
			must.NoError(err)
			must.Equal(uint16(tls.VersionTLS13), tlsCfg.MinVersion)
		})
	}
}

func TestTLSConfig_ClientTLSConfig_LoadsMaterial(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)

	serverAuth, err := TLSConfig{CAFile: pki.CAFile, ServerName: "cp.internal"}.ClientTLSConfig()
	must.NoError(err)
	must.NotNil(serverAuth.RootCAs, "the CA file's pool should be loaded")
	must.Equal("cp.internal", serverAuth.ServerName)
	must.Empty(serverAuth.Certificates, "no client certificate is configured")

	clientCert, err := TLSConfig{CertFile: pki.ClientCertFile, KeyFile: pki.ClientKeyFile}.ClientTLSConfig()
	must.NoError(err)
	must.Nil(clientCert.RootCAs, "system roots must verify the server")
	must.Len(clientCert.Certificates, 1)
}

func TestTLSConfig_Credentials_RejectsInvalidConfig(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	notPEM := filepath.Join(dir, "not.pem")
	must.NoError(os.WriteFile(notPEM, []byte("not a certificate"), 0o600))

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
			must := require.New(t)
			_, err := tc.cfg.Credentials()
			must.ErrorContains(err, tc.wantErr)
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
			must := require.New(t)
			summary := tc.cfg.Summary()
			for _, want := range tc.want {
				must.Contains(summary, want)
			}
			must.NotContains(summary, "BEGIN", "the summary must not carry PEM content")
			must.NotContains(summary, pki.ClientKeyFile, "the summary must not carry the key path")
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
	must := require.New(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
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
	must := require.New(t)
	creds, err := cfg.Credentials()
	must.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return NewGRPCTransport(ctx, addr, WithTLSCredentials(creds))
}

func TestGRPCTransport_MutualTLSSucceeds(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)
	srv := &peerRecordingServer{}
	addr := startTCPServer(t, srv, pki.ServerTLSConfig(true))

	tr, err := dialWithTLS(t, addr, TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	})
	must.NoError(err)
	must.NoError(tr.Emit(types.HarnessEvent{Type: "ready"}))
	must.NoError(tr.Close())

	must.Eventually(func() bool {
		_, received := srv.snapshot()
		return len(received) == 1
	}, 5*time.Second, 5*time.Millisecond)

	clientCN, received := srv.snapshot()
	must.Len(received, 1)
	must.Equal("ready", received[0].Type)
	must.Equal(tlstest.ClientCommonName, clientCN)
}

func TestGRPCTransport_ServerAuthenticatedTLSSucceeds(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)
	addr := startTCPServer(t, &peerRecordingServer{}, pki.ServerTLSConfig(false))

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	must.NoError(err)
	_ = tr.Close()
}

func TestGRPCTransport_TLSRefusesPlaintextServer(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "handshake", "NewGRPCTransport must fail against a plaintext server")
	t.Logf("refused: %v", err)
}

func TestGRPCTransport_TLSRefusesServerFromOtherCA(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "certificate signed by unknown authority", "NewGRPCTransport must reject a server certificate from an untrusted CA")
	t.Logf("refused: %v", err)
}

func TestGRPCTransport_TLSRefusesServerBelowTLS13(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)
	serverTLS := pki.ServerTLSConfig(false)
	serverTLS.MaxVersion = tls.VersionTLS12
	addr := startTCPServer(t, &peerRecordingServer{}, serverTLS)

	tr, err := dialWithTLS(t, addr, TLSConfig{CAFile: pki.CAFile})
	if err == nil {
		_ = tr.Close()
	}
	must.ErrorContains(err, "protocol version", "NewGRPCTransport must require TLS 1.3 from the server")
	t.Logf("refused: %v", err)
}
