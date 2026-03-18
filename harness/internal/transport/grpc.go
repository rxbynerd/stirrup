package transport

import (
	"context"
	"fmt"
	"io"
	"sync"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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
	done      chan struct{} // closed when the read loop exits
	startOnce sync.Once    // ensures the read goroutine is started exactly once
}

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
		conn.Close()
		return nil, fmt.Errorf("open RunTask stream: %w", err)
	}

	return &GRPCTransport{
		conn:   conn,
		stream: stream,
		done:   make(chan struct{}),
	}, nil
}

// newGRPCTransportFromStream creates a GRPCTransport from an existing stream
// and connection. This is used internally for testing.
func newGRPCTransportFromStream(conn *grpc.ClientConn, stream pb.HarnessService_RunTaskClient) *GRPCTransport {
	return &GRPCTransport{
		conn:   conn,
		stream: stream,
		done:   make(chan struct{}),
	}
}

// Emit scrubs secret patterns from the event's string fields, translates
// the event to its proto representation, and sends it on the gRPC stream.
func (g *GRPCTransport) Emit(event types.HarnessEvent) error {
	// Scrub known secret patterns from all string fields.
	event.Text = security.Scrub(event.Text)
	event.Content = security.Scrub(event.Content)
	event.Message = security.Scrub(event.Message)

	pe := harnessEventToProto(event)

	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.stream.Send(pe); err != nil {
		return fmt.Errorf("send harness event: %w", err)
	}
	return nil
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

// Close sends CloseSend on the stream to signal the harness is done
// sending, then closes the underlying gRPC connection.
func (g *GRPCTransport) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.stream.CloseSend(); err != nil {
		// Still close the connection even if CloseSend fails.
		g.conn.Close()
		return fmt.Errorf("close send: %w", err)
	}

	return g.conn.Close()
}

// Done returns a channel that is closed when the read loop exits, either
// due to stream EOF, an error, or Close being called.
func (g *GRPCTransport) Done() <-chan struct{} {
	return g.done
}
