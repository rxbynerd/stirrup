package core

import (
	"context"
	"testing"
	"time"

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

	// A refresh is always imminent: the token is issued already expired.
	tp := &fakeControlPlaneTransport{respondToken: "jwt", respondTokenPerRequest: true, respondTTL: time.Nanosecond}
	config := sandboxIdentityContainerConfig(t, "sandboxidentity-close-order-test", server.URL)

	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}

	last := loop.ownedClosers[len(loop.ownedClosers)-1]
	if _, ok := last.(*sandboxidentity.Refresher); !ok {
		t.Fatalf("last owned closer is %T, want *sandboxidentity.Refresher so it closes first", last)
	}

	if err := loop.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
	if err := tp.Close(); err != nil {
		t.Errorf("transport Close() error: %v", err)
	}

	if warnings := tp.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("shutdown with a pending refresh must not warn, got %+v", warnings)
	}
	if got := tp.emittedAfterClose(); got != 0 {
		t.Errorf("%d event(s) were emitted after the transport closed", got)
	}
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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()
	closersBefore := len(loop.ownedClosers)

	if _, err := loop.Run(ctx, config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if got := len(tp.emittedOfType("done")); got != 1 {
		t.Fatalf("expected 1 done event after the primary run, got %d", got)
	}

	handlersBefore := tp.handlerCount()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		RunFollowUpLoop(ctx, loop, config, 10)
	}()
	// The follow-up loop registers its control handler on entry; a
	// user_response delivered before that would fan out to nobody.
	deadline := time.Now().Add(5 * time.Second)
	for tp.handlerCount() <= handlersBefore {
		if time.Now().After(deadline) {
			t.Fatal("RunFollowUpLoop never registered its control handler")
		}
		time.Sleep(5 * time.Millisecond)
	}
	tp.deliver(types.ControlEvent{Type: "user_response", UserResponse: "again"})

	deadline = time.Now().Add(10 * time.Second)
	for len(tp.emittedOfType("done")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("follow-up run never completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tp.deliver(types.ControlEvent{Type: "cancel"})
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFollowUpLoop did not return after cancel")
	}

	if got := capture.createCallCount(); got != 1 {
		t.Errorf("follow-up created %d container(s) in total, want 1 (the sandbox is reused)", got)
	}
	if got := capture.stdinSeen(); len(got) != 1 {
		t.Errorf("follow-up delivered %d token(s) in total, want 1", len(got))
	}
	if got := len(tp.emittedOfType("sandbox_token_request")); got != 1 {
		t.Errorf("follow-up sent %d sandbox_token_request(s) in total, want 1", got)
	}
	if got := len(loop.ownedClosers); got != closersBefore {
		t.Errorf("follow-up changed the owned closers from %d to %d; the refresher must be built once", closersBefore, got)
	}
}
