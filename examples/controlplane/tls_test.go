package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	harnessv1 "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// testPKI is a throwaway CA with a server certificate for 127.0.0.1 and a
// client certificate, written as PEM files.
type testPKI struct {
	caFile, serverCert, serverKey, clientCert, clientKey string
	pool                                                 *x509.CertPool
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "controlplane test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	must.NoError(err)
	caCert, err := x509.ParseCertificate(caDER)
	must.NoError(err)

	p := testPKI{
		caFile:     filepath.Join(dir, "ca.pem"),
		serverCert: filepath.Join(dir, "server.pem"),
		serverKey:  filepath.Join(dir, "server-key.pem"),
		clientCert: filepath.Join(dir, "client.pem"),
		clientKey:  filepath.Join(dir, "client-key.pem"),
		pool:       x509.NewCertPool(),
	}
	p.pool.AddCert(caCert)
	writeTestPEM(t, p.caFile, "CERTIFICATE", caDER)

	issue := func(serial int64, tmpl *x509.Certificate, certFile, keyFile string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must.NoError(err)
		tmpl.SerialNumber = big.NewInt(serial)
		tmpl.NotBefore = time.Now().Add(-time.Hour)
		tmpl.NotAfter = time.Now().Add(time.Hour)
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		must.NoError(err)
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		must.NoError(err)
		writeTestPEM(t, certFile, "CERTIFICATE", der)
		writeTestPEM(t, keyFile, "PRIVATE KEY", keyDER)
	}
	issue(2, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, p.serverCert, p.serverKey)
	issue(3, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "stirrup-harness"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, p.clientCert, p.clientKey)
	return p
}

func writeTestPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	must := require.New(t)
	must.NoError(os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600))
}

func TestTLSFiles_PlaintextWhenUnset(t *testing.T) {
	must := require.New(t)
	sec, err := tlsFiles{}.security()
	must.NoError(err)
	must.Nil(sec.creds)
	must.Equal("plaintext, unauthenticated", sec.label)
}

func TestTLSFiles_RejectsIncompleteOrUnreadableSettings(t *testing.T) {
	must := require.New(t)
	p := newTestPKI(t)
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	must.NoError(os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	absent := filepath.Join(t.TempDir(), "absent.pem")
	tests := []struct {
		name    string
		files   tlsFiles
		wantErr string
	}{
		{"certificate without key", tlsFiles{certFile: p.serverCert}, "must be set together"},
		{"key without certificate", tlsFiles{keyFile: p.serverKey}, "must be set together"},
		{"client CA without server certificate", tlsFiles{clientCAFile: p.caFile}, "must be set together"},
		{"missing certificate file", tlsFiles{certFile: absent, keyFile: p.serverKey}, "load server certificate and key"},
		{"missing client CA file", tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: absent}, "read client CA file"},
		{"client CA without certificates", tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: notPEM}, "contains no PEM certificates"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := tc.files.security()
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestRun_RejectsEmptyTLSFlags(t *testing.T) {
	p := newTestPKI(t)
	tests := []struct {
		name string
		args []string
	}{
		{"empty certificate and key", []string{"-tls-cert", "", "-tls-key", ""}},
		{"empty client CA beside a certificate and key", []string{"-tls-cert", p.serverCert, "-tls-key", p.serverKey, "-tls-client-ca", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var stderr bytes.Buffer
			args := append([]string{"-config", "smoke-runconfig.json", "-listen", "127.0.0.1:0"}, tc.args...)
			must.Equal(exitUsage, run(args, strings.NewReader(""), io.Discard, &stderr))
			must.Contains(stderr.String(), "is set but empty")
		})
	}
}

// startSecuredServer serves a fresh test server on loopback with sec and
// returns its address.
func startSecuredServer(t *testing.T, sec listenSecurity) string {
	t.Helper()
	must := require.New(t)
	srv, _, _ := testServer(t, permissionDeny, "")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	gs := newGRPCServer(srv, sec)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// assignmentOver opens a RunTask stream with creds, sends ready and returns
// the first event the control plane sends back.
func assignmentOver(t *testing.T, addr string, creds credentials.TransportCredentials) (*harnessv1.ControlEvent, error) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := harnessv1.NewHarnessServiceClient(conn).RunTask(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready"}); err != nil {
		return nil, err
	}
	return stream.Recv()
}

func clientTLS(pool *x509.CertPool, certs ...tls.Certificate) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{RootCAs: pool, Certificates: certs, MinVersion: tls.VersionTLS12})
}

func TestNewGRPCServer_MutualTLSRefusesUntrustedClients(t *testing.T) {
	must := require.New(t)
	p := newTestPKI(t)
	sec, err := tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: p.caFile}.security()
	must.NoError(err)
	must.Equal("mutual TLS", sec.label)

	clientCert, err := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
	must.NoError(err)
	strangerPKI := newTestPKI(t)
	strangerCert, err := tls.LoadX509KeyPair(strangerPKI.clientCert, strangerPKI.clientKey)
	must.NoError(err)

	// One server for every case: the refused clients must not claim its
	// single session, so the trusted client that follows is still assigned.
	addr := startSecuredServer(t, sec)
	refused := []struct {
		name  string
		creds credentials.TransportCredentials
	}{
		{"plaintext client", insecure.NewCredentials()},
		{"no client certificate", clientTLS(p.pool)},
		{"certificate from another CA", clientTLS(p.pool, strangerCert)},
		{"TLS 1.2 client", credentials.NewTLS(&tls.Config{RootCAs: p.pool, Certificates: []tls.Certificate{clientCert}, MaxVersion: tls.VersionTLS12})},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := assignmentOver(t, addr, tc.creds)
			must.Equal(codes.Unavailable, status.Code(err), "error = %v", err)
		})
	}

	ev, err := assignmentOver(t, addr, clientTLS(p.pool, clientCert))
	must.NoError(err)
	must.Equal("task_assignment", ev.GetType())
}

func TestNewGRPCServer_TLSWithoutClientCAAcceptsAnyTLSClient(t *testing.T) {
	must := require.New(t)
	p := newTestPKI(t)
	sec, err := tlsFiles{certFile: p.serverCert, keyFile: p.serverKey}.security()
	must.NoError(err)
	must.Equal("TLS, clients unauthenticated", sec.label)

	addr := startSecuredServer(t, sec)
	_, err = assignmentOver(t, addr, insecure.NewCredentials())
	must.Equal(codes.Unavailable, status.Code(err), "error = %v", err)
	ev, err := assignmentOver(t, addr, clientTLS(p.pool))
	must.NoError(err)
	must.Equal("task_assignment", ev.GetType())
}

func TestServe_MutualTLSRunsSessionToSuccess(t *testing.T) {
	must := require.New(t)
	p := newTestPKI(t)
	sec, err := tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: p.caFile}.security()
	must.NoError(err)
	clientCert, err := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
	must.NoError(err)

	srv, _, _ := testServer(t, permissionDeny, "")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(clientTLS(p.pool, clientCert)))
	must.NoError(err)
	defer func() { _ = conn.Close() }()
	clientErr := make(chan error, 1)
	go func() {
		stream, err := harnessv1.NewHarnessServiceClient(conn).RunTask(context.Background())
		if err != nil {
			clientErr <- err
			return
		}
		if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready"}); err != nil {
			clientErr <- err
			return
		}
		if _, err := stream.Recv(); err != nil {
			clientErr <- err
			return
		}
		clientErr <- stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"})
	}()

	must.Equal(exitSuccess, serve(lis, srv, sec, io.Discard))
	must.NoError(<-clientErr)
}
