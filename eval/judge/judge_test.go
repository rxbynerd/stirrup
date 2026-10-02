package judge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func TestTestCommand_Pass(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "echo ok"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestTestCommand_Fail(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "exit 1"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail, got pass")
	}
}

func TestTestCommand_Timeout(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	j := types.EvalJudge{Type: "test-command", Command: "sleep 60"}
	v, err := Evaluate(ctx, j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail on timeout, got pass")
	}
}

func TestTestCommand_EmptyCommand(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestFileExists_AllExist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "b.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestFileExists_SomeMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "missing.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for missing file")
	}
	if v.Reason != "missing paths: missing.txt" {
		t.Fatalf("unexpected reason: %s", v.Reason)
	}
}

func TestFileExists_EmptyPaths(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass for empty paths, got fail: %s", v.Reason)
	}
}

func TestFileContains_Match(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world 123")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `world \d+`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
}

func TestFileContains_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `goodbye`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for non-matching pattern")
	}
}

func TestFileContains_FileNotExist(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "nope.txt", Pattern: `anything`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail for missing file")
	}
}

func TestComposite_AllPass(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "content")

	j := types.EvalJudge{
		Type:    "composite",
		Require: "all",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "file-exists", Paths: []string{"a.txt"}},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass, got fail: %s", v.Reason)
	}
	if len(v.Details) != 2 {
		t.Fatalf("expected 2 details, got %d", len(v.Details))
	}
}

func TestComposite_AllRequiredOneFails(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "all",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "test-command", Command: "false"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when one sub-judge fails with require=all")
	}
}

func TestComposite_AnyOnePasses(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "any",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "false"},
			{Type: "test-command", Command: "true"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Passed {
		t.Fatalf("expected pass with require=any, got fail: %s", v.Reason)
	}
}

func TestComposite_AnyAllFail(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type:    "composite",
		Require: "any",
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "false"},
			{Type: "test-command", Command: "exit 1"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail when all sub-judges fail with require=any")
	}
}

func TestComposite_DefaultRequireAll(t *testing.T) {
	dir := t.TempDir()

	j := types.EvalJudge{
		Type: "composite",
		// Require omitted, should default to "all"
		Judges: []types.EvalJudge{
			{Type: "test-command", Command: "true"},
			{Type: "test-command", Command: "false"},
		},
	}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("expected fail with default require=all and one failing sub-judge")
	}
}

// markerJudge is a test-command judge that leaves name in the workspace to
// prove it was evaluated, then exits with code.
func markerJudge(name string, code int) types.EvalJudge {
	return types.EvalJudge{Type: "test-command", Command: fmt.Sprintf("touch %s; exit %d", name, code)}
}

func ranMarker(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", name, err)
	}
	return err == nil
}

func composite(require string, judges ...types.EvalJudge) types.EvalJudge {
	return types.EvalJudge{Type: "composite", Require: require, Judges: judges}
}

func detailStatuses(v eval.JudgeVerdict) []string {
	statuses := make([]string, len(v.Details))
	for i, d := range v.Details {
		statuses[i] = d.Status
	}
	return statuses
}

func assertStatuses(t *testing.T, v eval.JudgeVerdict, want ...string) {
	t.Helper()
	got := detailStatuses(v)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("detail statuses = %v, want %v", got, want)
	}
	for i, d := range v.Details {
		if d.Passed != (d.Status == types.JudgeStatusPass) {
			t.Errorf("details[%d]: Passed = %v with status %q", i, d.Passed, d.Status)
		}
	}
}

func evaluateOK(t *testing.T, j types.EvalJudge, jctx JudgeContext) eval.JudgeVerdict {
	t.Helper()
	v, err := Evaluate(context.Background(), j, jctx)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	if v.Passed != (v.Status == types.JudgeStatusPass) {
		t.Errorf("Passed = %v with status %q", v.Passed, v.Status)
	}
	return v
}

func TestComposite_AllStopsAtFirstFail(t *testing.T) {
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), markerJudge("b", 1), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusFail || v.Passed {
		t.Fatalf("verdict = %+v, want fail", v)
	}
	if !ranMarker(t, dir, "a") || !ranMarker(t, dir, "b") {
		t.Error("sub-judges up to the failing one must run")
	}
	if ranMarker(t, dir, "c") {
		t.Error("a sub-judge after the failing one must not run")
	}
	assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusFail, eval.JudgeStatusSkipped)
	if want := "sub-judge 2 of 3 (test-command) failed (require all); 1 skipped"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
	if !strings.Contains(v.Details[2].Reason, "sub-judge 2 of 3") {
		t.Errorf("skipped Reason = %q, should name the deciding sub-judge", v.Details[2].Reason)
	}
}

