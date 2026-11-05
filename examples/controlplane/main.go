// Command controlplane is a minimal control plane for `stirrup job`. It
// serves the HarnessService.RunTask stream for exactly one session: it waits
// for the harness to connect, assigns the RunConfig from a file, answers
// permission requests, prints events as they arrive, sends each -followup
// prompt after a run's done to start the next run, and exits with a status
// that reflects the runs' outcomes.
//
// The transport is plaintext and unauthenticated unless the -tls-* flags
// are set; without them, run it on loopback or a private network only.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
)

// Exit statuses: 0 when every run succeeded, 1 for a run that did not
// succeed or a stream that ended without the last done event, 2 for a usage
// or setup error.
const (
	exitSuccess = 0
	exitFailed  = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:50051", "address to serve RunTask on")
	configPath := fs.String("config", "", "path to the RunConfig JSON file to assign (required)")
	addrFile := fs.String("addr-file", "", "write the bound listen address to this file once listening")
	permission := fs.String("permission", "deny", "how to answer permission requests: allow, deny or ask")
	var tf tlsFiles
	fs.StringVar(&tf.certFile, "tls-cert", "", "PEM server certificate; with -tls-key, serves TLS 1.3 instead of plaintext")
	fs.StringVar(&tf.keyFile, "tls-key", "", "PEM private key for -tls-cert")
	fs.StringVar(&tf.clientCAFile, "tls-client-ca", "", "PEM CA bundle; requires every client to present a certificate it issued (mutual TLS; needs -tls-cert)")
	var followUps []string
	fs.Func("followup", "prompt to send as a user_response after a run's done, starting a follow-up run; repeat for more runs (needs followUpGrace in the RunConfig)", func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("must not be empty")
		}
		followUps = append(followUps, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := rejectEmptyTLSFlags(fs); err != nil {
		writef(stderr, "controlplane: %v\n", err)
		return exitUsage
	}
	if *configPath == "" {
		writef(stderr, "controlplane: -config is required\n")
		return exitUsage
	}
	mode, err := parsePermissionMode(*permission)
	if err != nil {
		writef(stderr, "controlplane: %v\n", err)
		return exitUsage
	}
	sec, err := tf.security()
	if err != nil {
		writef(stderr, "controlplane: %v\n", err)
		return exitUsage
	}
	cfg, err := loadRunConfig(*configPath)
	if err != nil {
		writef(stderr, "controlplane: %v\n", err)
		return exitUsage
	}
	if len(followUps) > 0 && cfg.GetFollowUpGrace() <= 0 {
		writef(stderr, "controlplane: -followup needs followUpGrace in the RunConfig; without it the harness closes the stream after the first run\n")
		return exitUsage
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		writef(stderr, "controlplane: %v\n", err)
		return exitUsage
	}
	if *addrFile != "" {
		if err := os.WriteFile(*addrFile, []byte(lis.Addr().String()), 0o600); err != nil {
			writef(stderr, "controlplane: %v\n", err)
			_ = lis.Close()
			return exitUsage
		}
	}
	srv := newServer(cfg, mode, stdin, stdout, stderr)
	srv.followUps = followUps
	return serve(lis, srv, sec, stderr)
}

// serve runs the gRPC server on lis until the session's last run finishes and
// returns the process exit status.
func serve(lis net.Listener, srv *server, sec listenSecurity, stderr io.Writer) int {
	gs := newGRPCServer(srv, sec)
	go func() {
		if err := gs.Serve(lis); err != nil {
			writef(stderr, "controlplane: serve: %v\n", err)
		}
	}()
	defer stopGracefully(gs)

	writef(stderr, "controlplane: waiting for a harness on %s (%s)\n", lis.Addr(), sec.label)
	out := <-srv.results
	if out.err != nil {
		writef(stderr, "controlplane: run failed: %v\n", out.err)
		return exitFailed
	}
	if !out.result.Succeeded() {
		return exitFailed
	}
	return exitSuccess
}

// shutdownGrace bounds how long the server waits for the finished RunTask
// call to flush its closing status before dropping the connection.
const shutdownGrace = 5 * time.Second

func stopGracefully(gs *grpc.Server) {
	stopped := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownGrace):
		gs.Stop()
	}
}

func writef(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
