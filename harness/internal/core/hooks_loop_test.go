package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// errSentinelPreHookFailure / errSentinelPostHookFailure are the fixed
// errors fakeHookRunner returns to simulate a fatal hook failure in the
// lifecycle-hook integration tests below.
var (
	errSentinelPreHookFailure  = errors.New("preRun hook: simulated fatal failure")
	errSentinelPostHookFailure = errors.New("postRun hook: simulated fatal failure")
)

// fakeHookRunner is a test double for hook.Runner used by the agentic
// loop's lifecycle-hook integration tests (issue #461). It records every
// RunPost call's outcome argument (and the ctx's own Err() at call time,
// so tests can assert the detached post-hook ctx is not already dead)
// without needing a real Executor.
type fakeHookRunner struct {
	preResults  []types.HookExecution
	preErr      error
	postResults []types.HookExecution
	postErr     error

	preCalls    int
	postCalls   []string
	postCtxErrs []error
}

var _ hook.Runner = (*fakeHookRunner)(nil)

func (f *fakeHookRunner) RunPre(_ context.Context) ([]types.HookExecution, error) {
	f.preCalls++
	return f.preResults, f.preErr
}

func (f *fakeHookRunner) RunPost(ctx context.Context, outcome string) ([]types.HookExecution, error) {
	f.postCalls = append(f.postCalls, outcome)
	f.postCtxErrs = append(f.postCtxErrs, ctx.Err())
	return f.postResults, f.postErr
}

func simpleSuccessProvider() *mockProvider {
	return &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
}

// TestLoop_Hooks_PreRunFatalFailure_SetsSetupFailedZeroTurns pins the
// preRun-fatal-failure path: outcome is "setup_failed", zero turns ran
// (Run() returns before Git.Setup / the inner loop), and RunPost is
// never called since Run() returns early.
func TestLoop_Hooks_PreRunFatalFailure_SetsSetupFailedZeroTurns(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{preErr: errSentinelPreHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	// finishWithOutcome mirrors finishWithError's existing contract
	// (see TestLoop_PromptBuildError): the underlying hook error is
	// still surfaced as Run()'s Go error, alongside the populated
	// RunTrace reporting the classified outcome.
	if err == nil {
		t.Fatal("expected non-nil error from Run() on a fatal pre-run hook failure")
	}
	if !strings.Contains(err.Error(), "pre-run hooks") {
		t.Errorf("error must mention pre-run hooks, got: %v", err)
	}
	if runTrace.Outcome != "setup_failed" {
		t.Errorf("Outcome = %q, want setup_failed", runTrace.Outcome)
	}
	if runTrace.Turns != 0 {
		t.Errorf("Turns = %d, want 0 (the inner loop must never start)", runTrace.Turns)
	}
	if hooks.preCalls != 1 {
		t.Errorf("RunPre called %d times, want 1", hooks.preCalls)
	}
	if len(hooks.postCalls) != 0 {
		t.Errorf("RunPost called %d times, want 0 (post is skipped after a pre-run failure)", len(hooks.postCalls))
	}
}

// TestLoop_Hooks_PreRunFatalFailure_CtxDeadWinsOverSetupFailed pins that
// a ctx already dead (deadline/cancel) at pre-hook failure time is
// reported via classifyCtxOutcome, not the generic "setup_failed" — the
// hook almost certainly failed because the deadline hit.
func TestLoop_Hooks_PreRunFatalFailure_CtxDeadWinsOverSetupFailed(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{preErr: errSentinelPreHookFailure}
	loop.Hooks = hooks

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // plain cancel, no cause -> classifyCtxOutcome resolves "cancelled"

	config := buildTestConfig()
	runTrace, err := loop.Run(ctx, config)
	if err == nil {
		t.Fatal("expected non-nil error from Run() on a fatal pre-run hook failure")
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("Outcome = %q, want cancelled (ctx-cause classification must win over setup_failed)", runTrace.Outcome)
	}
}

// TestLoop_Hooks_PostRunFatalFailure_OverridesSuccessOnly pins the
// outcome-override rule: a fatal postRun failure turns an otherwise-
// successful run's outcome into "hook_failed".
func TestLoop_Hooks_PostRunFatalFailure_OverridesSuccessOnly(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{postErr: errSentinelPostHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "hook_failed" {
		t.Errorf("Outcome = %q, want hook_failed", runTrace.Outcome)
	}
	if len(hooks.postCalls) != 1 || hooks.postCalls[0] != "success" {
		t.Errorf("RunPost outcomes = %v, want [\"success\"]", hooks.postCalls)
	}
}

// TestLoop_Hooks_PostRunFatalFailure_DoesNotMaskNonSuccessOutcome pins
// the "never mask the primary failure cause" rule: when the run's own
// outcome is already non-success (max_turns here), a fatal postRun
// failure must not overwrite it with "hook_failed".
func TestLoop_Hooks_PostRunFatalFailure_DoesNotMaskNonSuccessOutcome(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = &infiniteToolCallProvider{}
	hooks := &fakeHookRunner{postErr: errSentinelPostHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	config.MaxTurns = 3

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "max_turns" {
		t.Errorf("Outcome = %q, want max_turns (must not be masked by the post-hook failure)", runTrace.Outcome)
	}
	if len(hooks.postCalls) != 1 || hooks.postCalls[0] != "max_turns" {
		t.Errorf("RunPost outcomes = %v, want [\"max_turns\"] (post still runs on every outcome by default)", hooks.postCalls)
	}
}

// TestLoop_Hooks_PostRunRunsOnTimeout pins two things at once: postRun
// hooks run on every outcome by default (including "timeout"), and the
// detached ctx handed to RunPost is not already dead even though the
// run's own wall-clock ctx has already expired by that point.
func TestLoop_Hooks_PostRunRunsOnTimeout(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			// Block long enough for the 50ms deadline below to fire
			// before the next turn boundary ctx check, matching the
			// existing TestLoop_CancelAttribute_Deadline pattern.
			time.Sleep(150 * time.Millisecond)
		},
	}
	hooks := &fakeHookRunner{}
	loop.Hooks = hooks

	config := buildTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "timeout" {
		t.Fatalf("prerequisite: Outcome = %q, want timeout", runTrace.Outcome)
	}
	if len(hooks.postCalls) != 1 || hooks.postCalls[0] != "timeout" {
		t.Fatalf("RunPost outcomes = %v, want [\"timeout\"]", hooks.postCalls)
	}
	if hooks.postCtxErrs[0] != nil {
		t.Errorf("RunPost ctx.Err() = %v, want nil (the detached post-hook ctx must not inherit the expired run ctx)", hooks.postCtxErrs[0])
	}
}

// TestLoop_Hooks_ContinueOnError_EmitsWarningTransportEvent pins that a
// continueOnError hook failure surfaces as a transport "warning" event
// and never touches the run's outcome.
func TestLoop_Hooks_ContinueOnError_EmitsWarningTransportEvent(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	hooks := &fakeHookRunner{
		postResults: []types.HookExecution{
			{Phase: "postRun", Index: 0, Name: "flaky", ContinuedOnError: true, Error: "exit code 1"},
		},
	}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success (continueOnError must never touch outcome)", runTrace.Outcome)
	}

	var warnings []types.HarnessEvent
	for _, ev := range rec.events {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("warning events = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0].Message, "flaky") {
		t.Errorf("warning message = %q, want it to name the hook", warnings[0].Message)
	}
}

// TestLoop_Hooks_HooklessUnchanged pins the byte-for-byte-unchanged
// guarantee: a run with Hooks left nil (a hand-assembled loop that
// never configured lifecycle hooks) emits no hooks.* spans and no
// warning events, and behaves exactly like a pre-#461 run.
func TestLoop_Hooks_HooklessUnchanged(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	// loop.Hooks intentionally left nil.

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", runTrace.Outcome)
	}
	for _, ev := range rec.events {
		if ev.Type == "warning" {
			t.Errorf("unexpected warning event on a hookless run: %+v", ev)
		}
	}
}

