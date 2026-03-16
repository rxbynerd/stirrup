// Package git defines the GitStrategy interface and implementations for
// managing version control during harness runs.
package git

import "context"

// GitResult holds the version control state after a run completes.
type GitResult struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// GitStrategy manages git operations during a harness run: creating
// branches, checkpointing work, and finalising the result.
type GitStrategy interface {
	// Setup prepares the workspace for a run (e.g. creating a branch).
	Setup(ctx context.Context, workspace string, taskID string) error

	// Checkpoint commits the current workspace state with the given message.
	Checkpoint(ctx context.Context, message string) error

	// Finalise completes the git workflow and returns the final branch/SHA.
	Finalise(ctx context.Context) (*GitResult, error)
}
