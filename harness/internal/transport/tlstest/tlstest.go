// Package tlstest generates throwaway certificate authorities and leaf
// certificates for tests of the control-plane TLS options. It is a
// test-support package (not _test-only) so both the transport package and
// the CLI command tests can import it.
package tlstest

import (
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
)

// ClientCommonName is the subject common name of every generated client
// certificate.
const ClientCommonName = "stirrup-harness"

// PKI is one certificate authority with a server certificate valid for
// 127.0.0.1 and localhost and a client certificate, all written as PEM
// files under a test-scoped directory.
type PKI struct {
	CAFile         string
	ServerCertFile string
	ServerKeyFile  string
	ClientCertFile string
	ClientKeyFile  string

	caPool     *x509.CertPool
	serverCert tls.Certificate
}

// New generates a fresh PKI. Each call creates an independent CA, so two
// PKIs do not trust each other's certificates.
func New(t testing.TB) *PKI {
	t.Helper()
	dir := t.TempDir()

	caKey := newKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "stirrup test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	p := &PKI{
		CAFile:         filepath.Join(dir, "ca.pem"),
		ServerCertFile: filepath.Join(dir, "server.pem"),
		ServerKeyFile:  filepath.Join(dir, "server-key.pem"),
		ClientCertFile: filepath.Join(dir, "client.pem"),
		ClientKeyFile:  filepath.Join(dir, "client-key.pem"),
		caPool:         x509.NewCertPool(),
	}
	p.caPool.AddCert(caCert)
	writePEM(t, p.CAFile, "CERTIFICATE", caDER)

	serverTmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	p.issueLeaf(t, serverTmpl, caCert, caKey, p.ServerCertFile, p.ServerKeyFile)

	clientTmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: ClientCommonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	p.issueLeaf(t, clientTmpl, caCert, caKey, p.ClientCertFile, p.ClientKeyFile)

	p.serverCert, err = tls.LoadX509KeyPair(p.ServerCertFile, p.ServerKeyFile)
	if err != nil {
		t.Fatalf("load server key pair: %v", err)
	}
	return p
}

// ServerTLSConfig returns a server configuration presenting the server
// certificate. With requireClientCert it rejects clients that do not
// present a certificate issued by this PKI's CA.
func (p *PKI) ServerTLSConfig(requireClientCert bool) *tls.Config {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{p.serverCert},
		MinVersion:   tls.VersionTLS12,
	}
	if requireClientCert {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = p.caPool
	}
	return cfg
}

func (p *PKI) issueLeaf(t testing.TB, tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey, certPath, keyPath string) {
	t.Helper()
	key := newKey(t)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	return n
}

func writePEM(t testing.TB, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
