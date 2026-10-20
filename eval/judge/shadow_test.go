package judge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func shadow(j types.EvalJudge) types.EvalJudge {
	j.Shadow = true
	return j
}

func fileExistsJudge(paths ...string) types.EvalJudge {
	return types.EvalJudge{Type: "file-exists", Paths: paths}
}

func TestComposite_ShadowFailDoesNotFailAll(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), shadow(fileExistsJudge("missing.txt")), markerJudge("b", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v: a shadow fail must not decide", v)
	assertStatuses(t, v, types.JudgeStatusPass, eval.JudgeStatusShadow, types.JudgeStatusPass)
	d := v.Details[1]
	check.Equal(types.JudgeStatusFail, d.ShadowVerdict, "shadow detail = %+v", d)
	check.False(d.Passed)
	check.Equal("missing paths: missing.txt", d.Reason)
	check.Equal("all 2 sub-judges passed; 1 shadow recorded", v.Reason)
}

func TestComposite_ShadowPassDoesNotSatisfyAny(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"present.txt": "x"})
	j := composite("any", shadow(fileExistsJudge("present.txt")), markerJudge("a", 1))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	must.Equal(types.JudgeStatusFail, v.Status, "verdict = %+v: a shadow pass must not decide", v)
	must.False(v.Passed)
	assertStatuses(t, v, eval.JudgeStatusShadow, types.JudgeStatusFail)
	check.Equal(types.JudgeStatusPass, v.Details[0].ShadowVerdict)
	check.Equal("0 of 1 sub-judge passed (require any); 1 shadow recorded", v.Reason)
}

func TestComposite_ShadowErrorKeepsRecordAndNeverErrorsComposite(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	for _, mode := range []string{"all", "any"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			fake := &fakeClient{err: errors.New("endpoint unreachable")}
			jctx := changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)})
			j := composite(mode, shadow(diffReviewJudge()), markerJudge("a", 0))

			v := evaluateOK(t, j, jctx)

			must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v: a shadow error must not decide", v)
			d := v.Details[0]
			must.Equal(eval.JudgeStatusShadow, d.Status, "shadow detail = %+v", d)
			must.Equal(types.JudgeStatusError, d.ShadowVerdict)
			must.NotNil(d.Record, "shadow detail = %+v", d)
			must.Equal(types.JudgeKindDiffReview, d.Record.Kind)
			must.Contains(d.Reason, "endpoint unreachable")
		})
	}
}

func TestComposite_ShadowRunsAfterTheOutcomeIsDecided(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 1), markerJudge("b", 0), shadow(fileExistsJudge("a")))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	must.Equal(types.JudgeStatusFail, v.Status, "verdict = %+v", v)
	check.False(ranMarker(t, dir, "b"), "a deciding sub-judge ran after the outcome was decided")
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped, eval.JudgeStatusShadow)
	d := v.Details[2]
	check.Equal(types.JudgeStatusPass, d.ShadowVerdict, "shadow detail = %+v, want it evaluated after the outcome was decided", d)
	check.Equal("all paths exist", d.Reason)
	check.Equal("sub-judge 1 of 3 (test-command) failed (require all); 1 skipped; 1 shadow recorded", v.Reason)
}

func TestComposite_ShadowLLMVerdictIsRecordedBesideDecidingJudge(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictFail)}
	jctx := changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)})
	j := composite("all", markerJudge("a", 0), shadow(diffReviewJudge()))

	v := evaluateOK(t, j, jctx)

	must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	must.Equal(1, fake.calls, "model calls")
	d := v.Details[1]
	must.Equal(types.JudgeStatusFail, d.ShadowVerdict, "shadow detail = %+v", d)
	must.NotNil(d.Record, "shadow detail = %+v", d)
	must.Equal(types.JudgeParseOK, d.Record.ParseStatus)
}

func TestComposite_CancelledShadowAfterDecisionKeepsTheDecision(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(verdictFail)}, cancel: cancel}
	jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
	j := composite("all", diffReviewJudge(), shadow(fileExistsJudge("a.txt")))

	v, err := Evaluate(ctx, j, jctx)
	must.NoError(err)

	must.Equal(types.JudgeStatusFail, v.Status, "verdict = %+v: want the fail decided before cancellation", v)
	d := v.Details[1]
	must.Equal(eval.JudgeStatusSkipped, d.Status, "shadow detail = %+v", d)
	must.Equal("not evaluated: cancelled", d.Reason)
}

func TestComposite_NestedShadowCompositeKeepsItsDetails(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	inner := shadow(composite("all", fileExistsJudge("missing.txt")))
	j := composite("all", markerJudge("a", 0), inner)

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	d := v.Details[1]
	must.Equal(eval.JudgeStatusShadow, d.Status, "nested shadow detail = %+v", d)
	must.Equal(types.JudgeStatusFail, d.ShadowVerdict)
	must.Len(d.Details, 1)
}

