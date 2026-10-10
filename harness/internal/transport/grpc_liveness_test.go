package transport

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// endOnSignalServer holds each RunTask open until end is closed, then
// returns, ending the RPC from the control-plane side.
type endOnSignalServer struct {
	pb.UnimplementedHarnessServiceServer
	opened chan struct{}
	end    chan struct{}
}

func newEndOnSignalServer() *endOnSignalServer {
	return &endOnSignalServer{opened: make(chan struct{}, 1), end: make(chan struct{})}
}

func (s *endOnSignalServer) RunTask(stream pb.HarnessService_RunTaskServer) error {
	select {
	case s.opened <- struct{}{}:
	default:
	}
	select {
	case <-s.end:
		return nil
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// dialLivenessTransport serves srv over bufconn and returns a transport
// opened with ctx, plus the server so a test can stop it.
func dialLivenessTransport(t *testing.T, ctx context.Context, srv pb.HarnessServiceServer) (*GRPCTransport, *grpc.Server) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	tr, err := NewGRPCTransport(ctx, "passthrough:///bufconn",
		WithDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		),
	)
	if err != nil {
		t.Fatalf("NewGRPCTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, grpcServer
}

func waitTransportDone(t *testing.T, tr *GRPCTransport) {
	t.Helper()
	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("read loop did not exit after the stream ended")
	}
}

// openStream makes the RPC reach the server: grpc-go opens the stream
// lazily, on the first message.
func openStream(t *testing.T, tr *GRPCTransport, srv *endOnSignalServer) {
	t.Helper()
	tr.OnControl(func(types.ControlEvent) {})
	if err := tr.Emit(types.HarnessEvent{Type: "ready"}); err != nil {
		t.Fatalf("Emit ready: %v", err)
	}
	select {
	case <-srv.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the RPC")
	}
}

func TestGRPCTransport_ErrNilWhileLive(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	if err := tr.Err(); err != nil {
		t.Errorf("Err() on a live stream = %v, want nil", err)
	}
}

func TestGRPCTransport_PeerEndingRPCIsReportedAsLoss(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	close(srv.end)
	waitTransportDone(t, tr)

	if err := tr.Err(); !errors.Is(err, ErrPeerEndedStream) {
		t.Errorf("Err() = %v, want ErrPeerEndedStream", err)
	}
}

func TestGRPCTransport_ConnectionFailureIsReportedAsLoss(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, grpcServer := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	grpcServer.Stop()
	waitTransportDone(t, tr)

	err := tr.Err()
	if err == nil {
		t.Fatal("Err() = nil after the server stopped, want the connection failure")
	}
	if errors.Is(err, ErrPeerEndedStream) {
		t.Errorf("Err() = %v, want a connection failure rather than an orderly peer end", err)
	}
	if code := status.Code(err); code == codes.OK {
		t.Errorf("Err() = %v, want a gRPC status error", err)
	}
}

func TestGRPCTransport_LocalCloseIsNotALoss(t *testing.T) {
	useShortStreamEndGrace(t)
	srv := newTestServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	tr.OnControl(func(types.ControlEvent) {})
	if err := tr.Emit(types.HarnessEvent{Type: "done"}); err != nil {
		t.Fatalf("Emit done: %v", err)
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitTransportDone(t, tr)

	if err := tr.Err(); err != nil {
		t.Errorf("Err() after Close = %v, want nil", err)
	}
}

func TestGRPCTransport_StreamContextCancelIsNotALoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, ctx, srv)
	openStream(t, tr, srv)

	cancel()
	waitTransportDone(t, tr)

	if err := tr.Err(); err != nil {
		t.Errorf("Err() after the stream context was cancelled = %v, want nil", err)
	}
}
