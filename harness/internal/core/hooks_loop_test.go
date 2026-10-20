package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
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
// loop's lifecycle-hook integration tests. It records every RunPost
// call's outcome argument (and the ctx's own Err() at call time,
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

	// onPost, when set, is invoked synchronously at the start of
	// RunPost with the ctx the loop handed in — used by tests that
	// need to observe or block on that ctx (e.g. simulating an
	// in-flight hook that only returns once its ctx is cancelled).
	onPost func(ctx context.Context)

	// onPre, when set, is invoked at the start of RunPre so a test can
	// stand in for a hook that mutates the workspace.
	onPre func()
}

var _ hook.Runner = (*fakeHookRunner)(nil)

func (f *fakeHookRunner) RunPre(_ context.Context) ([]types.HookExecution, error) {
	f.preCalls++
	if f.onPre != nil {
		f.onPre()
	}
	return f.preResults, f.preErr
}

func (f *fakeHookRunner) RunPost(ctx context.Context, outcome string) ([]types.HookExecution, error) {
	f.postCalls = append(f.postCalls, outcome)
	f.postCtxErrs = append(f.postCtxErrs, ctx.Err())
	if f.onPost != nil {
		f.onPost(ctx)
	}
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
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{preErr: errSentinelPreHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	// finishWithOutcome surfaces the underlying hook error as Run()'s Go
	// error (see TestLoop_PromptBuildError), alongside the populated
	// RunTrace reporting the classified outcome.
	must.Error(err, "Run() must fail on a fatal pre-run hook failure")
	must.ErrorContains(err, "pre-run hooks")
	check.Equal("setup_failed", runTrace.Outcome)
	check.Equal(0, runTrace.Turns, "the inner loop must never start")
	check.Equal(1, hooks.preCalls, "RunPre calls")
	check.Empty(hooks.postCalls, "post is skipped after a pre-run failure")
}

// TestLoop_Hooks_PreRunFatalFailure_EmitsDoneEvent pins that a fatal
// preRun hook failure emits a terminal "done" HarnessEvent
// (StopReason=outcome), not just the "error" event — control planes
// watch for the terminal "done" event per docs/deployment.md.
func TestLoop_Hooks_PreRunFatalFailure_EmitsDoneEvent(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	hooks := &fakeHookRunner{preErr: errSentinelPreHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	must.Error(err, "Run() must fail on a fatal pre-run hook failure")
	must.Equal("setup_failed", runTrace.Outcome)

	var sawError, sawDone bool
	var doneStopReason string
	for _, ev := range rec.events {
		switch ev.Type {
		case "error":
			sawError = true
		case "done":
			sawDone = true
			doneStopReason = ev.StopReason
		}
	}
	check.True(sawError, "expected an \"error\" event on the transport")
	must.True(sawDone, "expected a \"done\" event on the transport")
	check.Equal("setup_failed", doneStopReason, "done event StopReason")
}

// TestLoop_Hooks_PreRunFatalFailure_CtxDeadWinsOverSetupFailed pins that
// a ctx already dead (deadline/cancel) at pre-hook failure time is
// reported via classifyCtxOutcome, not the generic "setup_failed" — the
// hook almost certainly failed because the deadline hit.
func TestLoop_Hooks_PreRunFatalFailure_CtxDeadWinsOverSetupFailed(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{preErr: errSentinelPreHookFailure}
	loop.Hooks = hooks

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancelled without a cause, so classifyCtxOutcome reports "cancelled".

	config := buildTestConfig()
	runTrace, err := loop.Run(ctx, config)
	must.Error(err, "Run() must fail on a fatal pre-run hook failure")
	must.Equal("cancelled", runTrace.Outcome, "ctx-cause classification must win over setup_failed")
}

// TestLoop_Hooks_PostRunFatalFailure_OverridesSuccessOnly pins the
// outcome-override rule: a fatal postRun failure turns an otherwise-
// successful run's outcome into "hook_failed".
func TestLoop_Hooks_PostRunFatalFailure_OverridesSuccessOnly(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	hooks := &fakeHookRunner{postErr: errSentinelPostHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("hook_failed", runTrace.Outcome)
	check.Equal([]string{"success"}, hooks.postCalls, "RunPost outcomes")
}

// TestLoop_Hooks_PostRunFatalFailure_DoesNotMaskNonSuccessOutcome pins
// the "never mask the primary failure cause" rule: when the run's own
// outcome is already non-success (max_turns here), a fatal postRun
// failure must not overwrite it with "hook_failed".
func TestLoop_Hooks_PostRunFatalFailure_DoesNotMaskNonSuccessOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = &infiniteToolCallProvider{}
	hooks := &fakeHookRunner{postErr: errSentinelPostHookFailure}
	loop.Hooks = hooks

	config := buildTestConfig()
	config.MaxTurns = 3

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("max_turns", runTrace.Outcome, "the post-hook failure must not mask the run outcome")
	check.Equal([]string{"max_turns"}, hooks.postCalls, "post still runs on every outcome by default")
}

// TestLoop_Hooks_PostRunRunsOnTimeout pins two things at once: postRun
// hooks run on every outcome by default (including "timeout"), and the
// detached ctx handed to RunPost is not already dead even though the
// run's own wall-clock ctx has already expired by that point.
func TestLoop_Hooks_PostRunRunsOnTimeout(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			// Block long enough for the 50ms deadline below to fire
			// before the next turn boundary ctx check.
			time.Sleep(150 * time.Millisecond)
		},
	}
	hooks := &fakeHookRunner{}
	loop.Hooks = hooks

	config := buildTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	runTrace, err := loop.Run(ctx, config)
	must.NoError(err)
	must.Equal("timeout", runTrace.Outcome)
	must.Equal([]string{"timeout"}, hooks.postCalls, "RunPost outcomes")
	must.NoError(hooks.postCtxErrs[0], "the detached post-hook ctx must not inherit the expired run ctx")
}

