package runner

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
			must := require.New(t)
			got := buildResult("t1", time.Now(), nil, tc.verdict, false)
			must.Equal(tc.wantOutcome, got.Outcome)
			must.Equal(tc.wantError, got.Error)
			if got.Outcome == "error" {
				must.False(got.JudgeVerdict.Passed, "an error result must not report Passed")
			}

			var junit bytes.Buffer
			must.NoError(reporter.WriteJUnit(&junit, eval.SuiteResult{SuiteID: "s", Tasks: []eval.TaskResult{got}}))
			hasError := strings.Contains(junit.String(), "<error")
			must.Equal(tc.wantOutcome == "error", hasError, "JUnit <error> element present for outcome %q:\n%s", tc.wantOutcome, junit.String())
		})
	}
}