func TestValidateTree_ShadowRules(t *testing.T) {
	cases := []struct {
		name    string
		judge   types.EvalJudge
		wantErr string
	}{
		{"top-level shadow", shadow(fileExistsJudge("a")), "only valid on a composite sub-judge"},
		{"top-level shadow composite", shadow(composite("all", fileExistsJudge("a"))), "only valid on a composite sub-judge"},
		{"composite of shadows", composite("all", shadow(fileExistsJudge("a"))), "at least one sub-judge that is not a shadow"},
		{"nested composite of shadows", composite("all", markerJudge("a", 0), composite("any", shadow(fileExistsJudge("b")))), "sub-judge 2: composite judge needs at least one"},
		{"shadow beside a deciding judge", composite("all", markerJudge("a", 0), shadow(fileExistsJudge("b"))), ""},
		{"shadow composite with a deciding member", composite("all", markerJudge("a", 0), shadow(composite("all", fileExistsJudge("b")))), ""},
		{"shadow test-command", composite("all", fileExistsJudge("a"), shadow(markerJudge("b", 0))), `a "test-command" judge cannot be a shadow`},
		{"test-command inside a shadow composite", composite("all", fileExistsJudge("a"), shadow(composite("all", markerJudge("b", 0)))), "cannot be a shadow or sit inside one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := ValidateTree(tc.judge)
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestComposite_RejectsAllShadowTreeBeforeRunningAnything(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), composite("any", shadow(fileExistsJudge("b"))))

	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	must.Error(err, "a composite with no deciding sub-judge was evaluated")
	must.False(ranMarker(t, dir, "a"), "a sub-judge ran in a rejected tree")
}

func TestComposite_CancelledShadowDoesNotTurnAnUndecidedOutcomeIntoError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cases := map[string]struct{ reply, want, reason string }{
		"all": {verdictPass, types.JudgeStatusPass, "all 1 sub-judge passed"},
		"any": {verdictFail, types.JudgeStatusFail, "0 of 1 sub-judge passed (require any)"},
	}
	for mode, tc := range cases {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(tc.reply)}, cancel: cancel}
			jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
			j := composite(mode, diffReviewJudge(), shadow(fileExistsJudge("a.txt")))

			v, err := Evaluate(ctx, j, jctx)
			must.NoError(err)

			must.Equal(tc.want, v.Status, "verdict = %+v: a skipped shadow must not decide", v)
			must.Equal(tc.reason, v.Reason)
			assertStatuses(t, v, tc.want, eval.JudgeStatusSkipped)
			d := v.Details[1]
			must.Equal("not evaluated: cancelled", d.Reason, "shadow detail = %+v", d)
		})
	}
}

func TestComposite_CancelledBeforeADecidingJudgeAfterAShadowIsError(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(verdictPass)}, cancel: cancel}
	jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
	j := composite("all", shadow(diffReviewJudge()), fileExistsJudge("a.txt"))

	v, err := Evaluate(ctx, j, jctx)
	must.NoError(err)

	must.Equal(types.JudgeStatusError, v.Status, "verdict = %+v: the deciding judge never ran", v)
	must.Equal("cancelled", v.Reason)
	assertStatuses(t, v, eval.JudgeStatusShadow, eval.JudgeStatusSkipped)
}

func TestComposite_ShadowNotesCountEveryShadow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "x"})
	j := composite("all", fileExistsJudge("a.txt"), shadow(fileExistsJudge("a.txt")), shadow(fileExistsJudge("missing.txt")))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	must.Equal("all 1 sub-judge passed; 2 shadows recorded", v.Reason)
	check.Equal(types.JudgeStatusPass, v.Details[1].ShadowVerdict)
	check.Equal(types.JudgeStatusFail, v.Details[2].ShadowVerdict)
}

func TestComposite_DecidingErrorBesideAPassingShadowIsError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	unreadable := types.EvalJudge{Type: "file-contains", Path: "sub", Pattern: "x"}
	cases := map[string]string{
		"all": "sub-judge 1 of 2 (file-contains) errored (require all); 1 shadow recorded: ",
		"any": "0 of 1 sub-judge passed (require any); 1 shadow recorded; 1 errored, first: sub-judge 1 of 2 (file-contains): ",
	}
	for mode, prefix := range cases {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			v := evaluateOK(t, composite(mode, unreadable, shadow(fileExistsJudge("a.txt"))), JudgeContext{WorkspaceDir: dir})

			must.Equal(types.JudgeStatusError, v.Status, "verdict = %+v", v)
			must.True(strings.HasPrefix(v.Reason, prefix), "reason = %q, want prefix %q", v.Reason, prefix)
			d := v.Details[1]
			must.Equal(eval.JudgeStatusShadow, d.Status, "shadow detail = %+v", d)
			must.Equal(types.JudgeStatusPass, d.ShadowVerdict, "shadow detail = %+v, want a recorded pass that does not count", d)
		})
	}
}

func TestComposite_RejectsAShadowTestCommandBeforeRunningAnything(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), shadow(markerJudge("s", 0)))

	_, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir})
	must.ErrorContains(err, "cannot be a shadow")
	must.False(ranMarker(t, dir, "a"), "a sub-judge ran in a rejected tree")
	must.False(ranMarker(t, dir, "s"), "a shadow test-command ran in a rejected tree")
}
