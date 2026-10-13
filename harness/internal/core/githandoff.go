package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

var errDirtyAfterFinalCheckpoint = errors.New("working tree has uncommitted changes after the final checkpoint")

// finaliseGit makes the git strategy's final checkpoint on a detached
// context, records the resulting handoff on the trace, and returns the
// run's outcome. A final checkpoint that fails or leaves the working tree
// dirty turns an otherwise-successful run into "handoff_failed"; an
// earlier failure outcome is kept.
func (l *AgenticLoop) finaliseGit(ctx context.Context, outcome string) string {
	finaliseCtx, cancel := l.detachedContext(ctx, gitFinaliseBudget)
	defer cancel()
	_, span := l.Tracer.Start(l.traceCtx(ctx), "git.finalise")
	defer span.End()

	handoff, err := l.Git.Finalise(finaliseCtx, outcome)
	if handoff == nil && err == nil {
		return outcome
	}
	if handoff == nil {
		handoff = &types.GitHandoff{}
	}
	if err == nil && handoff.Dirty {
		err = errDirtyAfterFinalCheckpoint
	}
	if err != nil {
		scrubbed := recordSpanError(span, err)
		handoff.Error = capFailureText(scrubbed)
		l.Logger.WarnContext(l.traceCtx(ctx), "git finalise failed", "error", scrubbed)
		_ = l.Transport.Emit(types.HarnessEvent{
			Type:    "warning",
			Message: fmt.Sprintf("git finalise: %s", scrubbed),
		})
	}
	if recorder, ok := l.Trace.(trace.GitHandoffRecorder); ok {
		recorder.RecordGitHandoff(*handoff)
	}
	if err != nil && outcome == "success" {
		l.noteFailure("handoff_failed", "git finalise: "+handoff.Error)
		return "handoff_failed"
	}
	return outcome
}
