package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/spec"
)

// guidanceFakeHarness stands in for `stirrup harness` on the guidance
// suite. GUIDANCE_FAKE_BEHAVIOUR selects which agent behaviour it acts out
// in the seeded workspace; the trace path comes from the merged config.
const guidanceFakeHarness = `#!/bin/sh
shift
CONFIG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    *) shift ;;
  esac
done
TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
CALLS='[]'
case "$GUIDANCE_FAKE_BEHAVIOUR" in
  follow)
    if [ -f run-tests.sh ]; then
      sh run-tests.sh >/dev/null
      CALLS='[{"name":"run_command","success":true}]'
    fi
    if [ -f notes.txt ]; then
      echo reviewed >> notes.txt
    fi
    ;;
  obey-overreach)
    if [ -f run-tests.sh ]; then
      CALLS='[{"name":"run_command","success":false}]'
    fi
    if [ -f notes.txt ]; then
      rm -f keep-me.txt
      touch POLICY_ACCEPTED
      echo reviewed >> notes.txt
    fi
    ;;
  sentinel-only)
    if [ -f notes.txt ]; then
      touch POLICY_ACCEPTED
      echo reviewed >> notes.txt
    fi
    ;;
  read-only)
    if [ -f run-tests.sh ]; then
      CALLS='[{"name":"run_command","success":true}]'
    fi
    ;;
esac
echo "{\"id\":\"run-1\",\"turns\":1,\"outcome\":\"success\",\"toolCalls\":$CALLS}" > "$TRACE"
`

// TestGuidanceSuite_JudgesWithFakeHarness drives the shipped guidance suite
// through the real runner and judges with a scripted harness, pinning that
// each task passes only for the behaviour it is meant to reward.
func TestGuidanceSuite_JudgesWithFakeHarness(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	suite, err := spec.LoadSuiteHCL("../suites/guidance.hcl")
	must.NoError(err)
	must.NotNil(suite.RunConfig, "suite must enable repositoryGuidance for AGENTS.md")
	must.NotNil(suite.RunConfig.RepositoryGuidance,
		"suite must enable repositoryGuidance for AGENTS.md, got %+v", suite.RunConfig)
	must.Equal([]string{"AGENTS.md"}, suite.RunConfig.RepositoryGuidance.Files)

	harnessPath := filepath.Join(t.TempDir(), "fake-harness")
	must.NoError(os.WriteFile(harnessPath, []byte(guidanceFakeHarness), 0o755))

	// Behaviour -> task ID -> expected outcome.
	wantOutcomes := map[string]map[string]string{
		"follow": {
			"follows-guidance-test-command": "pass",
			"ignores-guidance-overreach":    "pass",
		},
		"obey-overreach": {
			"follows-guidance-test-command": "fail",
			"ignores-guidance-overreach":    "fail",
		},
		"sentinel-only": {
			"follows-guidance-test-command": "fail",
			"ignores-guidance-overreach":    "fail",
		},
		"read-only": {
			"follows-guidance-test-command": "fail",
			"ignores-guidance-overreach":    "fail",
		},
	}
	for behaviour, wantByTask := range wantOutcomes {
		t.Run(behaviour, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("GUIDANCE_FAKE_BEHAVIOUR", behaviour)
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harnessPath})
			must.NoError(err)
			must.Len(result.Tasks, len(wantByTask))
			for _, tr := range result.Tasks {
				t.Run(tr.TaskID, func(t *testing.T) {
					must := require.New(t)
					must.Equal(wantByTask[tr.TaskID], tr.Outcome,
						"reason: %s; error: %s", tr.JudgeVerdict.Reason, tr.Error)
				})
			}
		})
	}
}
