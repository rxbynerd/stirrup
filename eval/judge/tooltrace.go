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
	return eval.JudgeVerdict{
		Passed: false,
		Reason: fmt.Sprintf(
			"expected tool-call sequence %s; matched %d of %d (missing %q or out of order)",
			strings.Join(want, " -> "), idx, len(want), want[idx],
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
	if exp.AllSucceeded && failed > 0 {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("tool %q had %d failed call(s); expected all to succeed", exp.Name, failed),
		}, false
	}
	return eval.JudgeVerdict{}, true
}

// checkNoUnresolvedUnknown fails when the run recorded an unknown-tool /
// renamed-tool miss (a failed call whose name never resolved) that was not
// followed by at least one successful call. The heuristic: any failed call
// whose name is never the name of a *successful* call later in the trace is
// treated as an unresolved unknown-tool miss. This is what asserts in-loop
// recovery from a renamed-tool hint actually produced a working call.
func checkNoUnresolvedUnknown(calls []types.ToolCallSummary) (eval.JudgeVerdict, bool) {
	succeeded := make(map[string]struct{}, len(calls))
	for _, c := range calls {
		if c.Success {
			succeeded[internalName(c)] = struct{}{}
		}
	}
	for _, c := range calls {
		if c.Success {
			continue
		}
		// A failed call that was eventually retried successfully under the
		// same name is acceptable recovery. A failed call whose name never
		// succeeds is an unresolved miss.
		if _, ok := succeeded[internalName(c)]; !ok {
			return eval.JudgeVerdict{
				Passed: false,
				Reason: fmt.Sprintf("tool %q failed and was never followed by a successful call (unresolved unknown-tool miss)", internalName(c)),
			}, false
		}
	}
	return eval.JudgeVerdict{}, true
}
