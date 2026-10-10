package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestCancelSession_ConcurrentWithRunStartAlwaysCancelsRun races a
// session cancel against a run registering itself. Whichever wins the
// lock, the run's context must end with the session's cause.
func TestCancelSession_ConcurrentWithRunStartAlwaysCancelsRun(t *testing.T) {
	const iterations = 5000
	for i := range iterations {
		loop := buildTestLoop(&mockProvider{})
		loop.ensureControlRouting()
		runCtx, cancelRun := context.WithCancelCause(context.Background())

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			loop.cancelSession(ErrControlStreamLost, "discarded by stream loss")
		}()
		go func() {
			defer wg.Done()
			<-start
			if cause := loop.beginRun(cancelRun); cause != nil {
				cancelRun(cause)
			}
		}()
		close(start)
		wg.Wait()

		if !errors.Is(context.Cause(runCtx), ErrControlStreamLost) {
			t.Fatalf("iteration %d: run cause = %v, want ErrControlStreamLost", i, context.Cause(runCtx))
		}
		cancelRun(nil)
		loop.stopControlRouting()
	}
}
