// Command controlplane is a minimal control plane for `stirrup job`. It
// serves the HarnessService.RunTask stream for exactly one run: it waits for
// the harness to connect, assigns the RunConfig from a file, answers
// permission requests, prints events as they arrive and exits with a status
// that reflects the run's outcome.
//
// The transport is plaintext and unauthenticated; run it on loopback or a
// private network only.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	harnessv1 "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// Exit statuses: 0 for a successful run, 1 for a run that did not succeed or
// a stream that ended without a done event, 2 for a usage or setup error.
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
	if err := fs.Parse(args); err != nil {
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
	cfg, err := loadRunConfig(*configPath)
	if err != nil {
		writef(stderr, "controlplane: %v\n", err)
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
	return serve(lis, newServer(cfg, mode, stdin, stdout, stderr), stderr)
}

// serve runs the gRPC server on lis until the single run finishes and returns
// the process exit status.
func serve(lis net.Listener, srv *server, stderr io.Writer) int {
	gs := grpc.NewServer()
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	go func() {
		if err := gs.Serve(lis); err != nil {
			writef(stderr, "controlplane: serve: %v\n", err)
		}
	}()
	defer stopGracefully(gs)

	writef(stderr, "controlplane: waiting for a harness on %s (plaintext, unauthenticated)\n", lis.Addr())
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
