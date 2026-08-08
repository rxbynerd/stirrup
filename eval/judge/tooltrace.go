package judge

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// evaluateToolTrace inspects the run's RunTrace.ToolCalls: where
// file-exists / file-contains confirm the agent reached the right end
// state, tool-trace confirms it got there via the expected tool-use path.
// A nil JudgeContext.Trace is a hard error, not a silent pass, so a
// misconfigured runner (trace not threaded through) is not masked.
func evaluateToolTrace(j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if j.ToolTrace == nil {
		return eval.JudgeVerdict{}, fmt.Errorf("tool-trace judge requires a toolTrace block")
	}
	if jctx.Trace == nil {
		return eval.JudgeVerdict{}, fmt.Errorf("tool-trace judge requires a run trace but none was provided")
	}

	crit := j.ToolTrace
	calls := jctx.Trace.ToolCalls

	if verdict, ok := checkSequence(crit.Sequence, calls); !ok {
		return verdict, nil
	}
	for _, exp := range crit.Calls {
		if verdict, ok := checkExpectation(exp, calls); !ok {
			return verdict, nil
		}
	}
	if crit.ForbidUnknown {
		if verdict, ok := checkNoUnresolvedUnknown(calls); !ok {
			return verdict, nil
		}
	}

	return eval.JudgeVerdict{
		Passed: true,
		Reason: fmt.Sprintf("tool-trace satisfied over %d tool call(s)", len(calls)),
	}, nil
}

// internalName returns the canonical internal tool ID for a recorded call.
// Under a toolset profile the model-facing Name is an alias and
// InternalName carries the resolved ID; matching on the internal ID means a
// trace assertion holds regardless of the active profile.
func internalName(c types.ToolCallSummary) string {
	if c.InternalName != "" {
		return c.InternalName
	}
	return c.Name
}

// checkSequence verifies that each name in want appears at least once, in
// the given relative order, among the recorded calls. Calls between the
// matched names are permitted — only the relative order of the named tools
// is enforced.
func checkSequence(want []string, calls []types.ToolCallSummary) (eval.JudgeVerdict, bool) {
	if len(want) == 0 {
		return eval.JudgeVerdict{}, true
	}
	idx := 0
	for _, c := range calls {
		if idx < len(want) && internalName(c) == want[idx] {
			idx++
		}
	}
	if idx == len(want) {
		return eval.JudgeVerdict{}, true
	}
	// Report the whole unmatched tail, not just want[idx], so a multi-step
	// gap does not read as a single missing call.
	return eval.JudgeVerdict{
		Passed: false,
		Reason: fmt.Sprintf(
			"expected tool-call sequence %s; matched %d of %d (missing or out of order: %s)",
			strings.Join(want, " -> "), idx, len(want), strings.Join(want[idx:], ", "),
		),
	}, false
}

// checkExpectation verifies a single per-tool count / success expectation.
func checkExpectation(exp types.ToolCallExpectation, calls []types.ToolCallSummary) (eval.JudgeVerdict, bool) {
	matched := 0
	failed := 0
	for _, c := range calls {
		if internalName(c) != exp.Name {
			continue
		}
		matched++
		if !c.Success {
			failed++
		}
	}

	if matched < exp.MinCalls {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("tool %q called %d time(s), expected at least %d", exp.Name, matched, exp.MinCalls),
		}, false
	}
	if exp.MaxCalls != nil && matched > *exp.MaxCalls {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("tool %q called %d time(s), expected at most %d", exp.Name, matched, *exp.MaxCalls),
		}, false
	}
	// Fail closed: without this, all_succeeded is vacuously true when the
	// tool was never called, silently passing an expectation meant to gate
	// on success (CWE-754).
	if exp.AllSucceeded && matched == 0 && exp.MinCalls == 0 {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("tool %q: all_succeeded set but no calls matched; pair with min_calls >= 1 to assert the tool was invoked", exp.Name),
		}, false
	}
	if exp.AllSucceeded && failed > 0 {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("tool %q had %d failed call(s); expected all to succeed", exp.Name, failed),
		}, false
	}
	return eval.JudgeVerdict{}, true
}

// checkNoUnresolvedUnknown fails when the run recorded a failed call not
// followed *later in the trace* by a successful call to the same tool.
// Recovery is positional and forward-only: `[fail, success]` is acceptable
// recovery, `[success, fail]` is a trailing failure and must FAIL. A
// set-membership "ever succeeded" test would wrongly pass the latter.
func checkNoUnresolvedUnknown(calls []types.ToolCallSummary) (eval.JudgeVerdict, bool) {
	// Fail closed on an empty trace: a run with no tool calls cannot
	// demonstrate recovery, and a vacuous pass would make this check
	// useless as a gate (CWE-754).
	if len(calls) == 0 {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: "forbid_unknown: no tool calls recorded; cannot assert unknown-tool recovery",
		}, false
	}
	for i, c := range calls {
		if c.Success {
			continue
		}
		resolved := false
		for _, later := range calls[i+1:] {
			if later.Success && internalName(later) == internalName(c) {
				resolved = true
				break
			}
		}
		if !resolved {
			return eval.JudgeVerdict{
				Passed: false,
				Reason: fmt.Sprintf("tool %q failed and was never followed by a successful call (unresolved unknown-tool miss)", internalName(c)),
			}, false
		}
	}
	return eval.JudgeVerdict{}, true
}
