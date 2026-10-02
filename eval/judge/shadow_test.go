package judge

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 0), shadow(markerJudge("s", 1)), markerJudge("b", 0))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusPass {
		t.Fatalf("verdict = %+v, want pass: a shadow fail must not decide", v)
	}
	assertStatuses(t, v, types.JudgeStatusPass, eval.JudgeStatusShadow, types.JudgeStatusPass)
	if d := v.Details[1]; d.ShadowVerdict != types.JudgeStatusFail || d.Passed || !strings.Contains(d.Reason, "command failed") {
		t.Errorf("shadow detail = %+v, want verdict fail recorded with its reason", d)
	}
	if want := "all 2 sub-judges passed; 1 shadow recorded"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_ShadowPassDoesNotSatisfyAny(t *testing.T) {
	dir := t.TempDir()
	j := composite("any", shadow(markerJudge("s", 0)), markerJudge("a", 1))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusFail || v.Passed {
		t.Fatalf("verdict = %+v, want fail: a shadow pass must not decide", v)
	}
	assertStatuses(t, v, eval.JudgeStatusShadow, types.JudgeStatusFail)
	if v.Details[0].ShadowVerdict != types.JudgeStatusPass {
		t.Errorf("shadow verdict = %q, want pass", v.Details[0].ShadowVerdict)
	}
	if want := "0 of 1 sub-judge passed (require any); 1 shadow recorded"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_ShadowErrorKeepsRecordAndNeverErrorsComposite(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	for _, require := range []string{"all", "any"} {
		t.Run(require, func(t *testing.T) {
			fake := &fakeClient{err: errors.New("endpoint unreachable")}
			jctx := changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)})
			j := composite(require, shadow(diffReviewJudge()), markerJudge("a", 0))

			v := evaluateOK(t, j, jctx)

			if v.Status != types.JudgeStatusPass {
				t.Fatalf("verdict = %+v, want pass: a shadow error must not decide", v)
			}
			d := v.Details[0]
			if d.Status != eval.JudgeStatusShadow || d.ShadowVerdict != types.JudgeStatusError {
				t.Fatalf("shadow detail = %+v, want status shadow with verdict error", d)
			}
			if d.Record == nil || d.Record.Kind != types.JudgeKindDiffReview || !strings.Contains(d.Reason, "endpoint unreachable") {
				t.Errorf("shadow detail = %+v, want the diff-review record and error reason", d)
			}
		})
	}
}

func TestComposite_ShadowRunsAfterTheOutcomeIsDecided(t *testing.T) {
	dir := t.TempDir()
	j := composite("all", markerJudge("a", 1), markerJudge("b", 0), shadow(markerJudge("s", 0)))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusFail {
		t.Fatalf("verdict = %+v, want fail", v)
	}
	if ranMarker(t, dir, "b") {
		t.Error("a deciding sub-judge ran after the outcome was decided")
	}
	if !ranMarker(t, dir, "s") {
		t.Error("a shadow sub-judge was not evaluated after the outcome was decided")
	}
	assertStatuses(t, v, types.JudgeStatusFail, eval.JudgeStatusSkipped, eval.JudgeStatusShadow)
	if want := "sub-judge 1 of 3 (test-command) failed (require all); 1 skipped; 1 shadow recorded"; v.Reason != want {
		t.Errorf("Reason = %q, want %q", v.Reason, want)
	}
}

func TestComposite_ShadowLLMVerdictIsRecordedBesideDecidingJudge(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictFail)}
	jctx := changedJudgeContext(t, Options{ClientFactory: fake.factory(nil, nil)})
	j := composite("all", markerJudge("a", 0), shadow(diffReviewJudge()))

	v := evaluateOK(t, j, jctx)

	if v.Status != types.JudgeStatusPass || fake.calls != 1 {
		t.Fatalf("verdict = %+v after %d model calls, want pass with the shadow called once", v, fake.calls)
	}
	d := v.Details[1]
	if d.ShadowVerdict != types.JudgeStatusFail || d.Record == nil || d.Record.ParseStatus != types.JudgeParseOK {
		t.Errorf("shadow detail = %+v, want a parsed fail verdict with its record", d)
	}
}

func TestComposite_CancelledShadowAfterDecisionKeepsTheDecision(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(verdictFail)}, cancel: cancel}
	jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
	j := composite("all", diffReviewJudge(), shadow(markerJudge("s", 0)))

	v, err := Evaluate(ctx, j, jctx)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}

	if v.Status != types.JudgeStatusFail {
		t.Fatalf("verdict = %+v, want the fail decided before cancellation", v)
	}
	if ranMarker(t, jctx.WorkspaceDir, "s") {
		t.Error("a shadow sub-judge ran after the context was cancelled")
	}
	if d := v.Details[1]; d.Status != eval.JudgeStatusSkipped || d.Reason != "not evaluated: cancelled" {
		t.Errorf("shadow detail = %+v, want skipped because of cancellation", d)
	}
}

func TestComposite_NestedShadowCompositeKeepsItsDetails(t *testing.T) {
	dir := t.TempDir()
	inner := shadow(composite("all", markerJudge("x", 1)))
	j := composite("all", markerJudge("a", 0), inner)

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if v.Status != types.JudgeStatusPass {
		t.Fatalf("verdict = %+v, want pass", v)
	}
	d := v.Details[1]
	if d.Status != eval.JudgeStatusShadow || d.ShadowVerdict != types.JudgeStatusFail || len(d.Details) != 1 {
		t.Errorf("nested shadow detail = %+v, want a failed shadow composite with its own details", d)
	}
}

