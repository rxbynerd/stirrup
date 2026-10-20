package judge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// traceFromCalls builds a RunTrace from a list of tool calls. Names are
// treated as internal tool IDs (InternalName left empty, the default
// profile case).
func traceFromCalls(calls ...types.ToolCallSummary) *types.RunTrace {
	return &types.RunTrace{ToolCalls: calls}
}

func call(name string, success bool) types.ToolCallSummary {
	return types.ToolCallSummary{Name: name, Success: success}
}

func TestToolTrace_RequiresTrace(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file"},
	}}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: t.TempDir()})
	must.Error(err)
}

func TestToolTrace_RequiresCriteria(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace"}
	_, err := Evaluate(context.Background(), j, JudgeContext{Trace: traceFromCalls()})
	must.Error(err)
}

func TestToolTrace_Sequence_PassesInOrder(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file", "edit_file"},
	}}
	tr := traceFromCalls(
		call("read_file", true),
		call("grep_files", true),
		call("edit_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected pass, got fail: %s", v.Reason)
}

func TestToolTrace_Sequence_FailsOnOrderViolation(t *testing.T) {
	must := require.New(t)
	// edit_file before read_file violates read-before-edit.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file", "edit_file"},
	}}
	tr := traceFromCalls(
		call("edit_file", true),
		call("read_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.False(v.Passed, "expected fail when edit precedes read")
}

func TestToolTrace_Sequence_MatchesInternalName(t *testing.T) {
	must := require.New(t)
	// Under a profile the model-facing Name is an alias; the assertion is
	// written against the internal ID, which lives in InternalName.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file", "edit_file"},
	}}
	tr := traceFromCalls(
		types.ToolCallSummary{Name: "view", InternalName: "read_file", Success: true},
		types.ToolCallSummary{Name: "str_replace", InternalName: "edit_file", Success: true},
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "got fail: %s", v.Reason)
}

func TestToolTrace_Calls_FailsBelowMinCalls(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 2}},
	}}
	tr := traceFromCalls(call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.False(v.Passed, "expected fail when min_calls not met")
}

func TestToolTrace_Calls_MaxZeroForbidsTool(t *testing.T) {
	// A no-tool answer task: write_file must not be called (max 0).
	zero := 0
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "write_file", MaxCalls: &zero}},
	}}

	t.Run("passes when tool absent", func(t *testing.T) {
		must := require.New(t)
		tr := traceFromCalls(call("read_file", true))
		v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
		must.NoError(err)
		must.True(v.Passed, "got fail: %s", v.Reason)
	})

	t.Run("fails when tool present", func(t *testing.T) {
		must := require.New(t)
		tr := traceFromCalls(call("write_file", true))
		v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
		must.NoError(err)
		must.False(v.Passed, "expected fail when write_file present with max 0")
	})
}

func TestToolTrace_Calls_FailsWhenAnyCallErrors(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 1, AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("edit_file", false), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.False(v.Passed, "expected fail when an edit_file call errored")
}

func TestToolTrace_ForbidUnknown_RecoveryRules(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}

	t.Run("fails when failed tool never succeeds", func(t *testing.T) {
		must := require.New(t)
		// A later success of a different tool does not resolve a failed
		// search_files, which never succeeds.
		tr := traceFromCalls(
			call("search_files", false),
			call("grep_files", true),
		)
		v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
		must.NoError(err)
		must.False(v.Passed, "expected fail: search_files failed and never succeeded")
	})

	t.Run("passes when failed tool is retried successfully", func(t *testing.T) {
		must := require.New(t)
		tr := traceFromCalls(
			call("edit_file", false),
			call("edit_file", true),
		)
		v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
		must.NoError(err)
		must.True(v.Passed, "edit_file recovered, got fail: %s", v.Reason)
	})
}

func TestToolTrace_ForbidUnknown_TrailingFailureFails(t *testing.T) {
	must := require.New(t)
	// A success earlier in the trace must not resolve a later failure of the
	// same tool.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	tr := traceFromCalls(
		call("edit_file", true),
		call("edit_file", false),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.False(v.Passed, "expected fail: trailing edit_file failure was never followed by a success")
}

func TestToolTrace_ForbidUnknown_FailThenSuccessPasses(t *testing.T) {
	must := require.New(t)
	// A failure followed by a later success of the same tool is resolved.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	tr := traceFromCalls(
		call("edit_file", false),
		call("edit_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected pass: edit_file failure recovered by a later success, got fail: %s", v.Reason)
}

func TestToolTrace_ForbidUnknown_EmptyTraceFails(t *testing.T) {
	must := require.New(t)
	// A zero-call trace must not pass forbid_unknown vacuously.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: traceFromCalls()})
	must.NoError(err)
	must.False(v.Passed, "expected fail: forbid_unknown against an empty trace must not pass vacuously")
}

func TestToolTrace_Calls_AllSucceededWithNoCallFails(t *testing.T) {
	must := require.New(t)
	// A tool absent from the trace with all_succeeded set and no min_calls
	// must not pass vacuously.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("read_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.False(v.Passed, "expected fail: all_succeeded with no matching call and no min_calls must not pass vacuously")
}

func TestToolTrace_Calls_AllSucceededPasses(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 1, AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("edit_file", true), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected pass when all edit_file calls succeeded, got fail: %s", v.Reason)
}

func TestToolTrace_Calls_MinCallsMetPasses(t *testing.T) {
	must := require.New(t)
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 2}},
	}}
	tr := traceFromCalls(call("edit_file", true), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected pass when min_calls met, got fail: %s", v.Reason)
}

// TestToolTrace_Calls_WithinMaxPasses covers a non-zero upper bound being respected.
func TestToolTrace_Calls_WithinMaxPasses(t *testing.T) {
	must := require.New(t)
	two := 2
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MaxCalls: &two}},
	}}
	tr := traceFromCalls(call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected pass when calls within max, got fail: %s", v.Reason)
}

func TestToolTrace_Composite_SubJudgeReceivesTrace(t *testing.T) {
	must := require.New(t)
	// The composite path threads JudgeContext (including Trace) to every
	// sub-judge, so a tool-trace sub-judge sees the trace.
	dir := t.TempDir()
	writeFile(t, dir, "out.txt", "done")
	j := types.EvalJudge{
		Type:    "composite",
		Require: "all",
		Judges: []types.EvalJudge{
			{Type: "file-contains", Path: "out.txt", Pattern: "done"},
			{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
				Sequence: []string{"read_file", "edit_file"},
			}},
		},
	}
	tr := traceFromCalls(call("read_file", true), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir, Trace: tr})
	must.NoError(err)
	must.True(v.Passed, "expected composite pass, got fail: %s", v.Reason)
}
