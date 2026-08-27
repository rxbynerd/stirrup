package cmd

import (
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/health"
)

// TestRunJob_MissingControlPlaneAddrIsPlain checks that the
// "CONTROL_PLANE_ADDR environment variable is required" error stays
// plain text with no ANSI escapes, since log aggregators ingest the
// job's stderr verbatim.
func TestRunJob_MissingControlPlaneAddrIsPlain(t *testing.T) {
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)
	if err == nil {
		t.Fatal("runJob returned nil, want an error when CONTROL_PLANE_ADDR is unset")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CONTROL_PLANE_ADDR environment variable is required") {
		t.Errorf("error = %q, want the CONTROL_PLANE_ADDR-required message", msg)
	}
	if strings.Contains(msg, "\x1b[") {
		t.Errorf("job error must not contain ANSI escapes (log aggregators ingest it verbatim): %q", msg)
	}
}

// fakeControlPlane implements just enough of HarnessServiceServer to drive
// runJob through the assignment wait and into a (deliberately invalid)
// task_assignment, without ever reaching a real provider or executor.
type fakeControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	readyRecv chan struct{}
}

func (s *fakeControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	close(s.readyRecv)

	// An invalid Mode fails types.ValidateRunConfig immediately inside
	// core.BuildLoopWithTransport, so runJob returns without any
	// provider/executor I/O.
	return stream.Send(&pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{Mode: "bogus-mode"},
	})
}

// TestRunJob_ReadinessMarkerScopedToAssignmentWait drives runJob against a
// fake control plane and asserts that the readiness marker exists only
// while the harness is waiting for a task assignment, while the liveness
// marker spans the whole call.
func TestRunJob_ReadinessMarkerScopedToAssignmentWait(t *testing.T) {
	_ = health.RemoveProbe(health.LivenessMarker)
	_ = health.RemoveProbe(health.ReadinessMarker)
	t.Cleanup(func() {
		_ = health.RemoveProbe(health.LivenessMarker)
		_ = health.RemoveProbe(health.ReadinessMarker)
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := &fakeControlPlane{readyRecv: make(chan struct{})}
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	t.Setenv("CONTROL_PLANE_ADDR", lis.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.readyRecv:
	case <-time.After(5 * time.Second):
		t.Fatal("fake control plane never received the ready event")
	}

	// The harness is now blocked waiting for task_assignment: both
	// markers should be present.
	if err := health.CheckProbe(health.LivenessMarker); err != nil {
		t.Errorf("liveness marker missing during assignment wait: %v", err)
	}
	if err := health.CheckProbe(health.ReadinessMarker); err != nil {
		t.Errorf("readiness marker missing during assignment wait: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "building harness") {
			t.Fatalf("runJob() error = %v, want a building-harness failure from the invalid RunConfig", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJob did not return after the invalid task_assignment")
	}

	// runJob returned, so its deferred cleanup (mirroring process exit in
	// the real "stirrup job" binary) has run: both markers are gone.
	if err := health.CheckProbe(health.ReadinessMarker); err == nil {
		t.Error("readiness marker still present after task_assignment; want it removed once assigned")
	}
	if err := health.CheckProbe(health.LivenessMarker); err == nil {
		t.Error("liveness marker still present after runJob returned; want it removed on exit")
	}
}