// TestLoop_Hooks_PostRunCutShortByShutdownSignal pins that a detached
// postRun hook survives the run's own wall-clock deadline/cancel
// (covered by the timeout test above), but a distinct process-shutdown
// signal on l.Shutdown still cuts it off well before its full
// configured budget.
func TestLoop_Hooks_PostRunCutShortByShutdownSignal(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	loop.Shutdown = shutdownCtx

	hookCtxCancelled := make(chan struct{})
	hooks := &fakeHookRunner{
		onPost: func(ctx context.Context) {
			// Simulate a long-running hook that only returns once its
			// ctx is cancelled.
			<-ctx.Done()
			close(hookCtxCancelled)
		},
	}
	loop.Hooks = hooks

	config := buildTestConfig()
	// A generous budget the hook would otherwise be entitled to run
	// for; the shutdown signal must cut it short well before this
	// elapses.
	config.Hooks = &types.HooksConfig{PostRun: []types.HookConfig{
		{Command: "true", TimeoutSeconds: 60},
	}}

	// Fire the shutdown signal shortly after Run() starts, simulating
	// a SIGTERM arriving while the postRun hook is in flight.
	go func() {
		time.Sleep(20 * time.Millisecond)
		shutdownCancel()
	}()

	start := time.Now()
	runTrace, err := loop.Run(context.Background(), config)
	elapsed := time.Since(start)

	must.NoError(err)
	check.Equal("success", runTrace.Outcome)

	cancelled := false
	select {
	case <-hookCtxCancelled:
		cancelled = true
	default:
	}
	check.True(cancelled, "the in-flight hook's postCtx was never cancelled by the shutdown signal")
	check.LessOrEqual(elapsed, 2*time.Second, "shutdown must cut postRun short of its 60s configured timeout")
}

// TestLoop_Hooks_ContinueOnError_EmitsWarningTransportEvent pins that a
// continueOnError hook failure surfaces as a transport "warning" event
// and never touches the run's outcome.
func TestLoop_Hooks_ContinueOnError_EmitsWarningTransportEvent(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	check.Equal("success", runTrace.Outcome, "continueOnError must never touch outcome")

	var warnings []types.HarnessEvent
	for _, ev := range rec.events {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}
	must.Len(warnings, 1, "warning events")
	must.Contains(warnings[0].Message, "flaky", "warning message should name the hook")
}

// TestLoop_Hooks_NilHooks_RunsWithoutWarnings pins that a run with Hooks
// left nil (a hand-assembled loop that never configured lifecycle hooks)
// succeeds and emits no warning events.
func TestLoop_Hooks_NilHooks_RunsWithoutWarnings(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	// loop.Hooks intentionally left nil.

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)

	var warnings []types.HarnessEvent
	for _, ev := range rec.events {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}
	check.Empty(warnings, "a hookless run must emit no warning events")
}

