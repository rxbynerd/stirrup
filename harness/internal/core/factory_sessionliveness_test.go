package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func constructing(step string) types.SessionPhase {
	return types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: step}
}

func shortSessionLivenessInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionLivenessInterval
	sessionLivenessInterval = func(*types.RunConfig) time.Duration { return d }
	t.Cleanup(func() { sessionLivenessInterval = prev })
}

func TestBuildLoopWithTransport_SessionLivenessReportsConstructionSteps(t *testing.T) {
	must := require.New(t)
	sock, _, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{respondToken: "the-jwt-token"}
	config := sandboxIdentityContainerConfig(t, "liveness-construction-steps", server.URL)
	liveness := &recordingLiveness{}

	loop, err := BuildLoopWithTransport(context.Background(), config, tp, WithSessionLiveness(liveness))
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	must.Equal([]types.SessionPhase{
		constructing(types.SessionStepTokenExchange),
		constructing(""),
		constructing(types.SessionStepSandboxCreate),
		constructing(""),
	}, liveness.phases)
	must.Same(liveness, loop.Liveness, "the built loop reports its runs to the same liveness")
}

func TestBuildLoopWithTransport_SessionLivenessSetsIntervalFromConfig(t *testing.T) {
	must := require.New(t)
	config := buildTestConfig()
	config.HeartbeatSeconds = 15
	rec := &recordingTransport{}
	h := StartSessionHeartbeat(rec, time.Hour, constructing(""), nil)
	defer h.Stop()

	t.Setenv("TEST", "unused")

	loop, err := BuildLoopWithTransport(context.Background(), config, rec, WithSessionLiveness(h))
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	h.mu.Lock()
	defer h.mu.Unlock()
	must.Equal(15*time.Second, h.interval)
}

// TestBuildLoopWithTransport_SessionHeartbeatCoversStalledTokenExchange
// withholds the sandbox_token_response and requires heartbeats tagged
// with the exchange step while construction waits for it.
func TestBuildLoopWithTransport_SessionHeartbeatCoversStalledTokenExchange(t *testing.T) {
	must := require.New(t)
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	shortSessionLivenessInterval(t, testLivenessInterval)

	tp := &fakeControlPlaneTransport{noRespond: true}
	config := sandboxIdentityFailClosedConfig(t, "liveness-stalled-exchange")
	h := StartSessionHeartbeat(tp, time.Hour, constructing(""), nil)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), stallFor)
	defer cancel()
	_, err := BuildLoopWithTransport(ctx, config, tp, WithSessionLiveness(h))
	must.Error(err)
	must.Zero(capture.createCallCount(), "no sandbox is created without a token")

	stalled := 0
	for _, e := range tp.emittedOfType("heartbeat") {
		if e.SessionPhase != nil && *e.SessionPhase == constructing(types.SessionStepTokenExchange) {
			stalled++
		}
	}
	must.GreaterOrEqual(stalled, 3, "heartbeats during the stalled exchange")
}
