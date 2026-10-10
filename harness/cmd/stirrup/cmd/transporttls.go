package cmd

import (
	"fmt"
	"io"
	"os"

	flag "github.com/spf13/pflag"
	"google.golang.org/grpc/credentials"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
)

// Control-plane TLS settings for `stirrup job`. They are environment
// variables rather than RunConfig fields because the job dials the
// control plane before any RunConfig arrives, so they have no proto
// mirror.
const (
	envControlPlaneTLSCAFile     = "CONTROL_PLANE_TLS_CA_FILE"
	envControlPlaneTLSCertFile   = "CONTROL_PLANE_TLS_CERT_FILE"
	envControlPlaneTLSKeyFile    = "CONTROL_PLANE_TLS_KEY_FILE"
	envControlPlaneTLSServerName = "CONTROL_PLANE_TLS_SERVER_NAME"
)

// Flags carrying the same settings for `stirrup harness --transport grpc`.
// They are harness-only, not RunConfig flags, so `stirrup run-config`
// does not register them.
const (
	flagTransportTLSCAFile     = "transport-tls-ca-file"
	flagTransportTLSCertFile   = "transport-tls-cert-file"
	flagTransportTLSKeyFile    = "transport-tls-key-file"
	flagTransportTLSServerName = "transport-tls-server-name"
)

func controlPlaneTLSFromEnv() transport.TLSConfig {
	return transport.TLSConfig{
		CAFile:     os.Getenv(envControlPlaneTLSCAFile),
		CertFile:   os.Getenv(envControlPlaneTLSCertFile),
		KeyFile:    os.Getenv(envControlPlaneTLSKeyFile),
		ServerName: os.Getenv(envControlPlaneTLSServerName),
	}
}

// resolveControlPlaneTLS loads the job's TLS credentials, returning nil
// for the plaintext default. Any failure is returned rather than
// downgraded to plaintext.
func resolveControlPlaneTLS(cfg transport.TLSConfig) (credentials.TransportCredentials, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	creds, err := cfg.Credentials()
	if err != nil {
		return nil, fmt.Errorf("control plane TLS (%s, %s, %s, %s): %w",
			envControlPlaneTLSCAFile, envControlPlaneTLSCertFile, envControlPlaneTLSKeyFile, envControlPlaneTLSServerName, err)
	}
	return creds, nil
}

func addTransportTLSFlags(f *flag.FlagSet) {
	f.String(flagTransportTLSCAFile, "", "PEM CA bundle that must have issued the control plane's certificate (requires --transport grpc; default: system roots). Any --transport-tls-* flag enables TLS 1.3 with no plaintext fallback. CLI-only, never part of a RunConfig.")
	f.String(flagTransportTLSCertFile, "", "PEM client certificate presented to the control plane for mutual TLS (requires --transport-tls-key-file and --transport grpc).")
	f.String(flagTransportTLSKeyFile, "", "PEM private key for --transport-tls-cert-file (requires --transport-tls-cert-file and --transport grpc).")
	f.String(flagTransportTLSServerName, "", "Server name to verify the control plane's certificate against, when it differs from the --transport-addr host (requires --transport grpc).")
}

func transportTLSFromFlags(f *flag.FlagSet) transport.TLSConfig {
	ca, _ := f.GetString(flagTransportTLSCAFile)
	cert, _ := f.GetString(flagTransportTLSCertFile)
	key, _ := f.GetString(flagTransportTLSKeyFile)
	serverName, _ := f.GetString(flagTransportTLSServerName)
	return transport.TLSConfig{CAFile: ca, CertFile: cert, KeyFile: key, ServerName: serverName}
}

// resolveTransportTLSFlags loads the harness's --transport-tls-*
// credentials for a resolved transport type, returning nil for the
// plaintext default. TLS flags on a non-grpc transport or a lone
// certificate or key are usage errors (exit 4); a file that cannot be
// loaded is an I/O error (exit 3).
func resolveTransportTLSFlags(cfg transport.TLSConfig, transportType string) (credentials.TransportCredentials, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if transportType != "grpc" {
		return nil, usageError(fmt.Errorf("--transport-tls-* flags require --transport grpc (transport is %q)", transportType))
	}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, usageError(fmt.Errorf("--%s and --%s must be set together", flagTransportTLSCertFile, flagTransportTLSKeyFile))
	}
	creds, err := cfg.Credentials()
	if err != nil {
		return nil, ioError(fmt.Errorf("--transport-tls-*: %w", err))
	}
	return creds, nil
}

// logControlPlaneTransport writes the one startup line naming the
// connection's security mode. It never includes file contents.
func logControlPlaneTransport(w io.Writer, addr string, cfg transport.TLSConfig) {
	_, _ = fmt.Fprintf(w, "stirrup: control plane transport %s addr=%s\n", cfg.Summary(), addr)
}