// TestLoop_Hooks_ResultsReachRunTrace pins that hook results reach
// RunTrace.HookResults through the JSONL trace emitter's optional
// HookRecorder capability.
func TestLoop_Hooks_ResultsReachRunTrace(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Trace = trace.NewJSONLTraceEmitter(discardWriter{}, false)
	hooks := &fakeHookRunner{
		preResults:  []types.HookExecution{{Phase: "preRun", Index: 0, Name: "clone", ExitCode: 0}},
		postResults: []types.HookExecution{{Phase: "postRun", Index: 0, Name: "smoke", ExitCode: 0}},
	}
	loop.Hooks = hooks

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	names := make([]string, 0, len(runTrace.HookResults))
	for _, result := range runTrace.HookResults {
		names = append(names, result.Name)
	}
	must.Equal([]string{"clone", "smoke"}, names, "hook results in run order")
}

// discardWriter is a minimal io.Writer that discards everything, used
// where a test needs a real JSONLTraceEmitter but does not care about
// the on-disk bytes.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestPostHookBudget_SizesFromPostRunTimeouts pins postHookBudget's sizing:
// the 30s margin alone when no postRun hook is configured, otherwise the
// sum of the effective postRun timeouts plus that margin. preRun entries
// do not contribute.
func TestPostHookBudget_SizesFromPostRunTimeouts(t *testing.T) {
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
			must := require.New(t)
			must.Equal(tc.want, postHookBudget(tc.hooks))
		})
	}
}

// workspaceProbeBuilder records whether a marker file existed in the
// workspace when Build ran, and how often Build ran.
type workspaceProbeBuilder struct {
	marker string
	calls  int
	seen   bool
}

func (b *workspaceProbeBuilder) Build(_ context.Context, _ prompt.PromptContext) (string, error) {
	b.calls++
	_, err := os.Stat(b.marker)
	b.seen = err == nil
	return "system prompt", nil
}

// recordingPromptTrace wraps a JSONL emitter with the optional prompt
// recorder capabilities and logs when each is invoked.
type recordingPromptTrace struct {
	*trace.JSONLTraceEmitter
	log               *[]string
	systemInstruction []string
	resolutions       []string
}

func (e *recordingPromptTrace) RecordSystemInstructions(instructions string) {
	e.systemInstruction = append(e.systemInstruction, instructions)
	*e.log = append(*e.log, "record_system_instructions")
}

func (e *recordingPromptTrace) RecordPromptResolution(model, tier string) {
	e.resolutions = append(e.resolutions, model+"/"+tier)
	*e.log = append(*e.log, "record_prompt_resolution")
}

func TestLoop_Hooks_PromptBuiltAfterPreRunHook(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "cloned.txt")

	var log []string
	builder := &workspaceProbeBuilder{marker: marker}
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Prompt = builder
	loop.Hooks = &fakeHookRunner{onPre: func() {
		log = append(log, "pre_hook")
		check.NoError(os.WriteFile(marker, []byte("x"), 0o644), "write marker file")
	}}
	rec := &recordingPromptTrace{JSONLTraceEmitter: trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false), log: &log}
	loop.Trace = rec

	config := buildTestConfig()
	config.Executor.Workspace = workspace
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.Equal(1, builder.calls, "Build calls")
	check.True(builder.seen, "Build did not see the file created by the pre-run hook")
	check.Equal([]string{"pre_hook", "record_system_instructions", "record_prompt_resolution"}, log, "event order")
	check.Equal([]string{"system prompt"}, rec.systemInstruction, "RecordSystemInstructions calls")
	check.Len(rec.resolutions, 1, "RecordPromptResolution calls")
}

func TestLoop_Hooks_PreRunFatalFailure_NeverBuildsPrompt(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var log []string
	builder := &workspaceProbeBuilder{marker: filepath.Join(t.TempDir(), "absent")}
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Prompt = builder
	loop.Hooks = &fakeHookRunner{preErr: errSentinelPreHookFailure}
	rec := &recordingPromptTrace{JSONLTraceEmitter: trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false), log: &log}
	loop.Trace = rec

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.Error(err, "Run() must fail on a fatal pre-run hook failure")
	check.Equal("setup_failed", runTrace.Outcome)
	check.Zero(builder.calls, "Build must not run after a failed pre-run hook")
	check.Empty(log, "prompt recorders must not run after a failed pre-run hook")
}

func TestLoop_Hooks_PromptBuildErrorAfterHooksSkipsPostRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Prompt = &failingPromptBuilder{err: errors.New("builder boom")}
	hooks := &fakeHookRunner{}
	loop.Hooks = hooks

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.ErrorContains(err, "build system prompt")
	check.Equal("error", runTrace.Outcome)
	check.Equal(1, hooks.preCalls, "hooks run before the prompt build")
	check.Empty(hooks.postCalls, "post must not run after an early failure")
}
