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

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func TestTestCommand_ZeroExitPasses(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "echo ok"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.True(t, v.Passed, "reason: %s", v.Reason)
}

func TestTestCommand_NonZeroExitFails(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command", Command: "exit 1"}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.False(t, v.Passed)
}

func TestTestCommand_TimeoutFails(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	j := types.EvalJudge{Type: "test-command", Command: "sleep 60"}
	v, err := Evaluate(ctx, j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.False(t, v.Passed, "a command that outlives its context must fail")
}

func TestTestCommand_EmptyCommandIsError(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "test-command"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.Error(t, err)
}

func TestFileExists_AllExist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "b.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.True(t, v.Passed, "reason: %s", v.Reason)
}

func TestFileExists_SomeMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")

	j := types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt", "missing.txt"}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.False(t, v.Passed)
	require.Equal(t, "missing paths: missing.txt", v.Reason)
}

func TestFileExists_EmptyPaths(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{}}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.True(t, v.Passed, "no paths to check, reason: %s", v.Reason)
}

func TestFileContains_MatchingPatternPasses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world 123")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `world \d+`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.True(t, v.Passed, "reason: %s", v.Reason)
}

func TestFileContains_NonMatchingPatternFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hello.txt", "hello world")

	j := types.EvalJudge{Type: "file-contains", Path: "hello.txt", Pattern: `goodbye`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.False(t, v.Passed)
}

func TestFileContains_MissingFileFails(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "nope.txt", Pattern: `anything`}
	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err)
	require.False(t, v.Passed)
}

func TestComposite_AllPassingSubJudgesPass(t *testing.T) {
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
	require.NoError(t, err)
	require.True(t, v.Passed, "reason: %s", v.Reason)
	require.Len(t, v.Details, 2)
}

func TestComposite_AllWithOneFailingSubJudgeFails(t *testing.T) {
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
	require.NoError(t, err)
	require.False(t, v.Passed, "one sub-judge fails with require=all")
}

func TestComposite_AnyWithOnePassingSubJudgePasses(t *testing.T) {
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
	require.NoError(t, err)
	require.True(t, v.Passed, "require=any with one passing sub-judge, reason: %s", v.Reason)
}

func TestComposite_AnyWithAllSubJudgesFailingFails(t *testing.T) {
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
	require.NoError(t, err)
	require.False(t, v.Passed, "all sub-judges fail with require=any")
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
	require.NoError(t, err)
	require.False(t, v.Passed, "default require=all with one failing sub-judge")
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
		require.NoError(t, err, "stat %s", name)
	}
	return err == nil
}

func composite(mode string, judges ...types.EvalJudge) types.EvalJudge {
	return types.EvalJudge{Type: "composite", Require: mode, Judges: judges}
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
	require.Equal(t, strings.Join(want, ","), strings.Join(got, ","), "detail statuses")
	for i, d := range v.Details {
		require.Equal(t, d.Status == types.JudgeStatusPass, d.Passed, "details[%d] with status %q", i, d.Status)
	}
}

func evaluateOK(t *testing.T, j types.EvalJudge, jctx JudgeContext) eval.JudgeVerdict {
	t.Helper()
	v, err := Evaluate(context.Background(), j, jctx)
	require.NoError(t, err, "Evaluate")
	require.Equal(t, v.Status == types.JudgeStatusPass, v.Passed, "status %q", v.Status)
	return v
}

func TestComposite_AllStopsAtFirstFail(t *testing.T) {
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), markerJudge("b", 1), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	require.Equal(t, types.JudgeStatusFail, v.Status, "verdict = %+v", v)
	require.False(t, v.Passed, "verdict = %+v", v)
	require.True(t, ranMarker(t, dir, "a"), "sub-judges up to the failing one must run")
	require.True(t, ranMarker(t, dir, "b"), "sub-judges up to the failing one must run")
	require.False(t, ranMarker(t, dir, "c"), "a sub-judge after the failing one must not run")
	assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusFail, eval.JudgeStatusSkipped)
	require.Equal(t, "sub-judge 2 of 3 (test-command) failed (require all); 1 skipped", v.Reason)
	require.Contains(t, v.Details[2].Reason, "sub-judge 2 of 3", "skipped detail should name the deciding sub-judge")
}

func TestComposite_AllFailSkipsLLMSubJudge(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	j := composite("all", types.EvalJudge{Type: "file-exists", Paths: []string{"absent.txt"}}, diffReviewJudge())

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	require.Equal(t, types.JudgeStatusFail, v.Status, "verdict = %+v", v)
	require.Zero(t, fake.calls, "model called after a deterministic sub-judge had failed")
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped)
	d := v.Details[1]
	require.Equal(t, "diff-review", d.Type, "skipped detail = %+v", d)
	require.Nil(t, d.Record, "skipped detail = %+v, want no Record", d)
}

