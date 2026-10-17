package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	suite, err := spec.LoadSuiteHCL("../suites/guidance.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if suite.RunConfig == nil || suite.RunConfig.RepositoryGuidance == nil ||
		len(suite.RunConfig.RepositoryGuidance.Files) != 1 || suite.RunConfig.RepositoryGuidance.Files[0] != "AGENTS.md" {
		t.Fatalf("suite must enable repositoryGuidance for AGENTS.md, got %+v", suite.RunConfig)
	}

	harnessPath := filepath.Join(t.TempDir(), "fake-harness")
	if err := os.WriteFile(harnessPath, []byte(guidanceFakeHarness), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]map[string]string{
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
	for behaviour, want := range cases {
		t.Run(behaviour, func(t *testing.T) {
			t.Setenv("GUIDANCE_FAKE_BEHAVIOUR", behaviour)
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harnessPath})
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if len(result.Tasks) != len(want) {
				t.Fatalf("got %d task results, want %d", len(result.Tasks), len(want))
			}
			for _, tr := range result.Tasks {
				if tr.Outcome != want[tr.TaskID] {
					t.Errorf("task %s: outcome %q, want %q (reason: %s; error: %s)",
						tr.TaskID, tr.Outcome, want[tr.TaskID], tr.JudgeVerdict.Reason, tr.Error)
				}
			}
		})
	}
}
