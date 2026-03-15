package git

import "context"

// NoneGitStrategy is a GitStrategy that performs no version control
// operations. All methods are no-ops.
type NoneGitStrategy struct{}

// NewNoneGitStrategy returns a new NoneGitStrategy.
func NewNoneGitStrategy() *NoneGitStrategy {
	return &NoneGitStrategy{}
}

// Setup is a no-op.
func (n *NoneGitStrategy) Setup(_ context.Context, _ string, _ string) error {
	return nil
}

// Checkpoint is a no-op.
func (n *NoneGitStrategy) Checkpoint(_ context.Context, _ string) error {
	return nil
}

// Finalise returns an empty GitResult.
func (n *NoneGitStrategy) Finalise(_ context.Context) (*GitResult, error) {
	return &GitResult{}, nil
}