func TestComposite_AnyStopsAtFirstPass(t *testing.T) {
	dir := t.TempDir()
	j := composite("any", markerJudge("a", 1), markerJudge("b", 0), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	require.True(t, v.Passed, "verdict = %+v", v)
	require.False(t, ranMarker(t, dir, "c"), "a sub-judge after the passing one must not run")
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusPass, eval.JudgeStatusSkipped)
	require.Equal(t, "sub-judge 2 of 3 (test-command) passed (require any); 1 skipped", v.Reason)
}

func TestComposite_AnyContinuesPastFails(t *testing.T) {
	dir := t.TempDir()
	j := composite("any", markerJudge("a", 1), markerJudge("b", 1), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusFail, types.JudgeStatusPass)
	require.NotContains(t, v.Reason, "skipped", "nothing was skipped")
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
			erroring: types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{}},
			wantType: "tool-trace",
			wantMsg:  "requires a run trace",
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

			require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
			require.False(t, v.Passed, "verdict = %+v", v)
			require.False(t, ranMarker(t, dir, "c"), "a sub-judge after the erroring one must not run")
			assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusError, eval.JudgeStatusSkipped)
			wantPrefix := fmt.Sprintf("sub-judge 2 of 3 (%s) errored (require all); 1 skipped: ", tc.wantType)
			require.True(t, strings.HasPrefix(v.Reason, wantPrefix), "Reason = %q, want prefix %q", v.Reason, wantPrefix)
			require.Contains(t, v.Reason, tc.wantMsg)
		})
	}
}

func TestComposite_AnyErrorThenPassPasses(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
	dir, base := changedWorkspace(t)
	j := composite("any", diffReviewJudge(), markerJudge("b", 0), markerJudge("c", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{ClientFactory: fake.factory(nil, nil)}})

	require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	require.True(t, v.Passed, "verdict = %+v", v)
	require.False(t, ranMarker(t, dir, "c"), "a sub-judge after the passing one must not run")
	assertStatuses(t, v, types.JudgeStatusError, types.JudgeStatusPass, eval.JudgeStatusSkipped)
}

func TestComposite_AnyErrorWithoutPassIsError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
	j := composite("any", diffReviewJudge(), markerJudge("b", 1))

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
	require.False(t, v.Passed, "verdict = %+v", v)
	assertStatuses(t, v, types.JudgeStatusError, types.JudgeStatusFail)
	wantPrefix := "0 of 2 sub-judges passed (require any); 1 errored, first: sub-judge 1 of 2 (diff-review): "
	require.True(t, strings.HasPrefix(v.Reason, wantPrefix), "Reason = %q, want prefix %q", v.Reason, wantPrefix)
	require.Contains(t, v.Reason, "HTTP 503")
}

func TestComposite_AnyReportsFirstOfSeveralErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
	noTrace := types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{}}
	j := composite("any", markerJudge("a", 1), noTrace, markerJudge("b", 1), diffReviewJudge())

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
	require.False(t, v.Passed, "verdict = %+v", v)
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusError, types.JudgeStatusFail, types.JudgeStatusError)
	want := "0 of 4 sub-judges passed (require any); 2 errored, first: sub-judge 2 of 4 (tool-trace): " +
		"tool-trace judge requires a run trace but none was provided"
	require.Equal(t, want, v.Reason)
}

// cancellingClient cancels the context when asked for a completion, then
// answers like the embedded fakeClient.
type cancellingClient struct {
	*fakeClient
	cancel context.CancelFunc
}

func (c cancellingClient) Complete(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
	c.cancel()
	return c.fakeClient.Complete(ctx, req)
}

func TestComposite_CancelledBetweenSubJudgesIsError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	// The first sub-judge must not decide the outcome, or the composite stops
	// before it looks at the context.
	cases := map[string]string{"any": verdictFail, "all": verdictPass}
	for mode, undecided := range cases {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(undecided)}, cancel: cancel}
			jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
			j := composite(mode, diffReviewJudge(), markerJudge("b", 0), markerJudge("c", 0))

			v, err := Evaluate(ctx, j, jctx)
			require.NoError(t, err, "Evaluate")

			require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
			require.False(t, v.Passed, "verdict = %+v", v)
			require.Equal(t, "cancelled", v.Reason)
			require.False(t, ranMarker(t, jctx.WorkspaceDir, "b"), "a sub-judge ran after the context was cancelled")
			require.False(t, ranMarker(t, jctx.WorkspaceDir, "c"), "a sub-judge ran after the context was cancelled")
			require.Len(t, v.Details, 3, "details = %+v, want one entry per sub-judge", v.Details)
			for _, d := range v.Details[1:] {
				require.Equal(t, eval.JudgeStatusSkipped, d.Status, "detail = %+v", d)
				require.Equal(t, "not evaluated: cancelled", d.Reason, "detail = %+v", d)
			}
		})
	}
}