func TestComposite_AllFailSkipsLLMSubJudge(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	j := composite("all", types.EvalJudge{Type: "file-exists", Paths: []string{"absent.txt"}}, diffReviewJudge())

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	if v.Status != types.JudgeStatusFail {
		t.Fatalf("verdict = %+v, want fail", v)
	}
	if fake.calls != 0 {
		t.Errorf("model was called %d times after a deterministic sub-judge had failed", fake.calls)
	}
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped)
	if d := v.Details[1]; d.Type != "diff-review" || d.Record != nil {
		t.Errorf("skipped detail = %+v, want a diff-review entry without a Record", d)
	}
}

func TestComposite_AnyStopsAtFirstPass(t *testing.T) {
	dir := t.TempDir()
	j := composite("any", markerJudge("a", 1), markerJudge("b", 0), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusPass || !v.Passed {
		t.Fatalf("verdict = %+v, want pass", v)
	}
	if ranMarker(t, dir, "c") {
		t.Error("a sub-judge after the passing one must not run")
	}
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusPass, eval.JudgeStatusSkipped)
	if want := "sub-judge 2 of 3 (test-command) passed (require any); 1 skipped"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_AnyContinuesPastFails(t *testing.T) {
	dir := t.TempDir()
	j := composite("any", markerJudge("a", 1), markerJudge("b", 1), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusPass {
		t.Fatalf("verdict = %+v, want pass", v)
	}
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusFail, types.JudgeStatusPass)
	if strings.Contains(v.Reason, "skipped") {
		t.Errorf("Reason = %q, nothing was skipped", v.Reason)
	}
}

func TestComposite_AllErrorIsDecisive(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cases := []struct {
		name     string
		erroring types.EvalJudge
		client   *fakeClient
		wantType string
		wantMsg  string
	}{
		{
			name:     "sub-judge returns an error",
			erroring: types.EvalJudge{Type: "test-command"},
			wantType: "test-command",
			wantMsg:  "requires a command",
		},
		{
			name:     "sub-judge returns an error verdict",
			erroring: diffReviewJudge(),
			client:   &fakeClient{err: errors.New("provider returned HTTP 503: overloaded")},
			wantType: "diff-review",
			wantMsg:  "overloaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := changedWorkspace(t)
			jctx := JudgeContext{WorkspaceDir: dir, Baseline: &base}
			if tc.client != nil {
				jctx.ClientFactory = tc.client.factory(nil, nil)
			}
			j := composite("all", markerJudge("a", 0), tc.erroring, markerJudge("c", 0))

			v := evaluateOK(t, j, jctx)

			if v.Status != types.JudgeStatusError || v.Passed {
				t.Fatalf("verdict = %+v, want error", v)
			}
			if ranMarker(t, dir, "c") {
				t.Error("a sub-judge after the erroring one must not run")
			}
			assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusError, eval.JudgeStatusSkipped)
			wantPrefix := fmt.Sprintf("sub-judge 2 of 3 (%s) errored (require all); 1 skipped: ", tc.wantType)
			if !strings.HasPrefix(v.Reason, wantPrefix) || !strings.Contains(v.Reason, tc.wantMsg) {
				t.Errorf("Reason = %q, want prefix %q and cause %q", v.Reason, wantPrefix, tc.wantMsg)
			}
		})
	}
}

func TestComposite_AnyErrorThenPassPasses(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
	dir, base := changedWorkspace(t)
	j := composite("any", diffReviewJudge(), markerJudge("b", 0), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{ClientFactory: fake.factory(nil, nil)}})

	if v.Status != types.JudgeStatusPass || !v.Passed {
		t.Fatalf("verdict = %+v, want pass", v)
	}
	if ranMarker(t, dir, "c") {
		t.Error("a sub-judge after the passing one must not run")
	}
	assertStatuses(t, v, types.JudgeStatusError, types.JudgeStatusPass, eval.JudgeStatusSkipped)
}

func TestComposite_AnyErrorWithoutPassIsError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
	j := composite("any", diffReviewJudge(), markerJudge("b", 1))

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	if v.Status != types.JudgeStatusError || v.Passed {
		t.Fatalf("verdict = %+v, want error", v)
	}
	assertStatuses(t, v, types.JudgeStatusError, types.JudgeStatusFail)
	wantPrefix := "0 of 2 sub-judges passed (require any); 1 errored, first: sub-judge 1 of 2 (diff-review): "
	if !strings.HasPrefix(v.Reason, wantPrefix) || !strings.Contains(v.Reason, "HTTP 503") {
		t.Errorf("Reason = %q, want prefix %q and the cause", v.Reason, wantPrefix)
	}
}

