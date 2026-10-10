package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc/credentials"
)

// TLSMode names the security of the harness's connection to the control
// plane.
type TLSMode string

const (
	TLSModePlaintext TLSMode = "plaintext"
	TLSModeTLS       TLSMode = "tls"
	TLSModeMutualTLS TLSMode = "mutual-tls"
)

// TLSConfig names the PEM files that secure the gRPC connection to the
// control plane. It is not part of RunConfig: `stirrup job` dials before
// any RunConfig arrives, so it is populated from environment variables
// (job) or flags (harness --transport grpc).
//
// Setting any field enables TLS; a CA file pins the server's issuer
// (system roots otherwise), and a certificate plus key adds a client
// certificate for mutual TLS.
type TLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// Enabled reports whether any TLS setting is present. An enabled config
// never falls back to plaintext.
func (c TLSConfig) Enabled() bool {
	return c.CAFile != "" || c.CertFile != "" || c.KeyFile != "" || c.ServerName != ""
}

// Mode reports the connection security c configures.
func (c TLSConfig) Mode() TLSMode {
	switch {
	case !c.Enabled():
		return TLSModePlaintext
	case c.CertFile != "" || c.KeyFile != "":
		return TLSModeMutualTLS
	default:
		return TLSModeTLS
	}
}

// Summary describes the active mode for a startup log line. It names the
// trust source and server-name override but never reads or prints file
// contents.
func (c TLSConfig) Summary() string {
	if !c.Enabled() {
		return string(TLSModePlaintext)
	}
	parts := []string{"mode=" + string(c.Mode())}
	if c.CAFile != "" {
		parts = append(parts, "ca="+c.CAFile)
	} else {
		parts = append(parts, "ca=system-roots")
	}
	if c.ServerName != "" {
		parts = append(parts, "server-name="+c.ServerName)
	}
	return strings.Join(parts, " ")
}

// ClientTLSConfig loads the configured files into a client *tls.Config
// with TLS 1.3 as the minimum version. It fails when TLS is not enabled,
// when only one of the certificate and key is set, or when a file is
// missing, unreadable, or not valid PEM.
func (c TLSConfig) ClientTLSConfig() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, errors.New("TLS is not configured")
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, errors.New("a client certificate and key must be set together")
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: c.ServerName,
	}

	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %q contains no PEM certificates", c.CAFile)
		}
		cfg.RootCAs = pool
	}

	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate and key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// Credentials returns gRPC transport credentials for c; see
// ClientTLSConfig for the failure cases.
func (c TLSConfig) Credentials() (credentials.TransportCredentials, error) {
	cfg, err := c.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}