func TestValidateTree_ShadowRules(t *testing.T) {
	cases := []struct {
		name    string
		judge   types.EvalJudge
		wantErr string
	}{
		{"top-level shadow", shadow(markerJudge("a", 0)), "only valid on a composite sub-judge"},
		{"top-level shadow composite", shadow(composite("all", markerJudge("a", 0))), "only valid on a composite sub-judge"},
		{"composite of shadows", composite("all", shadow(markerJudge("a", 0))), "at least one sub-judge that is not a shadow"},
		{"nested composite of shadows", composite("all", markerJudge("a", 0), composite("any", shadow(markerJudge("b", 0)))), "sub-judge 2: composite judge needs at least one"},
		{"shadow beside a deciding judge", composite("all", markerJudge("a", 0), shadow(markerJudge("b", 0))), ""},
		{"shadow composite with a deciding member", composite("all", markerJudge("a", 0), shadow(composite("all", markerJudge("b", 0)))), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTree(tc.judge)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateTree: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ValidateTree error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestComposite_RejectsAllShadowTreeBeforeRunningAnything(t *testing.T) {
	dir := t.TempDir()
	j := composite("all", shadow(markerJudge("a", 0)))

	if _, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir}); err == nil {
		t.Fatal("a composite with no deciding sub-judge was evaluated")
	}
	if ranMarker(t, dir, "a") {
		t.Error("a sub-judge ran in a rejected tree")
	}
}

func TestComposite_CancelledShadowDoesNotTurnAnUndecidedOutcomeIntoError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cases := map[string]struct{ reply, want, reason string }{
		"all": {verdictPass, types.JudgeStatusPass, "all 1 sub-judge passed"},
		"any": {verdictFail, types.JudgeStatusFail, "0 of 1 sub-judge passed (require any)"},
	}
	for require, tc := range cases {
		t.Run(require, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(tc.reply)}, cancel: cancel}
			jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
			j := composite(require, diffReviewJudge(), shadow(fileExistsJudge("a.txt")))

			v, err := Evaluate(ctx, j, jctx)
			if err != nil {
				t.Fatalf("Evaluate returned an error: %v", err)
			}

			if v.Status != tc.want || v.Reason != tc.reason {
				t.Fatalf("verdict = %+v, want %s with reason %q: a skipped shadow must not decide", v, tc.want, tc.reason)
			}
			assertStatuses(t, v, tc.want, eval.JudgeStatusSkipped)
			if d := v.Details[1]; d.Reason != "not evaluated: cancelled" {
				t.Errorf("shadow detail = %+v, want skipped because of cancellation", d)
			}
		})
	}
}

func TestComposite_CancelledBeforeADecidingJudgeAfterAShadowIsError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := cancellingClient{fakeClient: &fakeClient{resp: okResponse(verdictPass)}, cancel: cancel}
	jctx := changedJudgeContext(t, Options{ClientFactory: func(types.JudgeLLMConfig, string) (JudgeClient, error) { return client, nil }})
	j := composite("all", shadow(diffReviewJudge()), fileExistsJudge("a.txt"))

	v, err := Evaluate(ctx, j, jctx)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}

	if v.Status != types.JudgeStatusError || v.Reason != "cancelled" {
		t.Fatalf("verdict = %+v, want error: the deciding judge never ran", v)
	}
	assertStatuses(t, v, eval.JudgeStatusShadow, eval.JudgeStatusSkipped)
}

func TestComposite_ShadowNotesCountEveryShadow(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "x"})
	j := composite("all", fileExistsJudge("a.txt"), shadow(fileExistsJudge("a.txt")), shadow(fileExistsJudge("missing.txt")))

	v := evaluateOK(t, j, JudgeContext{WorkspaceDir: dir})

	if want := "all 1 sub-judge passed; 2 shadows recorded"; v.Status != types.JudgeStatusPass || v.Reason != want {
		t.Fatalf("verdict = %+v, want pass with reason %q", v, want)
	}
	if v.Details[1].ShadowVerdict != types.JudgeStatusPass || v.Details[2].ShadowVerdict != types.JudgeStatusFail {
		t.Errorf("shadow verdicts = %q, %q; want pass, fail", v.Details[1].ShadowVerdict, v.Details[2].ShadowVerdict)
	}
}

func TestComposite_DecidingErrorBesideAPassingShadowIsError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	unreadable := types.EvalJudge{Type: "file-contains", Path: "sub", Pattern: "x"}
	cases := map[string]string{
		"all": "sub-judge 1 of 2 (file-contains) errored (require all); 1 shadow recorded: ",
		"any": "0 of 1 sub-judge passed (require any); 1 shadow recorded; 1 errored, first: sub-judge 1 of 2 (file-contains): ",
	}
	for require, prefix := range cases {
		t.Run(require, func(t *testing.T) {
			v := evaluateOK(t, composite(require, unreadable, shadow(fileExistsJudge("a.txt"))), JudgeContext{WorkspaceDir: dir})

			if v.Status != types.JudgeStatusError || !strings.HasPrefix(v.Reason, prefix) {
				t.Fatalf("verdict = %+v, want error with reason starting %q", v, prefix)
			}
			if d := v.Details[1]; d.Status != eval.JudgeStatusShadow || d.ShadowVerdict != types.JudgeStatusPass {
				t.Errorf("shadow detail = %+v, want a recorded pass that does not count", d)
			}
		})
	}
}