func TestComposite_CancelledBeforeStartRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), markerJudge("b", 0))

	v, err := Evaluate(ctx, j, JudgeContext{WorkspaceDir: dir})
	require.NoError(t, err, "Evaluate")

	require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
	require.False(t, v.Passed, "verdict = %+v", v)
	require.Equal(t, "cancelled", v.Reason)
	require.False(t, ranMarker(t, dir, "a"), "a sub-judge ran under a cancelled context")
	require.False(t, ranMarker(t, dir, "b"), "a sub-judge ran under a cancelled context")
	assertStatuses(t, v, eval.JudgeStatusSkipped, eval.JudgeStatusSkipped)
}

func TestComposite_ReasonsAreSingularForOneSubJudge(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		judge types.EvalJudge
		want  string
	}{
		{"any with no pass", composite("any", markerJudge("a", 1)), "0 of 1 sub-judge passed (require any)"},
		{"all passing", composite("all", markerJudge("b", 0)), "all 1 sub-judge passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateOK(t, tc.judge, JudgeContext{WorkspaceDir: dir})

			require.Equal(t, tc.want, v.Reason)
		})
	}
}

func TestComposite_AnyAllFailIsFailNotError(t *testing.T) {
	v := evaluateOK(t, composite("any", markerJudge("a", 1), markerJudge("b", 2)), JudgeContext{WorkspaceDir: t.TempDir()})

	require.Equal(t, types.JudgeStatusFail, v.Status, "verdict = %+v", v)
	require.False(t, v.Passed, "verdict = %+v", v)
	assertStatuses(t, v, types.JudgeStatusFail, types.JudgeStatusFail)
	require.Equal(t, "0 of 2 sub-judges passed (require any)", v.Reason)
}

func TestComposite_AllPassReportsStatus(t *testing.T) {
	v := evaluateOK(t, composite("all", markerJudge("a", 0), markerJudge("b", 0)), JudgeContext{WorkspaceDir: t.TempDir()})

	require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusPass)
	require.Equal(t, "all 2 sub-judges passed", v.Reason)
}

func TestComposite_NestedStatusPropagates(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")

	t.Run("nested error is decisive for an outer all", func(t *testing.T) {
		fake := &fakeClient{err: errors.New("provider returned HTTP 503")}
		dir, base := changedWorkspace(t)
		inner := composite("any", diffReviewJudge(), markerJudge("inner-fail", 1))
		j := composite("all", inner, markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{ClientFactory: fake.factory(nil, nil)}})

		require.Equal(t, types.JudgeStatusError, v.Status, "verdict = %+v", v)
		require.False(t, ranMarker(t, dir, "after"), "a sub-judge after the erroring composite must not run")
		assertStatuses(t, v, types.JudgeStatusError, eval.JudgeStatusSkipped)
		require.Equal(t, "composite", v.Details[0].Type, "details[0] = %+v", v.Details[0])
		require.Contains(t, v.Details[0].Reason, "1 errored", "details[0] = %+v", v.Details[0])
		require.True(t, strings.HasPrefix(v.Reason, "sub-judge 1 of 2 (composite) errored (require all); 1 skipped: "), "Reason = %q", v.Reason)
	})

	t.Run("nested pass lets an outer all continue", func(t *testing.T) {
		dir := t.TempDir()
		inner := composite("any", markerJudge("inner-fail", 1), markerJudge("inner-pass", 0), markerJudge("inner-skipped", 0))
		j := composite("all", inner, markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

		require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)
		assertStatuses(t, v, types.JudgeStatusPass, types.JudgeStatusPass)
		require.True(t, ranMarker(t, dir, "after"), "outer all must continue after a passing inner composite")
		require.False(t, ranMarker(t, dir, "inner-skipped"), "inner short-circuit must skip only the inner remainder")
	})

	t.Run("nested fail is decisive for an outer all", func(t *testing.T) {
		dir := t.TempDir()
		j := composite("all", composite("all", markerJudge("inner-fail", 1)), markerJudge("after", 0))

		v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

		require.Equal(t, types.JudgeStatusFail, v.Status, "verdict = %+v", v)
		assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped)
		require.False(t, ranMarker(t, dir, "after"), "a sub-judge after the failing composite must not run")
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
	require.Equal(t, "file-exists,file-contains,test-command,composite", strings.Join(gotTypes, ","), "detail types = %v", gotTypes)
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped, eval.JudgeStatusSkipped, eval.JudgeStatusSkipped)
	require.Equal(t, "sub-judge 1 of 4 (file-exists) failed (require all); 3 skipped", v.Reason)
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

			require.Nil(t, v.Record, "composite verdict should carry no record")
			require.Nil(t, v.Details[0].Record, "deterministic sub-judge should carry no record")

			d := v.Details[1]
			require.Equal(t, tc.wantStatus, d.Status)
			require.NotNil(t, d.Record, "diff-review detail lost its Record")
			require.Equal(t, types.JudgeKindDiffReview, d.Record.Kind)
			require.Equal(t, tc.wantParse, d.Record.ParseStatus)
			require.Equal(t, "served-model-1", d.Record.ServedModel)
		})
	}
}

