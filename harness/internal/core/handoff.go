package core

import (
	"context"
	"fmt"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// maxFailureTextBytes bounds every failure text the loop carries on the
// trace and RunResult.
const maxFailureTextBytes = 1024

// handoffBudget bounds the workspace handoff.
const handoffBudget = 5 * time.Minute

func capFailureText(s string) string {
	capped, _ := types.CapFinalAssistantText(s, maxFailureTextBytes)
	return capped
}

// runFailure is the reason recorded for one failure outcome of a run.
type runFailure struct {
	outcome string
	text    string
}

// noteFailure records text as the reason for outcome. The reason is
// reported only if outcome is still the run's outcome when it ends, so a
// later reclassification (for example to "cancelled") falls back to
// describeOutcome.
func (l *AgenticLoop) noteFailure(outcome, text string) {
	l.failure = runFailure{outcome: outcome, text: text}
}

// runErrorText returns the scrubbed, length-capped RunTrace.Error for a
// run that ended with outcome; empty for "success".
func (l *AgenticLoop) runErrorText(config *types.RunConfig, outcome string) string {
	if outcome == "success" {
		return ""
	}
	text := l.failure.text
	if l.failure.outcome != outcome || text == "" {
		text = describeOutcome(config, outcome)
	}
	return capFailureText(security.Scrub(text))
}

// describeOutcome is the error text for an outcome whose cause carries
// no error of its own.
func describeOutcome(config *types.RunConfig, outcome string) string {
	switch outcome {
	case "max_turns":
		return fmt.Sprintf("reached the turn limit (maxTurns %d)", config.MaxTurns)
	case "budget_exceeded":
		return "exceeded the run's token budget"
	case "timeout":
		return "reached the run timeout"
	case "cancelled":
		return "cancelled before the run completed"
	case "control_stream_lost":
		return "the control stream ended before the run completed"
	case "stalled":
		return "stopped after repeated identical tool calls"
	case "tool_failures":
		return "stopped after too many consecutive tool failures"
	case "verification_failed":
		return "verification failed after the final retry"
	case "structured_output_failed":
		return "the final answer did not satisfy outputSchema after the corrective attempts"
	case "guardrail_blocked":
		return "blocked by the guardrail"
	case "rule_of_two_violation":
		return "aborted by Rule-of-Two enforcement"
	default:
		return fmt.Sprintf("run ended with outcome %q", outcome)
	}
}

// hasWorkspace reports whether an executor type provides a workspace to
// hand off.
func hasWorkspace(executorType string) bool {
	return executorType != "none" && executorType != "api"
}

// handoffWorkspace exports the run's workspace on a detached context,
// records the receipt on the trace, and returns the run's outcome. A
// failed required export turns an otherwise-successful run into
// "handoff_failed"; an earlier failure outcome is kept.
func (l *AgenticLoop) handoffWorkspace(ctx context.Context, config *types.RunConfig, outcome string) string {
	if l.Handoff == nil || !hasWorkspace(config.Executor.Type) {
		return outcome
	}
	handoffCtx, cancel := l.detachedContext(ctx, handoffBudget)
	defer cancel()
	_, span := l.Tracer.Start(l.traceCtx(ctx), "workspace.handoff")
	defer span.End()

	receipt, err := l.Handoff.Handoff(handoffCtx, config, l.Executor)
	if receipt == nil && err == nil {
		return outcome
	}
	if receipt == nil {
		receipt = &types.ExportReceipt{}
	}
	receipt.Required = config.Executor.ExportRequired()
	if err != nil {
		scrubbed := recordSpanError(span, err)
		receipt.Status = types.ExportStatusFailed
		receipt.Error = capFailureText(scrubbed)
		l.Logger.WarnContext(l.traceCtx(ctx), "workspace export failed", "uri", receipt.URI, "required", receipt.Required, "error", scrubbed)
		_ = l.Transport.Emit(types.HarnessEvent{
			Type:    "warning",
			Message: fmt.Sprintf("workspace export: %s", scrubbed),
		})
	}
	if recorder, ok := l.Trace.(trace.WorkspaceExportRecorder); ok {
		recorder.RecordWorkspaceExport(*receipt)
	}
	if err != nil && receipt.Required && outcome == "success" {
		l.noteFailure("handoff_failed", "workspace export: "+receipt.Error)
		return "handoff_failed"
	}
	return outcome
}

// finishRun records the run's error text, finishes the trace, and emits
// "done" carrying it. Every Run exit after Trace.Start ends here, after
// the git, hook, command-output and workspace handoff steps, so a
// control plane acting on "done" observes their results. stopHeartbeat
// runs just before "done". The returned error is the trace persistence
// failure; the trace carries its text when the run has no earlier error.
func (l *AgenticLoop) finishRun(ctx context.Context, config *types.RunConfig, outcome string, stopHeartbeat func()) (*types.RunTrace, error) {
	errText := l.runErrorText(config, outcome)
	if recorder, ok := l.Trace.(trace.RunErrorRecorder); ok && errText != "" {
		recorder.RecordRunError(errText)
	}
	runTrace, traceErr := l.finishTrace(ctx, outcome)
	if runTrace != nil && runTrace.Error == "" {
		switch {
		case errText != "":
			runTrace.Error = errText
		case traceErr != nil:
			runTrace.Error = capFailureText(security.Scrub("finish trace: " + traceErr.Error()))
		}
	}

	stopHeartbeat()
	// "done" is the control plane's cue to send its next control event;
	// from here the run is no longer cancellable, so a cancel must route
	// as idle rather than to this run's context.
	l.setActiveRun(nil)
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
		Trace:      doneSummary(runTrace),
	}); err != nil {
		l.logEmitFailure("done", err)
	}
	return runTrace, traceErr
}

// doneSummary is the part of a RunTrace that "done" carries: the fields
// the proto RunTrace summary mirrors, including the structured output.
// The config, tool calls, hook results and final assistant text stay on
// the trace emitter and result sink, off the event stream.
func doneSummary(rt *types.RunTrace) *types.RunTrace {
	if rt == nil {
		return nil
	}
	return &types.RunTrace{
		ID:               rt.ID,
		StartedAt:        rt.StartedAt,
		CompletedAt:      rt.CompletedAt,
		Turns:            rt.Turns,
		TokenUsage:       rt.TokenUsage,
		Outcome:          rt.Outcome,
		StopDetails:      rt.StopDetails,
		Git:              rt.Git,
		WorkspaceExport:  rt.WorkspaceExport,
		Error:            rt.Error,
		StructuredOutput: rt.StructuredOutput,
	}
}
