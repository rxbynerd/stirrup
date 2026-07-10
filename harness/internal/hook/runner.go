package hook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// maxOutputTailBytes caps the trace-recorded tail of a hook's combined
// stdout+stderr. Bounded because hook results are trace-only and
// persisted verbatim; "tail" (not head) because a failing command's most
// useful diagnostic — the error summary — is usually printed last.
const maxOutputTailBytes = 4 * 1024

// ExecRunner is the production hook.Runner: it executes every configured
// hook via the run's Executor, in order, honouring per-hook timeout
// resolution, continueOnError, and (for postRun) the RunOn outcome
// filter. Mirrors the GitStrategy injection pattern: constructed and
// owned by the factory, held on AgenticLoop.Hooks as the Runner
// interface.
type ExecRunner struct {
	// Hooks is the run's configured HooksConfig. Must be non-nil; the
	// factory only constructs an ExecRunner when at least one hook is
	// configured (see buildHookRunner), otherwise it injects Noop.
	Hooks *types.HooksConfig

	// Exec is the run's Executor. Hooks dispatch through it so they
	// share the run's sandbox and network egress posture with every
	// agent tool call.
	Exec executor.Executor

	// Logger receives per-hook diagnostics. Optional; nil falls back to
	// slog.Default().
	Logger *slog.Logger
}

// RunPre executes every configured preRun hook in order.
func (r *ExecRunner) RunPre(ctx context.Context) ([]types.HookExecution, error) {
	if r.Hooks == nil {
		return nil, nil
	}
	return r.run(ctx, PhasePreRun, r.Hooks.PreRun, "", false)
}

// RunPost executes every configured postRun hook whose RunOn matches
// outcome, in order.
func (r *ExecRunner) RunPost(ctx context.Context, outcome string) ([]types.HookExecution, error) {
	if r.Hooks == nil {
		return nil, nil
	}
	return r.run(ctx, PhasePostRun, r.Hooks.PostRun, outcome, true)
}

// run executes hooks in order against ctx, applying the RunOn filter
// (postRun only — applyRunOnFilter is false for preRun, where RunOn is
// always empty per ValidateRunConfig) and skip-and-mark once a fatal
// (non-continueOnError) failure occurs. Every hook produces exactly one
// types.HookExecution — including runOn-filtered and post-fatal-failure
// entries, marked Skipped — so Index always stays aligned with the
// configured list. Returns a non-nil error the instant a fatal failure
// occurs; RunPre/RunPost callers treat that as phase failure.
func (r *ExecRunner) run(ctx context.Context, phase string, hooks []types.HookConfig, outcome string, applyRunOnFilter bool) ([]types.HookExecution, error) {
	results := make([]types.HookExecution, 0, len(hooks))
	var fatalErr error

	for i, h := range hooks {
		if applyRunOnFilter && !hookRuns(h.RunOn, outcome) {
			results = append(results, types.HookExecution{
				Phase: phase, Index: i, Name: h.Name, Command: h.Command, Skipped: true,
			})
			continue
		}
		if fatalErr != nil {
			results = append(results, types.HookExecution{
				Phase: phase, Index: i, Name: h.Name, Command: h.Command, Skipped: true,
			})
			continue
		}

		exec := r.runOne(ctx, phase, i, h)
		results = append(results, exec)

		if exec.Error != "" && !h.ContinueOnError {
			fatalErr = fmt.Errorf("%s hook %d (%s) failed: %s", phase, i, hookLabel(h), exec.Error)
		}
	}

	return results, fatalErr
}

