package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoop_Hooks_OTelPhaseAndHookSpans pins the OTel shape of a run with
// lifecycle hooks: one hooks.<phase> span per phase under the root span,
// each parenting one hook span per execution, with a continueOnError
// failure marking only its own hook span as an error.
func TestLoop_Hooks_OTelPhaseAndHookSpans(t *testing.T) {
	emitter, exporter := newInMemoryOTelEmitter(t)

	loop := buildTestLoop(simpleSuccessProvider())
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	loop.Hooks = &fakeHookRunner{
		preResults: []types.HookExecution{
			{Phase: "preRun", Index: 0, Name: "clone", Command: "git clone repo"},
		},
		postResults: []types.HookExecution{
			{Phase: "postRun", Index: 0, Command: "notify", ExitCode: 1, ContinuedOnError: true, Error: "exit status 1"},
			{Phase: "postRun", Index: 1, Command: "upload"},
		},
	}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	assert.Len(t, runTrace.HookResults, 3, "HookResults = %+v", runTrace.HookResults)

	spans := exporter.GetSpans()
	byID := map[string]tracetest.SpanStub{}
	var root tracetest.SpanStub
	for _, s := range spans {
		byID[s.SpanContext.SpanID().String()] = s
		if s.Name == "invoke_agent stirrup" {
			root = s
		}
	}

	phases := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		if s.Name != "hooks.preRun" && s.Name != "hooks.postRun" {
			continue
		}
		assert.NotContains(t, phases, s.Name, "more than one %s span", s.Name)
		phases[s.Name] = s
		assert.Equal(t, root.SpanContext.SpanID(), s.Parent.SpanID(), "%s parent should be the root span", s.Name)
		assert.NotEqual(t, codes.Error, s.Status.Code,
			"%s has error status %q; a continueOnError failure must not fail the phase", s.Name, s.Status.Description)
	}
	require.Len(t, phases, 2, "phase spans = %v, want hooks.preRun and hooks.postRun", phases)

	perPhase := map[string]int{}
	for _, s := range spans {
		if s.Name != "hook" {
			continue
		}
		parent := byID[s.Parent.SpanID().String()]
		perPhase[parent.Name]++
		var exitCode int64
		for _, kv := range s.Attributes {
			if kv.Key == "hook.exit_code" {
				exitCode = kv.Value.AsInt64()
			}
		}
		wantError := exitCode != 0
		isError := s.Status.Code == codes.Error
		assert.Equal(t, wantError, isError, "error status of hook span under %s (exit code %d)", parent.Name, exitCode)
	}
	assert.Equal(t, 1, perPhase["hooks.preRun"], "hook spans per phase = %v", perPhase)
	assert.Equal(t, 2, perPhase["hooks.postRun"], "hook spans per phase = %v", perPhase)
}
