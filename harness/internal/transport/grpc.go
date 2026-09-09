package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// GRPCTransport implements Transport over a gRPC bidirectional stream.
// The harness acts as a client, connecting outbound to the control plane's
// gRPC endpoint. Events flow in both directions over a single RunTask stream.
type GRPCTransport struct {
	conn      *grpc.ClientConn
	stream    pb.HarnessService_RunTaskClient
	mu        sync.Mutex // serialises writes to the stream
	handlerMu sync.Mutex // serialises handler registration
	handlers  []func(types.ControlEvent)
	closed    bool                     // guarded by mu; set once the stream is half-closed
	done      chan struct{}            // closed when the read loop exits
	startOnce sync.Once                // ensures the read goroutine is started exactly once
	Security  *security.SecurityLogger // optional; emits SecretRedactedInOutput when scrubbing fires
}

// ErrTransportClosed is returned by Emit once Close has half-closed the
// stream. Emitters that outlive the run (the heartbeat, say) can use it
// to distinguish an orderly shutdown from a transport failure.
var ErrTransportClosed = errors.New("transport closed")

// GRPCTransportOption configures a GRPCTransport.
type GRPCTransportOption func(*grpcTransportConfig)

type grpcTransportConfig struct {
	tlsCreds credentials.TransportCredentials
	dialOpts []grpc.DialOption
}

// WithTLSCredentials configures TLS for the gRPC connection.
func WithTLSCredentials(creds credentials.TransportCredentials) GRPCTransportOption {
	return func(c *grpcTransportConfig) {
		c.tlsCreds = creds
	}
}

// WithDialOptions appends additional gRPC dial options. This is primarily
// useful for testing (e.g. bufconn dialer).
func WithDialOptions(opts ...grpc.DialOption) GRPCTransportOption {
	return func(c *grpcTransportConfig) {
		c.dialOpts = append(c.dialOpts, opts...)
	}
}

// NewGRPCTransport dials the given gRPC target address and opens a
// bidirectional RunTask stream. The returned transport is ready for use.
// The context controls the lifetime of the stream — cancelling it will
// terminate the bidi stream.
//
// By default the connection uses insecure credentials; pass
// WithTLSCredentials to enable TLS.
func NewGRPCTransport(ctx context.Context, target string, opts ...GRPCTransportOption) (*GRPCTransport, error) {
	cfg := &grpcTransportConfig{}
	for _, o := range opts {
		o(cfg)
	}

	var dialOpts []grpc.DialOption
	if cfg.tlsCreds != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(cfg.tlsCreds))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	dialOpts = append(dialOpts, cfg.dialOpts...)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %q: %w", target, err)
	}

	client := pb.NewHarnessServiceClient(conn)
	stream, err := client.RunTask(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open RunTask stream: %w", err)
	}

	return &GRPCTransport{
		conn:   conn,
		stream: stream,
		done:   make(chan struct{}),
	}, nil
}

// Emit scrubs secret patterns from the event's string fields, translates
// the event to its proto representation, and sends it on the gRPC stream.
// When the optional Security logger is wired and any redaction occurs,
// emits a SecretRedactedInOutput event with the matched pattern name and a
// stable location string identifying the call site.
func (g *GRPCTransport) Emit(event types.HarnessEvent) error {
	event.Text = g.scrubAndReport(event.Text, "transport.grpc.event.text")
	event.Content = g.scrubAndReport(event.Content, "transport.grpc.event.content")
	event.Message = g.scrubAndReport(event.Message, "transport.grpc.event.message")
	event.Input = scrubEventInput(event.Input, g.scrubAndReport, "transport.grpc.event.input")

	pe := harnessEventToProto(event)

	g.mu.Lock()
	defer g.mu.Unlock()

	// A Send after the half-close is not merely rejected: grpc-go's
	// SendMsg calls finish() on the stream, which aborts the RPC and
	// would cut short Close's wait for the peer to acknowledge the
	// events already queued.
	if g.closed {
		return ErrTransportClosed
	}

	if err := g.stream.Send(pe); err != nil {
		return fmt.Errorf("send harness event: %w", err)
	}
	return nil
}

// scrubAndReport scrubs value and, if any redaction happened, fires a
// SecretRedactedInOutput event for each distinct pattern that matched.
// Silently skips the event when no Security logger is wired.
func (g *GRPCTransport) scrubAndReport(value, location string) string {
	scrubbed, stats := security.ScrubWithStats(value)
	if stats.Count > 0 && g.Security != nil {
		for _, p := range stats.Patterns {
			g.Security.SecretRedactedInOutput(p, location)
		}
	}
	return scrubbed
}

