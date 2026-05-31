package judge

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// evaluateToolTrace inspects the run's RunTrace.ToolCalls rather than the
// workspace filesystem (issue #233). It is the trace-side counterpart to
// the file-state judges: where file-exists / file-contains confirm the
// agent reached the right end state, tool-trace confirms it got there by
// the expected tool-use path.
//
// A nil JudgeContext.Trace is a hard error rather than a silent pass: a
// tool-trace judge that cannot see the trace has nothing to assert, and
// treating that as a pass would mask a misconfigured runner (e.g. a suite
// that forgot to retain the trace, or a replay path that did not thread it
// through). The runner always parses the trace before judging, so a nil
// here means the wiring is broken.
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
// Under a toolset profile (issue #234) the model-facing Name is an alias and
// InternalName carries the resolved ID; under the default profile
// InternalName is empty and Name is already canonical. Matching on the
// internal ID means a trace assertion written against the canonical name
// holds regardless of the active profile.
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
	// Everything from idx onward failed to match in order: idx is the first
	// element with no in-order occurrence, and the greedy scan above could
	// not advance past it, so the whole tail is unsatisfied. Naming only
	// want[idx] hides the rest and makes a multi-step gap look like a
	// single missing call.
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
	// Fail closed when all_succeeded is asserted but no call matched and no
	// lower bound forces one to exist. Without this guard the predicate is
	// vacuously true (zero failures among zero matches), so a run where the
	// model never called the tool would silently pass an expectation that
	// was meant to gate on the tool succeeding (CWE-754). Authors who want
	// to allow zero calls should not set all_succeeded; authors who want to
	// require the tool set min_calls >= 1, which this branch points them to.
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

// checkNoUnresolvedUnknown fails when the run recorded a failed call that was
// not followed *later in the trace* by a successful call to the same tool —
// an unresolved unknown-tool / renamed-tool miss. Recovery is positional: a
// failure is resolved only by a success that occurs strictly after it, so
// `[edit_file:fail, edit_file:success]` is acceptable recovery while
// `[edit_file:success, edit_file:fail]` is a trailing failure that nothing
// recovered and must FAIL. A set-membership test over "ever succeeded" would
// wrongly pass the latter — masking exactly the recovery regressions this
// check exists to catch — so the scan is forward-only.
func checkNoUnresolvedUnknown(calls []types.ToolCallSummary) (eval.JudgeVerdict, bool) {
	// Fail closed on an empty trace: forbid_unknown is meant to gate on the
	// recovery path having run, and a run with no tool calls (harness crash,
	// early exit, no model output) cannot demonstrate recovery. A vacuous
	// pass here would make the judge useless as a gate (CWE-754).
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
