package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	harnessv1 "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// listenSecurity is how the server protects the RunTask stream.
type listenSecurity struct {
	creds credentials.TransportCredentials // nil serves plaintext
	label string                           // for the startup line
}

var plaintextSecurity = listenSecurity{label: "plaintext, unauthenticated"}

// newGRPCServer returns a gRPC server protected by sec with srv registered.
func newGRPCServer(srv *server, sec listenSecurity) *grpc.Server {
	var opts []grpc.ServerOption
	if sec.creds != nil {
		opts = append(opts, grpc.Creds(sec.creds))
	}
	gs := grpc.NewServer(opts...)
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	return gs
}

// rejectEmptyTLSFlags fails when a -tls-* flag was passed with an empty
// value, so an unset shell variable cannot silently downgrade the server to
// plaintext or mutual TLS to server-only TLS.
func rejectEmptyTLSFlags(fs *flag.FlagSet) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && strings.HasPrefix(f.Name, "tls-") && f.Value.String() == "" {
			err = fmt.Errorf("-%s is set but empty: omit it, or give it a file", f.Name)
		}
	})
	return err
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