// OnControl registers a handler for incoming control events. Multiple calls
// accumulate handlers; all registered handlers are called for each event
// (fan-out). The underlying read goroutine is started on the first call and
// is not restarted on subsequent calls — it is safe to call OnControl after
// the loop is already running.
//
// Handlers must not call Emit on the same GRPCTransport (that would deadlock
// on the write mutex); spawn a goroutine if a handler needs to emit.
func (g *GRPCTransport) OnControl(handler func(event types.ControlEvent)) {
	g.handlerMu.Lock()
	g.handlers = append(g.handlers, handler)
	g.handlerMu.Unlock()

	g.startReadLoop()
}

// startReadLoop starts the single stream reader, at most once per
// transport. Close relies on it too: the reader owns the only Recv call,
// and reaching stream end is how Close learns the peer has consumed
// everything sent.
func (g *GRPCTransport) startReadLoop() {
	g.startOnce.Do(func() {
		go func() {
			defer close(g.done)
			for {
				pe, err := g.stream.Recv()
				if err != nil {
					if err == io.EOF {
						return
					}
					// Stream error — stop reading. The caller can detect
					// this via the done channel and inspect the connection.
					return
				}
				ev := controlEventFromProto(pe)
				g.handlerMu.Lock()
				hs := make([]func(types.ControlEvent), len(g.handlers))
				copy(hs, g.handlers)
				g.handlerMu.Unlock()
				for _, h := range hs {
					h(ev)
				}
			}
		}()
	})
}

// streamEndGrace bounds how long Close waits for the RPC to end after the
// half-close before tearing the connection down anyway. A var so tests can
// shrink it.
//
// The transport is closed ahead of the executor in the factory's LIFO
// closer order, so raising this eats into the sandbox teardown budget on
// the shutdown-watchdog path.
var streamEndGrace = 2 * time.Second

// Close sends CloseSend on the stream to signal the harness is done
// sending, waits (up to streamEndGrace) for the control plane to end the
// RPC, then closes the underlying gRPC connection.
//
// The wait is load-bearing, not politeness: closing the gRPC connection
// discards frames the writer has queued but not yet flushed, so a
// terminal "done" emitted immediately before Close is otherwise lost
// whenever the writer goroutine has not been scheduled yet. A control
// plane that keeps the stream open past the half-close (one serving
// follow-ups) pays streamEndGrace at process exit instead.
//
// The guarantee is best-effort and covers an orderly close only. A
// cancelled stream context (the harness's own SIGTERM path) has already
// killed the RPC, and a peer that stops reading stalls the writer behind
// HTTP/2 flow control until the grace expires; both discard whatever was
// still queued.
func (g *GRPCTransport) Close() error {
	g.mu.Lock()
	g.closed = true
	// grpc-go's CloseSend cannot fail, but the field is an interface:
	// a half-close that did not happen means no stream end to wait for.
	closeSendErr := g.stream.CloseSend()
	g.mu.Unlock()

	if closeSendErr != nil {
		_ = g.conn.Close()
		return fmt.Errorf("close send: %w", closeSendErr)
	}

	g.awaitStreamEnd()

	return g.conn.Close()
}

// awaitStreamEnd blocks until the reader observes the end of the stream
// or streamEndGrace elapses. It starts the reader when no caller
// registered a control handler, since nothing else would consume the
// stream to its end — so the reader serves two lifecycles: dispatching
// control events during a run, and observing the peer's acknowledgement
// at close.
//
// Timing out means the peer never ended the RPC, so anything the writer
// had not flushed is about to be discarded. That is the shape a lost
// terminal "done" takes, so it is reported rather than swallowed.
func (g *GRPCTransport) awaitStreamEnd() {
	g.startReadLoop()

	start := time.Now()
	timer := time.NewTimer(streamEndGrace)
	defer timer.Stop()
	select {
	case <-g.done:
		slog.Debug("grpc transport drained", "duration", time.Since(start))
	case <-timer.C:
		slog.Warn("grpc transport close timed out waiting for the control plane to end the RPC; queued events may be lost",
			"grace", streamEndGrace)
	}
}

// Done returns a channel that is closed when the read loop exits, either
// due to stream EOF, an error, or Close being called.
func (g *GRPCTransport) Done() <-chan struct{} {
	return g.done
}