func TestComposite_NestedDetailsKeepRecordsAtAnyDepth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	innermost := composite("any", markerJudge("a", 1), diffReviewJudge())
	j := composite("all", markerJudge("b", 0), composite("all", innermost))

	v := evaluateOK(t, j, changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)}))

	require.Equal(t, types.JudgeStatusPass, v.Status, "verdict = %+v", v)

	middle := v.Details[1]
	require.Equal(t, "composite", middle.Type)
	require.Nil(t, middle.Record)
	require.Len(t, middle.Details, 1, "details[1] should hold one nested entry")

	inner := middle.Details[0]
	require.Equal(t, "composite", inner.Type)
	require.Nil(t, inner.Record)
	require.Len(t, inner.Details, 2, "details[1].details[0] should hold two nested entries")

	review := inner.Details[1]
	require.Equal(t, "diff-review", review.Type)
	require.Equal(t, types.JudgeStatusPass, review.Status, "innermost entry = %+v", review)
	require.NotNil(t, review.Record, "innermost diff-review lost its Record")
	require.Equal(t, types.JudgeKindDiffReview, review.Record.Kind)
	require.Equal(t, "served-model-1", review.Record.ServedModel)

	require.Empty(t, v.Details[0].Details, "a deterministic sub-judge carries no details")
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

			require.ErrorContains(t, err, tc.wantMsg)
			require.False(t, ranMarker(t, dir, "a"), "no sub-judge may run when the tree is invalid")
		})
	}
}

