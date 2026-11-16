package cmd

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// sessionControlPlane records every event of a session until the harness
// closes the stream. It withholds the assignment until heldHeartbeats
// awaiting_assignment heartbeats (at least one) have arrived, then sends
// task, or a cancel when task is nil.
type sessionControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	task           *pb.RunConfig
	heldHeartbeats int

	mu       sync.Mutex
	received []*pb.HarnessEvent
	closed   chan struct{}
}

func newSessionControlPlane(task *pb.RunConfig, heldHeartbeats int) *sessionControlPlane {
	return &sessionControlPlane{task: task, heldHeartbeats: heldHeartbeats, closed: make(chan struct{})}
}

func (s *sessionControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	defer close(s.closed)
	awaiting := 0
	assigned := false
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
		if assigned {
			continue
		}
		if ev.GetType() == "heartbeat" && ev.GetSessionPhase().GetPhase() == types.SessionPhaseAwaitingAssignment {
			awaiting++
		}
		if awaiting < s.heldHeartbeats {
			continue
		}
		assigned = true
		next := &pb.ControlEvent{Type: "cancel"}
		if s.task != nil {
			next = &pb.ControlEvent{Type: "task_assignment", Task: s.task}
		}
		if err := stream.Send(next); err != nil {
			return err
		}
	}
}

func (s *sessionControlPlane) events() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.HarnessEvent(nil), s.received...)
}

// phaseSequence lists the distinct phases the session's heartbeats carried,
// in order, dropping repeats of the phase before.
func phaseSequence(events []*pb.HarnessEvent) []types.SessionPhase {
	var out []types.SessionPhase
	for _, ev := range events {
		if ev.GetType() != "heartbeat" {
			continue
		}
		p := types.SessionPhase{Phase: ev.GetSessionPhase().GetPhase(), Step: ev.GetSessionPhase().GetStep()}
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}

func eventTypes(events []*pb.HarnessEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.GetType()
	}
	return out
}

func shortSessionHeartbeat(t *testing.T) {
	t.Helper()
	prev := sessionHeartbeatInterval
	sessionHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { sessionHeartbeatInterval = prev })
}

func runJobAgainst(t *testing.T, srv *sessionControlPlane) error {
	t.Helper()
	must := require.New(t)
	startControlPlane(t, srv)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return", "got %v", eventTypes(srv.events()))
	}
	select {
	case <-srv.closed:
	case <-time.After(terminalWaitBudget):
		must.FailNow("the harness did not close the stream")
	}
	return runErr
}

func TestRunJob_SessionHeartbeatsWhileAwaitingAssignment(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	shortSessionHeartbeat(t)
	srv := newSessionControlPlane(nil, 4)

	must.NoError(runJobAgainst(t, srv), "a cancel before assignment exits cleanly")

	events := srv.events()
	must.Equal("ready", events[0].GetType(), "ready stays the first event")
	must.Equal([]types.SessionPhase{{Phase: types.SessionPhaseAwaitingAssignment}}, phaseSequence(events))
	must.GreaterOrEqual(len(events)-1, 4, "heartbeats while the assignment was withheld")
}

func TestRunJob_SessionPhasesEndWithDone(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	shortSessionHeartbeat(t)
	srv := newSessionControlPlane(fatalPreRunHookJobConfig(t), 1)

	must.Error(runJobAgainst(t, srv))

	events := srv.events()
	must.Equal("done", events[len(events)-1].GetType(), "done is the session's last event; got %v", eventTypes(events))
	must.Equal([]types.SessionPhase{
		{Phase: types.SessionPhaseAwaitingAssignment},
		{Phase: types.SessionPhaseConstructing},
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepPreRunHooks},
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhasePostRun},
	}, phaseSequence(events))
}

func TestRunJob_BuildFailureStopsHeartbeatsBeforeErrorAndDone(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	shortSessionHeartbeat(t)
	srv := newSessionControlPlane(&pb.RunConfig{Mode: "execution"}, 1)

	must.ErrorContains(runJobAgainst(t, srv), "building harness")

	events := srv.events()
	kinds := eventTypes(events)
	must.Equal([]string{"error", "done"}, kinds[len(kinds)-2:], "the terminal pair ends the session")
	phases := phaseSequence(events)
	must.Equal(types.SessionPhaseConstructing, phases[len(phases)-1].Phase)
}
