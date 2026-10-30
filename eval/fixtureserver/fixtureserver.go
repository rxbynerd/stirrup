// Package fixtureserver starts the loopback servers an eval task's harness
// run talks to in place of a live dependency. A server binds a port on
// 127.0.0.1 for one task trial and is closed when the trial ends.
package fixtureserver

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// Server is a running fixture server.
type Server struct {
	// URL is the server's base URL, without a trailing slash.
	URL string

	srv *http.Server
}

// Start validates cfg and starts the fixture server it describes. The
// caller must Close the returned server.
func Start(cfg types.FixtureServerConfig) (*Server, error) {
	var handler http.Handler
	switch cfg.Mode {
	case types.FixtureModeSSE:
		turns, err := LoadTurns(cfg.StreamFile)
		if err != nil {
			return nil, err
		}
		handler = newSSEHandler(turns)
	case types.FixtureModeMCP:
		if cfg.StreamFile != "" {
			return nil, fmt.Errorf("fixture server mode %q takes no stream file", cfg.Mode)
		}
		handler = newMCPHandler()
	default:
		return nil, fmt.Errorf("unknown fixture server mode %q", cfg.Mode)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fixture server: listen: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	return &Server{URL: "http://" + lis.Addr().String(), srv: srv}, nil
}

// Close stops the server and drops its open connections.
func (s *Server) Close() error {
	return s.srv.Close()
}