func TestComposite_NestedConfigurationErrorsNamePosition(t *testing.T) {
	cases := []struct {
		name  string
		judge types.EvalJudge
		want  string
	}{
		{
			"empty composite two levels down",
			composite("all", markerJudge("a", 0), composite("any", markerJudge("b", 0), composite("any"))),
			"sub-judge 2: sub-judge 2: composite judge requires at least one sub-judge",
		},
		{
			"invalid leaf in a nested composite",
			composite("all", markerJudge("a", 0), composite("any", types.EvalJudge{Type: "test-command"})),
			"sub-judge 2: sub-judge 1: test-command judge requires a command",
		},
		{
			"unknown type",
			composite("all", markerJudge("a", 0), types.EvalJudge{Type: "nonexistent"}),
			`sub-judge 2: unknown judge type: "nonexistent"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(context.Background(), tc.judge, JudgeContext{WorkspaceDir: t.TempDir()})

			require.EqualError(t, err, tc.want)
		})
	}
}

func TestComposite_StaticLeafErrorsAreHardErrors(t *testing.T) {
	cases := []struct {
		name    string
		leaf    types.EvalJudge
		wantMsg string
	}{
		{"test-command without a command", types.EvalJudge{Type: "test-command"}, "requires a command"},
		{"file-exists with an empty path", types.EvalJudge{Type: "file-exists", Paths: []string{"a", ""}}, "non-empty paths"},
		{"file-exists outside the workspace", types.EvalJudge{Type: "file-exists", Paths: []string{"../outside"}}, "resolves outside workspace"},
		{"file-contains without a path", types.EvalJudge{Type: "file-contains", Pattern: "x"}, "requires a path"},
		{"file-contains without a pattern", types.EvalJudge{Type: "file-contains", Path: "a"}, "requires a pattern"},
		{"file-contains with an invalid pattern", types.EvalJudge{Type: "file-contains", Path: "a", Pattern: "("}, "invalid pattern"},
		{"file-contains outside the workspace", types.EvalJudge{Type: "file-contains", Path: "../outside", Pattern: "x"}, "resolves outside workspace"},
		{"tool-trace without a block", types.EvalJudge{Type: "tool-trace"}, "requires a toolTrace block"},
		{"diff-review without criteria", types.EvalJudge{Type: "diff-review"}, "requires a criteria string"},
		{
			"diff-review with an invalid llm block",
			types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Provider: "bogus", Model: "m"}},
			"llm block",
		},
		{
			"llm block on another type",
			types.EvalJudge{Type: "test-command", Command: "true", LLM: &types.JudgeLLMConfig{Model: "m"}},
			"does not support an llm block",
		},
	}
	for _, mode := range []string{"any", "all"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				writeFile(t, dir, "present.txt", "x")
				first := markerJudge("a", 0)
				if mode == "all" {
					first = markerJudge("a", 1)
				}
				j := composite(mode, first, tc.leaf)

				_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})

				require.ErrorContains(t, err, "sub-judge 2: ")
				require.ErrorContains(t, err, tc.wantMsg)
				require.False(t, ranMarker(t, dir, "a"), "no sub-judge may run when a leaf is mis-specified")
			})
		}
	}
}

func TestComposite_InvalidPatternBehindPassingJudgeIsHardError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "present.txt", "x")
	j := composite("any",
		types.EvalJudge{Type: "file-exists", Paths: []string{"present.txt"}},
		types.EvalJudge{Type: "file-contains", Path: "present.txt", Pattern: "("},
	)

	v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})

	require.ErrorContains(t, err, "invalid pattern")
	require.False(t, v.Passed, "verdict = %+v, a mis-specified sub-judge must not pass", v)
	require.NotEqual(t, types.JudgeStatusPass, v.Status, "verdict = %+v, a mis-specified sub-judge must not pass", v)
}

func TestValidateTree_AcceptsValidAndRejectsInvalid(t *testing.T) {
	t.Run("accepts a valid tree", func(t *testing.T) {
		valid := composite("all",
			types.EvalJudge{Type: "file-exists", Paths: []string{"a", "dir/b"}},
			types.EvalJudge{Type: "file-contains", Path: "a", Pattern: `^\d+$`},
			types.EvalJudge{Type: "tool-trace", ToolTrace: &types.ToolTraceCriteria{}},
			composite("any", types.EvalJudge{Type: "test-command", Command: "true"}, diffReviewJudge()),
		)
		require.NoError(t, ValidateTree(valid))
	})
	t.Run("rejects a zero judge", func(t *testing.T) {
		require.ErrorContains(t, ValidateTree(types.EvalJudge{}), "unknown judge type")
	})
	t.Run("rejects a root leaf with a missing command", func(t *testing.T) {
		require.Error(t, ValidateTree(types.EvalJudge{Type: "test-command"}))
	})
}

// TestDiffReview_RequiresCriteria pins that the diff-review judge fails
// fast when criteria is empty, without making an API call.
func TestDiffReview_RequiresCriteria(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "diff-review"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.ErrorContains(t, err, "criteria")
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
	require.ErrorContains(t, err, "ANTHROPIC_API_KEY")
}

// TestEvaluate_StatusMatchesPassed pins that every judge type reports a
// Status consistent with Passed, so consumers can rely on Status alone.
func TestEvaluate_StatusMatchesPassed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "present.txt"), []byte("x"), 0o644))
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
			require.NoError(t, err)
			want := types.JudgeStatusFail
			if tc.wantPassed {
				want = types.JudgeStatusPass
			}
			require.Equal(t, tc.wantPassed, v.Passed, "status %q", v.Status)
			require.Equal(t, want, v.Status)
		})
	}
}

func TestFileExists_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-exists", Paths: []string{"../../../etc/passwd"}}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.Error(t, err)
}

func TestFileContains_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "file-contains", Path: "../../../etc/passwd", Pattern: "root"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.Error(t, err)
}

func TestEvaluate_RejectsUnknownJudgeType(t *testing.T) {
	dir := t.TempDir()
	j := types.EvalJudge{Type: "nonexistent"}
	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	require.Error(t, err)
}

func TestContainsType_SearchesNestedJudges(t *testing.T) {
	nested := types.EvalJudge{Type: "composite", Judges: []types.EvalJudge{
		{Type: "file-exists"},
		{Type: "composite", Judges: []types.EvalJudge{{Type: "diff-review"}}},
	}}
	require.True(t, ContainsType(nested, "diff-review"), "nested diff-review not found")
	require.False(t, ContainsType(nested, "test-command"), "absent type reported present")
	require.True(t, ContainsType(types.EvalJudge{Type: "diff-review"}, "diff-review"), "top-level diff-review not found")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "creating parent dirs")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "writing %s", name)
}
