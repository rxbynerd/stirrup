package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/sandboxidentity"
	"github.com/rxbynerd/stirrup/types"
)

func textOnlyResponse(id string) string {
	return openAIChunk(`{"id":"`+id+`","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
		openAIChunk(`{"id":"`+id+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"
}

// TestBuildLoopWithTransport_SandboxIdentity_RefresherClosesFirst pins the
// teardown order: the refresher is the last owned closer, so it stops
// before the transport it emits on and the executor it writes through,
// and a refresh pending at shutdown produces no spurious warning.
func TestBuildLoopWithTransport_SandboxIdentity_RefresherClosesFirst(t *testing.T) {
	sock, _, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	// The refresh is scheduled but not yet due, so it is pending at
	// shutdown. An already-expired token would let the refresher spend its
	// whole request budget, and warn, before Close runs.
	tp := &fakeControlPlaneTransport{respondToken: "jwt", respondTokenPerRequest: true, respondTTL: time.Hour}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-close-order-test", server.URL)

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	require.NoError(t, err)

	last := loop.ownedClosers[len(loop.ownedClosers)-1]
	require.IsType(t, (*sandboxidentity.Refresher)(nil), last,
		"the refresher must be the last owned closer so it closes first")

	assert.NoError(t, loop.Close())
	assert.NoError(t, tp.Close())

	assert.Empty(t, tp.emittedOfType("warning"), "shutdown with a pending refresh must not warn")
	assert.Zero(t, tp.emittedAfterClose(), "no event may be emitted after the transport closed")
}

// TestRunFollowUpLoop_SandboxIdentity_ReusesRefresher pins that follow-up
// runs reuse the loop's sandbox, exchanger, and refresher: a second run
// creates no container, sends no new sandbox_token_request, and delivers
// no new token, so one eight-request budget spans the whole session.
func TestRunFollowUpLoop_SandboxIdentity_ReusesRefresher(t *testing.T) {
	sock, capture, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	server := newOpenAIServer(t, nil, []string{textOnlyResponse("r1"), textOnlyResponse("r2")}, nil)
	defer server.Close()

	tp := &fakeControlPlaneTransport{respondToken: "the-jwt-token"}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-followup-test", server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	loop, err := BuildLoopWithTransport(ctx, config, tp)
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()
	closersBefore := len(loop.ownedClosers)

	_, err = loop.Run(ctx, config)
	require.NoError(t, err)
	require.Len(t, tp.emittedOfType("done"), 1, "the primary run must emit one done event")

	handlersBefore := tp.handlerCount()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		RunFollowUpLoop(ctx, loop, config, 10, FollowUpOptions{})
	}()
	// The loop's control handler is registered once at build, so input
	// delivered before the follow-up loop reaches its select is queued
	// rather than fanned out to nobody.
	tp.deliver(types.ControlEvent{Type: "user_response", UserResponse: "again"})

	require.Eventually(t, func() bool {
		return len(tp.emittedOfType("done")) >= 2
	}, 10*time.Second, 10*time.Millisecond, "the follow-up run never completed")

	tp.deliver(types.ControlEvent{Type: "cancel"})
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunFollowUpLoop did not return after cancel")
	}

	assert.Equal(t, 1, capture.createCallCount(),
		"the follow-up must reuse the sandbox, so only one container is created in total")
	assert.Len(t, capture.stdinSeen(), 1, "one token is delivered in total")
	assert.Len(t, tp.emittedOfType("sandbox_token_request"), 1, "one sandbox_token_request is sent in total")
	assert.Len(t, loop.ownedClosers, closersBefore,
		"the refresher must be built once, so the owned closers do not change")
	assert.Equal(t, handlersBefore, tp.handlerCount(),
		"the loop registers one control handler at build and shares it")
}
