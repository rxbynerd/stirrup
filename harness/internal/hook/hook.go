// Package hook implements lifecycle hooks: operator-authored exec
// commands that run before the agentic session starts (preRun) and
// after it ends (postRun). See docs/configuration.md#lifecycle-hooks.
package hook

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// Phase names recorded on types.HookExecution.Phase, and used as the
// OTel span name suffix ("hooks."+Phase) by the agentic loop.
const (
	PhasePreRun  = "preRun"
	PhasePostRun = "postRun"
)

// Runner executes a run's configured lifecycle hooks. Injected onto
// AgenticLoop.Hooks by the factory (Noop when none configured, or for
// a sub-agent), the same optional-component pattern as GuardRail/RuleOfTwo.
type Runner interface {
	// RunPre executes every configured preRun hook, in order, before
	// GitStrategy.Setup. Skipped entries stay Index-aligned with the
	// configured list. A non-nil error is a fatal phase failure
	// (outcome "setup_failed").
	RunPre(ctx context.Context) ([]types.HookExecution, error)

	// RunPost executes every configured postRun hook whose RunOn filter
	// matches outcome, in order, after GitStrategy.Finalise. The caller
	// overrides outcome to "hook_failed" only when outcome was "success".
	RunPost(ctx context.Context, outcome string) ([]types.HookExecution, error)
}