func TestComposite_AnyAllFailIsFailNotError(t *testing.T) {
	v := evaluateOK(t, composite("any", markerJudge("a", 1), markerJudge("b", 2)), JudgeContext{WorkspaceDir: t.TempDir()})

	if v.Status != types.JudgeStatusFail || v.Passed {
		t.Fatalf("verdict = %+v, want fail", v)
	}
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusFail)
	if want := "0 of 2 sub-judges passed (require any)"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_AllPassReportsStatus(t *testing.T) {
	v := evaluateOK(t, composite("all", markerJudge("a", 0), markerJudge("b", 0)), JudgeContext{WorkspaceDir: t.TempDir()})

	if v.Status != types.JudgeStatusPass {
		t.Fatalf("verdict = %+v, want pass", v)
	}
	assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusPass)
	if want := "all 2 sub-judges passed"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_NestedStatusPropagates(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")

	t.Run("nested error is decisive for an outer all", func(t *testing.T) {
		fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
		dir, base := changedWorkspace(t)
		inner := composite("any", diffReviewJudge(), markerJudge("inner-fail", 1))
		j := composite("all", inner, markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{ClientFactory: fake.factory(nil, nil)}})

		if v.Status != types.JudgeStatusError {
			t.Fatalf("verdict = %+v, want error", v)
		}
		if ranMarker(t, dir, "after") {
			t.Error("a sub-judge after the erroring composite must not run")
		}
		assertStatuses(t, v, types.JudgeStatusError, eval.JudgeStatusSkipped)
		if v.Details[0].Type != "composite" || !strings.Contains(v.Details[0].Reason, "1 errored") {
			t.Errorf("details[0] = %+v, want the nested composite's reason", v.Details[0])
		}
		if !strings.HasPrefix(v.Reason, "sub-judge 1 of 2 (composite) errored (require all); 1 skipped: ") {
			t.Errorf("Reason = %q", v.Reason)
		}
	})

	t.Run("nested pass lets an outer all continue", func(t *testing.T) {
		dir := t.TempDir()
		inner := composite("any", markerJudge("inner-fail", 1), markerJudge("inner-pass", 0), markerJudge("inner-skipped", 0))
		j := composite("all", inner, markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

		if v.Status != types.JudgeStatusPass {
			t.Fatalf("verdict = %+v, want pass", v)
		}
		assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusPass)
		if !ranMarker(t, dir, "after") || ranMarker(t, dir, "inner-skipped") {
			t.Error("inner short-circuit must skip only the inner remainder")
		}
	})

	t.Run("nested fail is decisive for an outer all", func(t *testing.T) {
		dir := t.TempDir()
		j := composite("all", composite("all", markerJudge("inner-fail", 1)), markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

		if v.Status != types.JudgeStatusFail {
			t.Fatalf("verdict = %+v, want fail", v)
		}
		assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped)
		if ranMarker(t, dir, "after") {
			t.Error("a sub-judge after the failing composite must not run")
		}
	})
}

func TestComposite_DetailsKeepDeclaredOrder(t *testing.T) {
	j := composite("all",
		types.EvalJudge{Type: "file-exists", Paths: []string{"absent.txt"}},
		types.EvalJudge{Type: "file-contains", Path: "a.txt", Pattern: "x"},
		markerJudge("never", 0),
		composite("any", markerJudge("never-either", 0)),
	)

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: t.TempDir()})

	var gotTypes []string
	for _, d := range v.Details {
		gotTypes = append(gotTypes, d.Type)
	}
	if want := "file-exists,file-contains,test-command,composite"; strings.Join(gotTypes, ",") != want {
		t.Errorf("detail types = %v, want %s", gotTypes, want)
	}
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped, eval.JudgeStatusSkipped, eval.JudgeStatusSkipped)
	if want := "sub-judge 1 of 4 (file-exists) failed (require all); 3 skipped"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_DetailsCarryRecords(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cases := []struct {
		name       string
		client     *fakeClient
		wantStatus string
		wantParse  string
	}{
		{"pass", &fakeClient{resp: okResponse(verdictPass)}, types.JudgeStatusPass, types.JudgeParseOK},
		{"fail", &fakeClient{resp: okResponse(verdictFail)}, types.JudgeStatusFail, types.JudgeParseOK},
		{"error", &fakeClient{resp: okResponse("I think it is fine.")}, types.JudgeStatusError, types.JudgeParseNoJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := composite("any", markerJudge("a", 1), diffReviewJudge())

			v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: tc.client.factory(nil, nil)}))

			if v.Record != nil {
				t.Errorf("composite verdict Record = %+v, want nil", v.Record)
			}
			if v.Details[0].Record != nil {
				t.Errorf("deterministic sub-judge Record = %+v, want nil", v.Details[0].Record)
			}
			d := v.Details[1]
			if d.Status != tc.wantStatus {
				t.Errorf("details[1].Status = %q, want %q", d.Status, tc.wantStatus)
			}
			if d.Record == nil {
				t.Fatal("diff-review detail lost its Record")
			}
			if d.Record.Kind != types.JudgeKindDiffReview || d.Record.ParseStatus != tc.wantParse || d.Record.ServedModel != "served-model-1" {
				t.Errorf("details[1].Record = %+v", d.Record)
			}
		})
	}
}

