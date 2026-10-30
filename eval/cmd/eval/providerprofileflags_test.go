package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const testCommandSuiteHCL = `
suite "profile-flags-suite" {
  description = "fixture for the provider profile flags"

  task "noop" {
    prompt = "noop"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`

// TestCmdRun_ProviderProfileFlagsReachHarnessArgv pins the path from the
// run flags through the runner to the harness argv.
func TestCmdRun_ProviderProfileFlagsReachHarnessArgv(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
		not  []string
	}{
		{
			name: "both profiles set",
			args: []string{"--endpoint-profile", "azure-openai", "--compat-profile", "zai-glm"},
			want: []string{"--provider-endpoint-profile\nazure-openai\n", "--provider-compat-profile\nzai-glm\n"},
		},
		{
			name: "profiles unset",
			not:  []string{"--provider-endpoint-profile", "--provider-compat-profile"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			argvPath := filepath.Join(t.TempDir(), "argv.txt")
			harnessPath := writeFakeHarness(t, `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done > `+argvPath+`
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
fi
`)
			args := append([]string{
				"run", "--suite", writeSuite(t, testCommandSuiteHCL), "--harness", harnessPath,
				"--output", filepath.Join(t.TempDir(), "out"),
			}, tc.args...)

			must.Equal(0, run(args, io.Discard))

			raw, err := os.ReadFile(argvPath)
			must.NoError(err)
			argv := string(raw)
			for _, want := range tc.want {
				must.Contains(argv, want)
			}
			for _, not := range tc.not {
				must.NotContains(argv, not)
			}
		})
	}
}

func TestCompletion_ProviderProfileFlagsRegistered(t *testing.T) {
	must := require.New(t)
	for _, f := range []string{"endpoint-profile", "compat-profile"} {
		must.Contains(evalCompletionFlags["run"], f)
	}
}
