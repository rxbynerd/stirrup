package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "NewGRPCTransport")
	t.Cleanup(func() { _ = tr.Close() })
	return tr, grpcServer
}

func waitTransportDone(t *testing.T, tr *GRPCTransport) {
	t.Helper()
	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "read loop did not exit after the stream ended")
	}
}

// openStream makes the RPC reach the server: grpc-go opens the stream
// lazily, on the first message.
func openStream(t *testing.T, tr *GRPCTransport, srv *endOnSignalServer) {
	t.Helper()
	tr.OnControl(func(types.ControlEvent) {})
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "ready"}), "Emit ready")
	select {
	case <-srv.opened:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "server never saw the RPC")
	}
}

func TestGRPCTransport_Err_NilWhileStreamLive(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	require.NoError(t, tr.Err(), "Err() on a live stream")
}

func TestGRPCTransport_PeerEndedRPC_ReportedAsLoss(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	close(srv.end)
	waitTransportDone(t, tr)

	require.ErrorIs(t, tr.Err(), ErrPeerEndedStream)
}

func TestGRPCTransport_ConnectionFailure_ReportedAsLoss(t *testing.T) {
	srv := newEndOnSignalServer()
	tr, grpcServer := dialLivenessTransport(t, context.Background(), srv)
	openStream(t, tr, srv)

	grpcServer.Stop()
	waitTransportDone(t, tr)

	err := tr.Err()
	require.Error(t, err, "Err() after the server stopped")
	require.NotErrorIs(t, err, ErrPeerEndedStream, "a connection failure rather than an orderly peer end")
	require.NotEqual(t, codes.OK, status.Code(err), "Err() should be a gRPC status error")
}

func TestGRPCTransport_LocalClose_NotALoss(t *testing.T) {
	useShortStreamEndGrace(t)
	srv := newTestServer()
	tr, _ := dialLivenessTransport(t, context.Background(), srv)
	tr.OnControl(func(types.ControlEvent) {})
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "done"}), "Emit done")

	require.NoError(t, tr.Close(), "Close")
	waitTransportDone(t, tr)

	require.NoError(t, tr.Err(), "Err() after Close")
}

func TestGRPCTransport_StreamContextCancel_NotALoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newEndOnSignalServer()
	tr, _ := dialLivenessTransport(t, ctx, srv)
	openStream(t, tr, srv)

	cancel()
	waitTransportDone(t, tr)

	require.NoError(t, tr.Err(), "Err() after the stream context was cancelled")
}