func TestComposite_ConfigurationErrorsAreHardErrors(t *testing.T) {
	cases := []struct {
		name    string
		judge   types.EvalJudge
		wantMsg string
	}{
		{"empty judge list", composite("all"), "at least one sub-judge"},
		{"invalid require", composite("most", markerJudge("a", 0)), "invalid require"},
		{"unknown sub-judge type after a failing sub-judge", composite("all", markerJudge("a", 1), types.EvalJudge{Type: "nonexistent"}), "unknown judge type"},
		{"nested invalid require", composite("any", markerJudge("a", 0), composite("most", markerJudge("b", 0))), "invalid require"},
		{"nested empty judge list", composite("any", markerJudge("a", 0), composite("any")), "at least one sub-judge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			_, err := Evaluate(context.Background(), tc.judge, JudgeContext{WorkspaceDir: dir})

			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantMsg)
			}
			if ranMarker(t, dir, "a") {
				t.Error("no sub-judge may run when the tree is invalid")
			}
		})
	}
}

// TestDiffReview_RequiresCriteria pins that the diff-review judge fails
// fast when criteria is empty, without making an API call.
func TestDiffReview_RequiresCriteria(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "diff-review"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error when criteria is empty")
	}
	if !strings.Contains(err.Error(), "criteria") {
		t.Errorf("error should mention criteria: %v", err)
	}
}

// TestDiffReview_RequiresAPIKey pins that with criteria set but
// ANTHROPIC_API_KEY unset, the judge returns a clear error.
func TestDiffReview_RequiresAPIKey(t *testing.T) {
	// git init the workspace so captureDiff succeeds before the api-key
	// check fires, pinning the api-key error rather than a repo error.
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Skipf("git init unavailable: %v", err)
	}
	_ = exec.Command("git", "-C", dir, "config", "user.email", "ci@example.test").Run()
	_ = exec.Command("git", "-C", dir, "config", "user.name", "ci").Run()
	if err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "init", "--quiet").Run(); err != nil {
		t.Skipf("git commit unavailable: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	j := types.EvalJudge{Type: "diff-review", Criteria: "be good"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error when ANTHROPIC_API_KEY is unset")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should mention the env var: %v", err)
	}
}

// TestEvaluate_StatusMatchesPassed pins that every judge type reports a
// Status consistent with Passed, so consumers can rely on Status alone.
func TestEvaluate_StatusMatchesPassed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		judge      types.EvalJudge
		wantPassed bool
	}{
		{"test-command pass", types.EvalJudge{Type: "test-command", Command: "true"}, true},
		{"test-command fail", types.EvalJudge{Type: "test-command", Command: "false"}, false},
		{"file-exists pass", types.EvalJudge{Type: "file-exists", Paths: []string{"present.txt"}}, true},
		{"file-exists fail", types.EvalJudge{Type: "file-exists", Paths: []string{"absent.txt"}}, false},
		{"composite pass", types.EvalJudge{Type: "composite", Judges: []types.EvalJudge{{Type: "file-exists", Paths: []string{"present.txt"}}}}, true},
		{"composite fail", types.EvalJudge{Type: "composite", Judges: []types.EvalJudge{{Type: "file-exists", Paths: []string{"absent.txt"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Evaluate(context.Background(), tc.judge, JudgeContext{WorkspaceDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			want := types.JudgeStatusFail
			if tc.wantPassed {
				want = types.JudgeStatusPass
			}
			if v.Passed != tc.wantPassed || v.Status != want {
				t.Errorf("Passed = %v, Status = %q; want %v, %q", v.Passed, v.Status, tc.wantPassed, want)
			}
		})
	}
}

func TestPathTraversal_FileExists(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{"../../../etc/passwd"}}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestPathTraversal_FileContains(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "../../../etc/passwd", Pattern: "root"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestUnknownJudgeType(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "nonexistent"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	if err == nil {
		t.Fatal("expected error for unknown judge type")
	}
}

func TestContainsType(t *testing.T) {
	nested := types.EvalJudge{Type: "composite", Judges: []types.EvalJudge{
		{Type: "file-exists"},
		{Type: "composite", Judges: []types.EvalJudge{{Type: "diff-review"}}},
	}}
	if !ContainsType(nested, "diff-review") {
		t.Error("nested diff-review not found")
	}
	if ContainsType(nested, "test-command") {
		t.Error("absent type reported present")
	}
	if !ContainsType(types.EvalJudge{Type: "diff-review"}, "diff-review") {
		t.Error("top-level diff-review not found")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating parent dirs: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
