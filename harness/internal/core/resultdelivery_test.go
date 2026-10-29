package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// fakeResultDelivery is a ResultDelivery that records what it was given
// and returns err.
type fakeResultDelivery struct {
	mu     sync.Mutex
	traces []*types.RunTrace
	ctx    ctxObservation
	err    error
	before func()
}

func (d *fakeResultDelivery) Deliver(ctx context.Context, _ *types.RunConfig, runTrace *types.RunTrace) error {
	if d.before != nil {
		d.before()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.traces = append(d.traces, runTrace)
	d.ctx = observe(ctx)
	return d.err
}

func TestRun_ResultDeliveryReceivesFinishedTraceBeforeDone(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	log := &orderLog{}
	loop := buildTestLoop(endTurnProvider())
	tr := &orderedTransport{log: log}
	loop.Transport = tr
	loop.Trace = &orderedEmitter{ctxProbeEmitter: newCtxProbeEmitter(), log: log}
	delivery := &fakeResultDelivery{before: func() { log.add("result_delivery") }}
	loop.ResultDelivery = delivery

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	must.Equal([]string{"trace_finish", "result_delivery", "done"}, log.snapshot())
	must.Len(delivery.traces, 1)
	check.Same(runTrace, delivery.traces[0])
	check.True(delivery.ctx.hasDeadline, "delivery runs under its own budget")
	for _, e := range tr.Events() {
		check.NotEqual("warning", e.Type, "a successful delivery sends no warning")
	}
}

func TestRun_ResultDeliveryFailureWarnsBeforeDoneWithoutChangingOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(endTurnProvider())
	tr := &cancellableTransport{}
	loop.Transport = tr
	long := "publish failed for " + spanProbeKey + " " + strings.Repeat("x", 2*maxFailureTextBytes)
	loop.ResultDelivery = &fakeResultDelivery{err: errors.New(long)}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)

	events := tr.Events()
	warningAt, doneAt := -1, -1
	for i, e := range events {
		switch {
		case e.Type == "warning" && strings.HasPrefix(e.Message, "result sink: "):
			warningAt = i
			check.NotContains(e.Message, spanProbeKey, "the warning is scrubbed")
			check.LessOrEqual(len(e.Message), len("result sink: ")+maxFailureTextBytes+64, "the warning is capped")
		case e.Type == "done":
			doneAt = i
			check.Equal("success", e.StopReason)
		}
	}
	must.GreaterOrEqual(warningAt, 0, "a result sink warning is sent")
	must.Less(warningAt, doneAt, "the warning precedes done")
}
