package runner

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/reporter"
	"github.com/rxbynerd/stirrup/types"
)

func TestBuildResult_ErrorStatusIsTheErrorOutcome(t *testing.T) {
	cases := []struct {
		name        string
		verdict     eval.JudgeVerdict
		wantOutcome string
		wantError   string
	}{
		{name: "pass", verdict: eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusPass}, wantOutcome: "pass"},
		{name: "fail", verdict: eval.JudgeVerdict{Passed: false, Status: types.JudgeStatusFail}, wantOutcome: "fail"},
		{name: "legacy passed without status", verdict: eval.JudgeVerdict{Passed: true}, wantOutcome: "pass"},
		{name: "error claiming a pass", verdict: eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusError, Reason: "model call failed"}, wantOutcome: "error", wantError: "model call failed"},
		{name: "error claiming a fail", verdict: eval.JudgeVerdict{Passed: false, Status: types.JudgeStatusError, Reason: "no verdict"}, wantOutcome: "error", wantError: "no verdict"},
		{name: "error without a reason", verdict: eval.JudgeVerdict{Status: types.JudgeStatusError}, wantOutcome: "error", wantError: "judge returned an error verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildResult("t1", time.Now(), nil, tc.verdict)
			if got.Outcome != tc.wantOutcome || got.Error != tc.wantError {
				t.Fatalf("outcome %q error %q, want %q / %q", got.Outcome, got.Error, tc.wantOutcome, tc.wantError)
			}
			if got.Outcome == "error" && got.JudgeVerdict.Passed {
				t.Error("an error result still reports Passed")
			}

			var junit bytes.Buffer
			if err := reporter.WriteJUnit(&junit, eval.SuiteResult{SuiteID: "s", Tasks: []eval.TaskResult{got}}); err != nil {
				t.Fatal(err)
			}
			if hasError := strings.Contains(junit.String(), "<error"); hasError != (tc.wantOutcome == "error") {
				t.Errorf("JUnit <error> present = %v for outcome %q:\n%s", hasError, tc.wantOutcome, junit.String())
			}
		})
	}
}
