package judge

import (
	"context"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// trace builds a RunTrace from a list of (name, success) tool calls. Names
// are treated as internal tool IDs (InternalName left empty, the default
// profile case).
func traceFromCalls(calls ...types.ToolCallSummary) *types.RunTrace {
	return &types.RunTrace{ToolCalls: calls}
}

func call(name string, success bool) types.ToolCallSummary {
	return types.ToolCallSummary{Name: name, Success: success}
}

func TestToolTrace_RequiresTrace(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file"},
	}}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error when trace is nil, got none")
	}
}

func TestToolTrace_RequiresCriteria(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace"}
	_, err := Evaluate(context.Background(), j, JudgeContext{Trace: traceFromCalls()})
	if err == nil {
		t.Fatal("expected error when toolTrace block is nil, got none")
	}
}

func TestToolTrace_SequencePass(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file", "edit_file"},
	}}
	tr := traceFromCalls(
		call("read_file", true),
		call("grep_files", true),
		call("edit_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestToolTrace_SequenceOrderViolation(t *testing.T) {
	// edit_file before read_file violates read-before-edit.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Sequence: []string{"read_file", "edit_file"},
	}}
	tr := traceFromCalls(
		call("edit_file", true),
		call("read_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when edit precedes read, got pass")
	}
}

func TestToolTrace_SequenceMatchesInternalNameUnderProfile(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass matching internal names, got fail: %s", v.Reason)
	}
}

func TestToolTrace_CallMinCalls(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 2}},
	}}
	tr := traceFromCalls(call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when min_calls not met, got pass")
	}
}

func TestToolTrace_CallMaxCallsForbid(t *testing.T) {
	// A no-tool answer task: assert read_file was NOT called (max 0).
	zero := 0
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "write_file", MaxCalls: &zero}},
	}}
	pass := traceFromCalls(call("read_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: pass})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass when write_file absent, got fail: %s", v.Reason)
	}

	fail := traceFromCalls(call("write_file", true))
	v, err = Evaluate(context.Background(), j, JudgeContext{Trace: fail})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when write_file present with max 0, got pass")
	}
}

func TestToolTrace_AllSucceeded(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 1, AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("edit_file", false), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when an edit_file call errored, got pass")
	}
}

func TestToolTrace_ForbidUnknownRecovers(t *testing.T) {
	// A failed search_files (renamed-tool miss) followed by a successful
	// grep_files is acceptable recovery only if the failed name itself
	// eventually succeeds; search_files never does, so ForbidUnknown fails.
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	tr := traceFromCalls(
		call("search_files", false),
		call("grep_files", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail: search_files failed and never succeeded")
	}

	// A failed edit_file retried successfully is acceptable recovery.
	ok := traceFromCalls(
		call("edit_file", false),
		call("edit_file", true),
	)
	v, err = Evaluate(context.Background(), j, JudgeContext{Trace: ok})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass: edit_file recovered, got fail: %s", v.Reason)
	}
}

// TestToolTrace_ForbidUnknownTrailingFailureFails pins that a success
// EARLIER in the trace must not resolve a LATER failure of the same tool.
func TestToolTrace_ForbidUnknownTrailingFailureFails(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	tr := traceFromCalls(
		call("edit_file", true),
		call("edit_file", false),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail: trailing edit_file failure was never followed by a success")
	}
}

// TestToolTrace_ForbidUnknownFailThenSuccessPasses is the recovery pair to the
// trailing-failure case: a failure followed by a later success is resolved.
func TestToolTrace_ForbidUnknownFailThenSuccessPasses(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	tr := traceFromCalls(
		call("edit_file", false),
		call("edit_file", true),
	)
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass: edit_file failure recovered by a later success, got fail: %s", v.Reason)
	}
}

// TestToolTrace_ForbidUnknownEmptyTraceFails pins that forbid_unknown
// against a zero-call trace must FAIL rather than pass vacuously.
func TestToolTrace_ForbidUnknownEmptyTraceFails(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		ForbidUnknown: true,
	}}
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: traceFromCalls()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail: forbid_unknown against an empty trace must not pass vacuously")
	}
}

// TestToolTrace_AllSucceededZeroCallsFails pins that a tool absent from the
// trace with all_succeeded set and no min_calls must FAIL, not pass vacuously.
func TestToolTrace_AllSucceededZeroCallsFails(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("read_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail: all_succeeded with no matching call and no min_calls must not pass vacuously")
	}
}

// TestToolTrace_AllSucceeded_Pass: two successful calls with all_succeeded pass.
func TestToolTrace_AllSucceeded_Pass(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 1, AllSucceeded: true}},
	}}
	tr := traceFromCalls(call("edit_file", true), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass when all edit_file calls succeeded, got fail: %s", v.Reason)
	}
}

func TestToolTrace_CallMinCalls_Pass(t *testing.T) {
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MinCalls: 2}},
	}}
	tr := traceFromCalls(call("edit_file", true), call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass when min_calls met, got fail: %s", v.Reason)
	}
}

// TestToolTrace_CallMaxCalls_Pass covers a non-zero upper bound being respected.
func TestToolTrace_CallMaxCalls_Pass(t *testing.T) {
	two := 2
	j := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{
		Calls: []types.ToolCallExpectation{{Name: "edit_file", MaxCalls: &two}},
	}}
	tr := traceFromCalls(call("edit_file", true))
	v, err := Evaluate(context.Background(), j, JudgeContext{Trace: tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass when calls within max, got fail: %s", v.Reason)
	}
}

func TestToolTrace_CompositeWithFileJudge(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected composite pass, got fail: %s", v.Reason)
	}
}
