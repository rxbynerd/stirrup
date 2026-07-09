package hook

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// Noop is a Runner that executes no hooks. Injected for sub-agent loops
// (hooks are parent-run-only) and for any run with no HooksConfig, so
// AgenticLoop.Hooks is never a bare nil interface in production.
type Noop struct{}

// NewNoop returns a Runner that never dispatches a hook.
func NewNoop() *Noop {
	return &Noop{}
}

// RunPre is a no-op.
func (*Noop) RunPre(_ context.Context) ([]types.HookExecution, error) {
	return nil, nil
}

// RunPost is a no-op.
func (*Noop) RunPost(_ context.Context, _ string) ([]types.HookExecution, error) {
	return nil, nil
}
