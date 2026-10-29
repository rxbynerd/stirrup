package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// listenSecurity is how the server protects the RunTask stream.
type listenSecurity struct {
	creds credentials.TransportCredentials // nil serves plaintext
	label string                           // for the startup line
}

var plaintextSecurity = listenSecurity{label: "plaintext, unauthenticated"}

func (s listenSecurity) serverOptions() []grpc.ServerOption {
	if s.creds == nil {
		return nil
	}
	return []grpc.ServerOption{grpc.Creds(s.creds)}
}

// tlsFiles names the PEM files behind the -tls-* flags.
type tlsFiles struct {
	certFile     string
	keyFile      string
	clientCAFile string
}

// security loads the files into server credentials. With no files set it
// serves plaintext. A server certificate and key enable TLS 1.3; a client
// CA additionally requires every client to present a certificate issued by
// that CA (mutual TLS).
func (f tlsFiles) security() (listenSecurity, error) {
	if f.certFile == "" && f.keyFile == "" && f.clientCAFile == "" {
		return plaintextSecurity, nil
	}
	if f.certFile == "" || f.keyFile == "" {
		return listenSecurity{}, errors.New("-tls-cert and -tls-key must be set together, and -tls-client-ca needs both")
	}
	cert, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return listenSecurity{}, fmt.Errorf("load server certificate and key: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if f.clientCAFile == "" {
		return listenSecurity{creds: credentials.NewTLS(cfg), label: "TLS, clients unauthenticated"}, nil
	}
	pem, err := os.ReadFile(f.clientCAFile)
	if err != nil {
		return listenSecurity{}, fmt.Errorf("read client CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return listenSecurity{}, fmt.Errorf("client CA file %q contains no PEM certificates", f.clientCAFile)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return listenSecurity{creds: credentials.NewTLS(cfg), label: "mutual TLS"}, nil
}