// TestLoop_Hooks_RecordedViaHookRecorder pins that hook results reach
// RunTrace.HookResults end-to-end through the real JSONL emitter's
// optional HookRecorder capability, not just through the fake in these
// other tests.
func TestLoop_Hooks_RecordedViaHookRecorder(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Trace = trace.NewJSONLTraceEmitter(discardWriter{})
	hooks := &fakeHookRunner{
		preResults:  []types.HookExecution{{Phase: "preRun", Index: 0, Name: "clone", ExitCode: 0}},
		postResults: []types.HookExecution{{Phase: "postRun", Index: 0, Name: "smoke", ExitCode: 0}},
	}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(runTrace.HookResults) != 2 {
		t.Fatalf("HookResults = %d entries, want 2", len(runTrace.HookResults))
	}
	if runTrace.HookResults[0].Name != "clone" || runTrace.HookResults[1].Name != "smoke" {
		t.Errorf("HookResults = %+v, want [clone, smoke] in order", runTrace.HookResults)
	}
}

// discardWriter is a minimal io.Writer that discards everything, used
// where a test needs a real JSONLTraceEmitter but does not care about
// the on-disk bytes.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestPostHookBudget pins postHookBudget's sizing directly: every
// loop-level hooks test above drives it only through a nil
// config.Hooks (the fakeHookRunner tests never set buildTestConfig's
// Hooks field), which only exercises the "just the 30s margin" branch.
// This pins the sum-of-effective-timeouts branch that actually sizes
// the detached post-hook ctx.
func TestPostHookBudget(t *testing.T) {
	cases := []struct {
		name  string
		hooks *types.HooksConfig
		want  time.Duration
	}{
		{"nil config", nil, 30 * time.Second},
		{"empty config", &types.HooksConfig{}, 30 * time.Second},
		{
			"sums effective postRun timeouts plus margin",
			&types.HooksConfig{PostRun: []types.HookConfig{
				{Command: "true", TimeoutSeconds: 120},
				{Command: "true", TimeoutSeconds: 60},
			}},
			(120 + 60 + 30) * time.Second,
		},
		{
			"zero timeout resolves to default before summing",
			&types.HooksConfig{PostRun: []types.HookConfig{{Command: "true"}}},
			(time.Duration(types.DefaultHookTimeoutSeconds) + 30) * time.Second,
		},
		{
			"preRun entries do not contribute",
			&types.HooksConfig{PreRun: []types.HookConfig{{Command: "true", TimeoutSeconds: 900}}},
			30 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postHookBudget(tc.hooks); got != tc.want {
				t.Errorf("postHookBudget(%+v) = %v, want %v", tc.hooks, got, tc.want)
			}
		})
	}
}
