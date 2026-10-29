package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

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
	require.New(t).NoError(os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600))
}

func TestTLSFiles_PlaintextWhenUnset(t *testing.T) {
	must := require.New(t)
	sec, err := tlsFiles{}.security()
	must.NoError(err)
	must.Nil(sec.creds)
	must.Empty(sec.serverOptions())
	must.Equal("plaintext, unauthenticated", sec.label)
}

func TestTLSFiles_RejectsIncompleteOrUnreadableSettings(t *testing.T) {
	p := newTestPKI(t)
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	require.New(t).NoError(os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	absent := filepath.Join(t.TempDir(), "absent.pem")
	tests := []struct {
		name  string
		files tlsFiles
	}{
		{"certificate without key", tlsFiles{certFile: p.serverCert}},
		{"key without certificate", tlsFiles{keyFile: p.serverKey}},
		{"client CA without server certificate", tlsFiles{clientCAFile: p.caFile}},
		{"missing certificate file", tlsFiles{certFile: absent, keyFile: p.serverKey}},
		{"missing client CA file", tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: absent}},
		{"client CA without certificates", tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: notPEM}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.files.security()
			require.New(t).Error(err)
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
	gs := grpc.NewServer(sec.serverOptions()...)
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// assignmentOver opens a RunTask stream with creds, sends ready and returns
// the error from receiving the task assignment.
func assignmentOver(t *testing.T, addr string, creds credentials.TransportCredentials) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	require.New(t).NoError(err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := harnessv1.NewHarnessServiceClient(conn).RunTask(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready"}); err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

func TestServe_MutualTLSAssignsOnlyToTrustedClients(t *testing.T) {
	p := newTestPKI(t)
	sec, err := tlsFiles{certFile: p.serverCert, keyFile: p.serverKey, clientCAFile: p.caFile}.security()
	require.New(t).NoError(err)
	require.New(t).Equal("mutual TLS", sec.label)

	clientCert, err := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
	require.New(t).NoError(err)
	otherCA := newTestPKI(t)
	strangerCert, err := tls.LoadX509KeyPair(otherCA.clientCert, otherCA.clientKey)
	require.New(t).NoError(err)

	tests := []struct {
		name    string
		creds   credentials.TransportCredentials
		wantErr bool
	}{
		{"trusted client certificate", credentials.NewTLS(&tls.Config{RootCAs: p.pool, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS13}), false},
		{"no client certificate", credentials.NewTLS(&tls.Config{RootCAs: p.pool, MinVersion: tls.VersionTLS13}), true},
		{"certificate from another CA", credentials.NewTLS(&tls.Config{RootCAs: p.pool, Certificates: []tls.Certificate{strangerCert}, MinVersion: tls.VersionTLS13}), true},
		{"plaintext client", insecure.NewCredentials(), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			addr := startSecuredServer(t, sec)
			err := assignmentOver(t, addr, tc.creds)
			if tc.wantErr {
				check.Error(err)
			} else {
				check.NoError(err)
			}
		})
	}
}

func TestServe_TLSWithoutClientCAAcceptsAnyTLSClient(t *testing.T) {
	must := require.New(t)
	p := newTestPKI(t)
	sec, err := tlsFiles{certFile: p.serverCert, keyFile: p.serverKey}.security()
	must.NoError(err)
	must.Equal("TLS, clients unauthenticated", sec.label)

	must.NoError(assignmentOver(t, startSecuredServer(t, sec), credentials.NewTLS(&tls.Config{RootCAs: p.pool, MinVersion: tls.VersionTLS13})))
	must.Error(assignmentOver(t, startSecuredServer(t, sec), insecure.NewCredentials()))
}
