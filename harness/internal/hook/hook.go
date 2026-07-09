// Package hook implements lifecycle hooks: operator-authored exec
// commands that run before the agentic session starts (preRun — clone a
// repo, provision a runtime) and after it ends (postRun — submit
// artifacts, run a smoke test). See issue #461.
//
// Hook output is trace-only: it is recorded on types.HookExecution and
// never enters the model's context. Hooks run through the same Executor
// the agent's tools use, so they share the run's sandbox and network
// egress posture — there is no separate credential or network surface.
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
// AgenticLoop.Hooks by the factory (Noop when the run has none
// configured, or is a sub-agent) so the loop's call sites need only a
// nil-interface check, the same optional-component pattern as
// GuardRail/RuleOfTwo.
type Runner interface {
	// RunPre executes every configured preRun hook, in order, before
	// GitStrategy.Setup. Returns every hook's result — including any
	// skipped after a fatal failure, so Index stays aligned with the
	// configured list — and a non-nil error the instant a
	// non-continueOnError hook fails. The caller treats a non-nil error
	// as a fatal phase failure (outcome "setup_failed").
	RunPre(ctx context.Context) ([]types.HookExecution, error)

	// RunPost executes every configured postRun hook whose RunOn filter
	// matches outcome, in order, after GitStrategy.Finalise. Returns
	// every hook's result — runOn-filtered and post-fatal-failure
	// entries are recorded as Skipped rather than omitted, so Index
	// stays aligned with the configured list — and a non-nil error the
	// instant a non-continueOnError hook fails. The caller overrides
	// outcome to "hook_failed" only when outcome was "success".
	RunPost(ctx context.Context, outcome string) ([]types.HookExecution, error)
}
