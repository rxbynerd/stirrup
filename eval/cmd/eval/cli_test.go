package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const cliSubprocessEnv = "STIRRUP_EVAL_TEST_AS_CLI"

// TestMain lets tests re-execute the test binary as the real CLI, so
// code paths that exit through log.Fatalf can be observed end to end.
func TestMain(m *testing.M) {
	if os.Getenv(cliSubprocessEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// runCLI re-executes the test binary as the CLI and returns its output and exit code.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // re-executes this test binary with test-controlled arguments
	cmd.Env = append(os.Environ(), cliSubprocessEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exitErr *exec.ExitError
	must.ErrorAs(err, &exitErr, "running the CLI")
	return out.String(), errOut.String(), exitErr.ExitCode()
}

func writeTwoTaskSuite(t *testing.T, trialsLine string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "suite.hcl")
	src := "suite \"cli-suite\" {\n" + trialsLine + `
  task "task-a" {
    prompt = "do task a"
    judge {
      type    = "test-command"
      command = "true"
    }
  }

  task "task-b" {
    prompt = "do task b"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	must.NoError(os.WriteFile(path, []byte(src), 0o644))
	return path
}

func TestCLI_Run_RejectsOutOfRangeTrials(t *testing.T) {
	suite := writeTwoTaskSuite(t, "")
	cases := []struct {
		name    string
		trials  string
		wantMsg string
	}{
		{"zero", "0", "-trials must be at least 1, got 0"},
		{"negative", "-1", "-trials must be at least 1, got -1"},
		{"above the cap", "21", "-trials must be at most 20, got 21"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, stderr, code := runCLI(t, "run", "--suite", suite, "--dry-run", "--trials", tc.trials)
			must.Equal(1, code, "stderr:\n%s", stderr)
			must.Contains(stderr, tc.wantMsg)
		})
	}
}

func TestCLI_Run_AcceptsTheTrialsCap(t *testing.T) {
	must := require.New(t)
	stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, ""), "--dry-run", "--trials", "20")
	must.Equal(0, code, "stderr:\n%s", stderr)
	must.Contains(stdout, "Trials: 20 per task (40 harness runs planned")
}

func TestCLI_Run_DryRunPrintsPlannedRuns(t *testing.T) {
	must := require.New(t)
	stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, ""), "--dry-run", "--trials", "3")
	must.Equal(0, code, "stderr:\n%s", stderr)
	must.Contains(stdout, "Trials: 3 per task (6 harness runs planned; the dry run validates each task once)")
}

func TestCLI_Run_SuiteTrialsApplyWhenFlagIsUnset(t *testing.T) {
	t.Run("suite trials apply when the flag is unset", func(t *testing.T) {
		must := require.New(t)
		stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, "  trials = 2\n"), "--dry-run")
		must.Equal(0, code, "stderr:\n%s", stderr)
		must.Contains(stdout, "Trials: 2 per task (4 harness runs planned")
	})

	t.Run("explicit flag overrides suite trials", func(t *testing.T) {
		must := require.New(t)
		stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, "  trials = 2\n"), "--dry-run", "--trials", "1")
		must.Equal(0, code, "stderr:\n%s", stderr)
		must.NotContains(stdout, "Trials:", "an explicit --trials 1 should override the suite attribute")
	})
}

func TestCLI_Compare_FlagParseErrorExitsTwo(t *testing.T) {
	must := require.New(t)
	_, stderr, code := runCLI(t, "compare", "--no-such-flag")
	must.Equal(2, code, "stderr:\n%s", stderr)
}
