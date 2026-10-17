package core

import (
	"context"
	"testing"

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
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if len(runTrace.HookResults) != 3 {
		t.Errorf("HookResults = %+v, want 3 entries", runTrace.HookResults)
	}

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
		if _, dup := phases[s.Name]; dup {
			t.Errorf("more than one %s span", s.Name)
		}
		phases[s.Name] = s
		if s.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("%s parent = %s, want the root span", s.Name, s.Parent.SpanID())
		}
		if s.Status.Code == codes.Error {
			t.Errorf("%s has error status %q; a continueOnError failure must not fail the phase", s.Name, s.Status.Description)
		}
	}
	if len(phases) != 2 {
		t.Fatalf("phase spans = %v, want hooks.preRun and hooks.postRun", phases)
	}

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
		if (exitCode != 0) != (s.Status.Code == codes.Error) {
			t.Errorf("hook span under %s: status %v does not match exit code %d", parent.Name, s.Status.Code, exitCode)
		}
	}
	if perPhase["hooks.preRun"] != 1 || perPhase["hooks.postRun"] != 2 {
		t.Errorf("hook spans per phase = %v, want 1 under hooks.preRun and 2 under hooks.postRun", perPhase)
	}
}
