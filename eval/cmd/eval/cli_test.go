package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // re-executes this test binary with test-controlled arguments
	cmd.Env = append(os.Environ(), cliSubprocessEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("running the CLI: %v", err)
	}
	return out.String(), errOut.String(), code
}

func writeTwoTaskSuite(t *testing.T, trialsLine string) string {
	t.Helper()
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
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLI_RunRejectsOutOfRangeTrials(t *testing.T) {
	suite := writeTwoTaskSuite(t, "")
	cases := []struct {
		trials  string
		wantMsg string
	}{
		{"0", "-trials must be at least 1, got 0"},
		{"-1", "-trials must be at least 1, got -1"},
		{"21", "-trials must be at most 20, got 21"},
	}
	for _, tc := range cases {
		t.Run(tc.trials, func(t *testing.T) {
			_, stderr, code := runCLI(t, "run", "--suite", suite, "--dry-run", "--trials", tc.trials)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("stderr missing %q:\n%s", tc.wantMsg, stderr)
			}
		})
	}
}

func TestCLI_RunAcceptsTheTrialsCap(t *testing.T) {
	stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, ""), "--dry-run", "--trials", "20")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, stderr)
	}
	if want := "Trials: 20 per task (" + strconv.Itoa(20*2) + " harness runs planned"; !strings.Contains(stdout, want) {
		t.Errorf("stdout missing %q:\n%s", want, stdout)
	}
}

func TestCLI_DryRunPrintsPlannedRuns(t *testing.T) {
	stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, ""), "--dry-run", "--trials", "3")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, stderr)
	}
	if want := "Trials: 3 per task (6 harness runs planned; the dry run validates each task once)"; !strings.Contains(stdout, want) {
		t.Errorf("stdout missing %q:\n%s", want, stdout)
	}
}

func TestCLI_SuiteTrialsApplyWhenFlagIsUnset(t *testing.T) {
	stdout, stderr, code := runCLI(t, "run", "--suite", writeTwoTaskSuite(t, "  trials = 2\n"), "--dry-run")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, stderr)
	}
	if want := "Trials: 2 per task (4 harness runs planned"; !strings.Contains(stdout, want) {
		t.Errorf("stdout missing %q:\n%s", want, stdout)
	}

	stdout, _, code = runCLI(t, "run", "--suite", writeTwoTaskSuite(t, "  trials = 2\n"), "--dry-run", "--trials", "1")
	if code != 0 || strings.Contains(stdout, "Trials:") {
		t.Errorf("an explicit --trials 1 should override the suite attribute: exit %d\n%s", code, stdout)
	}
}

func TestCLI_CompareFlagParseErrorExitsTwo(t *testing.T) {
	_, stderr, code := runCLI(t, "compare", "--no-such-flag")
	if code != 2 {
		t.Errorf("exit code = %d, want 2\n%s", code, stderr)
	}
}