// runOne dispatches a single hook through the Executor and builds its
// types.HookExecution result. Never returns an error directly — failure
// is reported via the result's Error/TimedOut fields so run's
// skip-and-mark bookkeeping has a uniform shape to inspect.
func (r *ExecRunner) runOne(ctx context.Context, phase string, index int, h types.HookConfig) types.HookExecution {
	result := types.HookExecution{
		Phase:   phase,
		Index:   index,
		Name:    h.Name,
		Command: h.Command,
	}

	if r.Exec == nil {
		result.Error = "hook runner misconfigured: no executor"
		return result
	}

	timeout := time.Duration(types.EffectiveHookTimeout(h)) * time.Second
	if caps := r.Exec.Capabilities(); caps.MaxTimeout > 0 && timeout > caps.MaxTimeout {
		timeout = caps.MaxTimeout
	}

	start := time.Now()
	execResult, err := r.Exec.Exec(ctx, h.Command, timeout)
	result.DurationMs = time.Since(start).Milliseconds()

	if execResult != nil {
		result.ExitCode = execResult.ExitCode
		result.OutputTail, result.Truncated = scrubbedTail(execResult.Stdout, execResult.Stderr)
	}

	switch {
	case err != nil:
		result.TimedOut = isTimeoutErr(err)
		result.Error = err.Error()
		result.ContinuedOnError = h.ContinueOnError
		r.logger().Warn("lifecycle hook failed", "phase", phase, "index", index, "name", h.Name, "error", err)
	case execResult.ExitCode != 0:
		result.Error = fmt.Sprintf("exit code %d", execResult.ExitCode)
		result.ContinuedOnError = h.ContinueOnError
		r.logger().Warn("lifecycle hook exited non-zero", "phase", phase, "index", index, "name", h.Name, "exitCode", execResult.ExitCode)
	default:
		r.logger().Debug("lifecycle hook succeeded", "phase", phase, "index", index, "name", h.Name, "durationMs", result.DurationMs)
	}

	return result
}

func (r *ExecRunner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// hookRuns reports whether a postRun hook with the given RunOn value
// should execute for outcome. Empty and "always" run unconditionally;
// "success" / "failure" gate on whether outcome == "success".
func hookRuns(runOn, outcome string) bool {
	switch runOn {
	case "success":
		return outcome == "success"
	case "failure":
		return outcome != "success"
	default:
		// "" and "always"; ValidateRunConfig rejects any other value,
		// but default-to-run is the safe interpretation for an
		// unrecognised value reaching here regardless.
		return true
	}
}

// hookLabel returns the identifier used in fatalErr messages: the
// operator-supplied Name when present, otherwise a positional fallback
// (the full Command can be up to 16KB and is not suitable for an error
// string).
func hookLabel(h types.HookConfig) string {
	if h.Name != "" {
		return h.Name
	}
	return "unnamed"
}

// isTimeoutErr reports whether err represents a hook execution that was
// killed by its timeout rather than failing on its own. Executor
// implementations signal this differently: k8s_execcore.go returns the
// context error verbatim (context.DeadlineExceeded), while local.go and
// container.go wrap it in a formatted "command timed out after %s"
// string with no %w. The substring check covers the latter shape.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(err.Error(), "timed out")
}

// scrubbedTail returns the scrubbed (security.Scrub), 4KB-tail-capped
// combined stdout+stderr for a hook result, along with whether the
// persisted (scrubbed) output exceeded the cap. Scrubbing runs over the
// full combined text before truncation so a secret pattern straddling
// the truncation boundary is still matched.
func scrubbedTail(stdout, stderr string) (tail string, truncated bool) {
	combined := stdout
	if stderr != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += stderr
	}
	scrubbed := security.Scrub(combined)
	if len(scrubbed) <= maxOutputTailBytes {
		return scrubbed, false
	}
	// The byte-index cut below can land mid-rune when a multi-byte UTF-8
	// character straddles the boundary; trimToRuneBoundary drops the
	// resulting leading continuation bytes so OutputTail is always valid
	// UTF-8 rather than json.Marshal silently substituting U+FFFD at the
	// start of the persisted tail.
	return trimToRuneBoundary(scrubbed[len(scrubbed)-maxOutputTailBytes:]), true
}

// trimToRuneBoundary drops any leading bytes of s that are UTF-8
// continuation bytes (i.e. cannot start a rune), so a caller that
// sliced s at an arbitrary byte offset gets back a string that starts
// on a rune boundary. Bounded to at most utf8.UTFMax-1 bytes dropped —
// the longest a valid rune's continuation-byte run can be.
func trimToRuneBoundary(s string) string {
	i := 0
	for i < len(s) && i < utf8.UTFMax && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
