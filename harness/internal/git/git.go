// Package git defines the GitStrategy interface and implementations for
// managing version control during harness runs.
package git

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// GitStrategy manages git operations during a harness run: creating
// branches, checkpointing work, and finalising the result.
type GitStrategy interface {
	// Setup prepares the workspace for a run (e.g. creating a branch).
	Setup(ctx context.Context, workspace string, taskID string) error

	// Checkpoint commits the current workspace state with the given message.
	Checkpoint(ctx context.Context, message string) error

	// Finalise commits any outstanding work with a message naming the
	// run's outcome and reports the resulting git state. It returns a nil
	// handoff when the strategy does not manage git. A strategy that does
	// returns a non-nil handoff even alongside an error, filled in as far
	// as the failure allowed.
	Finalise(ctx context.Context, outcome string) (*types.GitHandoff, error)
}
