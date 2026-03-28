// Package harnessapi provides the public API for embedding the stirrup harness
// in-process. It re-exports the minimal surface needed by external consumers
// (e.g. the stable control plane's local provisioner) without exposing the
// full internal implementation.
package harnessapi

import (
	"context"
	"io"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// Transport is the bidirectional channel between the harness and the
// control plane. Implementations must be safe for concurrent use.
type Transport = transport.Transport

// Loop wraps an AgenticLoop, exposing only the methods needed by external
// callers: Run and Close.
type Loop struct {
	inner *core.AgenticLoop
}

// BuildLoopWithTransport constructs an agentic loop from the given RunConfig,
// injecting the provided Transport for event I/O. When tp is non-nil it is
// used directly; when nil the transport is built from config.Transport.
func BuildLoopWithTransport(ctx context.Context, config *types.RunConfig, tp Transport) (*Loop, error) {
	al, err := core.BuildLoopWithTransport(ctx, config, tp)
	if err != nil {
		return nil, err
	}
	return &Loop{inner: al}, nil
}

// Run executes the agentic loop to completion and returns the run trace.
func (l *Loop) Run(ctx context.Context, config *types.RunConfig) (*types.RunTrace, error) {
	return l.inner.Run(ctx, config)
}

// Close releases all resources owned by the loop.
func (l *Loop) Close() error {
	return l.inner.Close()
}

// ensure Loop implements io.Closer at compile time.
var _ io.Closer = (*Loop)(nil)
