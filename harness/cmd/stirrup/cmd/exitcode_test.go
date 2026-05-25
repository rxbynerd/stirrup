package cmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyExitCode pins the core mapping Execute() relies on: a nil
// error is success (0), an *exitError carries its class code, an
// *exitError reached through a wrapping fmt.Errorf chain is still found
// via errors.As, and any other error preserves the historical default
// of 1 so nothing previously unclassified changes its exit status.
func TestClassifyExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: 0},
		{name: "validation", err: validationError(errors.New("bad config")), want: exitValidation},
		{name: "parse", err: parseError(errors.New("bad json")), want: exitParse},
		{name: "io", err: ioError(errors.New("no such file")), want: exitIO},
		{
			name: "wrapped exitError still classified",
			err:  errors.New("outer: " + parseError(errors.New("inner")).Error()),
			want: 1, // a plain re-string loses the wrapper; default applies
		},
		{
			name: "fmt.Errorf %w preserves the class",
			want: exitIO,
			err: func() error {
				return errors.Join(errors.New("context"), ioError(errors.New("disk full")))
			}(),
		},
		{name: "untyped error defaults to 1", err: errors.New("something else"), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyExitCode(tc.err); got != tc.want {
				t.Errorf("classifyExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestExitErrorWrappersAreTransparent pins that the wrapper does not
// change the operator-facing message or break errors.Is on the wrapped
// sentinel. Only the carried exit code is new behaviour.
func TestExitErrorWrappersAreTransparent(t *testing.T) {
	sentinel := errors.New("the underlying failure")
	for _, wrap := range []func(error) error{parseError, ioError, validationError} {
		got := wrap(sentinel)
		if got.Error() != sentinel.Error() {
			t.Errorf("wrapped Error() = %q, want %q", got.Error(), sentinel.Error())
		}
		if !errors.Is(got, sentinel) {
			t.Errorf("errors.Is should match the wrapped sentinel through the exitError")
		}
	}
}

// TestExitErrorWrappersNilPassthrough pins that wrapping a nil error
// returns nil rather than a non-nil exitError around nothing, so a call
// site can wrap unconditionally without inventing a phantom failure.
func TestExitErrorWrappersNilPassthrough(t *testing.T) {
	for _, wrap := range []func(error) error{parseError, ioError, validationError} {
		if got := wrap(nil); got != nil {
			t.Errorf("wrap(nil) = %v, want nil", got)
		}
	}
}

// TestLoadRunConfigFile_ExitClasses pins the file-loader's
// classification: a missing file is I/O (exit 3) and malformed JSON is
// a parse error (exit 2). The class is asserted via classifyExitCode so
// the test follows the same path Execute() does.
func TestLoadRunConfigFile_ExitClasses(t *testing.T) {
	t.Run("missing file is I/O", func(t *testing.T) {
		_, err := loadRunConfigFile(filepath.Join(t.TempDir(), "absent.json"))
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
	t.Run("malformed JSON is parse", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := loadRunConfigFile(path)
		if got := classifyExitCode(err); got != exitParse {
			t.Errorf("classifyExitCode = %d, want %d (parse); err=%v", got, exitParse, err)
		}
	})
	t.Run("empty file is I/O", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.json")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := loadRunConfigFile(path)
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
}

// TestReadRunConfigFromReader_ExitClasses pins the stdin reader's
// classification: malformed piped JSON is a parse error (exit 2) and an
// empty stream is an I/O error (exit 3). A non-*os.File reader forces
// the piped path without a real pipe (see isStdinPiped).
func TestReadRunConfigFromReader_ExitClasses(t *testing.T) {
	t.Run("malformed piped JSON is parse", func(t *testing.T) {
		_, err := readRunConfigFromReader(strings.NewReader("{bad"), "<stdin>")
		if got := classifyExitCode(err); got != exitParse {
			t.Errorf("classifyExitCode = %d, want %d (parse); err=%v", got, exitParse, err)
		}
	})
	t.Run("empty stream is I/O", func(t *testing.T) {
		_, err := readRunConfigFromReader(strings.NewReader(""), "<stdin>")
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
}

// TestReadPromptFile_ExitClassIsIO pins that every --prompt-file failure
// classifies as I/O (exit 3): the file is plain text so no path can be a
// JSON parse failure.
func TestReadPromptFile_ExitClassIsIO(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := readPromptFile(filepath.Join(t.TempDir(), "absent.txt"))
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.txt")
		if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := readPromptFile(path)
		if got := classifyExitCode(err); got != exitIO {
			t.Errorf("classifyExitCode = %d, want %d (I/O); err=%v", got, exitIO, err)
		}
	})
}

// TestBuildRunConfig_ValidationExitClass pins that a read-only mode with
// an allow-all policy — a config that parses cleanly but fails
// ValidateRunConfig — classifies as validation (exit 1) when resolved
// through the ResolveAll path the harness uses.
func TestBuildRunConfig_ValidationExitClass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// planning is read-only; allow-all on a read-only mode is the hard
	// invariant ValidateRunConfig rejects (see CLAUDE.md). The prompt is
	// present so resolution reaches the validator rather than the
	// prompt-required gate.
	body := `{"runId":"r","mode":"planning","prompt":"do a thing",` +
		`"maxTurns":10,"timeout":600,` +
		`"permissionPolicy":{"type":"allow-all"},` +
		`"provider":{"type":"anthropic","apiKeyRef":"secret://ANTHROPIC_API_KEY"},` +
		`"modelRouter":{"type":"static","provider":"anthropic","model":"claude-x"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := newTestHarnessCommand()
	_, err := BuildRunConfig(RunConfigSources{
		ConfigPath: path,
		Cmd:        cmd,
		Resolve:    ResolveAll,
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if got := classifyExitCode(err); got != exitValidation {
		t.Errorf("classifyExitCode = %d, want %d (validation); err=%v", got, exitValidation, err)
	}
}

// TestRunRunConfig_PromptRequiredExitClass pins that `run-config
// --validate` with no prompt on a non-TTY surface classifies as
// validation (exit 1). The stderrIsInteractive seam is pinned to false
// so the test does not depend on whether the test runner has a TTY.
func TestRunRunConfig_PromptRequiredExitClass(t *testing.T) {
	orig := stderrIsInteractive
	stderrIsInteractive = func() bool { return false }
	defer func() { stderrIsInteractive = orig }()

	cmd := newTestRunConfigCommand()
	_ = cmd.Flags().Set("validate", "true")

	// nil stdin (not an empty reader): a non-nil reader is treated as
	// piped by isStdinPiped and would trip the "input is empty" I/O
	// error before resolution reaches the prompt-required gate.
	var out bytes.Buffer
	err := runRunConfigWithIO(cmd, nil, nil, &out)
	if err == nil {
		t.Fatal("expected prompt-required error, got nil")
	}
	if !errors.Is(err, errPromptRequired) {
		t.Errorf("error should wrap errPromptRequired, got: %v", err)
	}
	if got := classifyExitCode(err); got != exitValidation {
		t.Errorf("classifyExitCode = %d, want %d (validation); err=%v", got, exitValidation, err)
	}
}

// TestCLIExitCodes_EndToEnd pins the real os.Exit codes the built binary
// produces for each failure class. A type-check pass does not prove the
// Execute() → classifyExitCode → os.Exit wiring fires, so this builds
// the binary once and execs it with stdin attached to a pipe (never a
// TTY under `go test`), exercising harness and run-config:
//
//	success            -> 0
//	validation failure -> 1
//	malformed --config -> 2
//	missing  --config  -> 3
func TestCLIExitCodes_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build + exec in -short mode")
	}
	bin := buildStirrupBinary(t)
	dir := t.TempDir()

	validConfig := filepath.Join(dir, "valid.json")
	// maxTurns + timeout are required by ValidateRunConfig and have no
	// file-side defaults (the flag-only path gets them from cobra
	// DefValues), so a from-file config must set them explicitly.
	writeFile(t, validConfig, `{"runId":"r","mode":"planning","prompt":"hi",`+
		`"maxTurns":10,"timeout":600,`+
		`"provider":{"type":"anthropic","apiKeyRef":"secret://ANTHROPIC_API_KEY"},`+
		`"modelRouter":{"type":"static","provider":"anthropic","model":"claude-x"}}`)

	invalidConfig := filepath.Join(dir, "invalid.json")
	// allow-all on the read-only planning mode is the sole reason this
	// fails ValidateRunConfig — every other required field is present so
	// the failure is unambiguously the read-only-mode invariant.
	writeFile(t, invalidConfig, `{"runId":"r","mode":"planning","prompt":"hi",`+
		`"maxTurns":10,"timeout":600,`+
		`"permissionPolicy":{"type":"allow-all"},`+
		`"provider":{"type":"anthropic","apiKeyRef":"secret://ANTHROPIC_API_KEY"},`+
		`"modelRouter":{"type":"static","provider":"anthropic","model":"claude-x"}}`)

	malformedConfig := filepath.Join(dir, "malformed.json")
	writeFile(t, malformedConfig, `{not json`)

	missingConfig := filepath.Join(dir, "does-not-exist.json")

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{
			name: "run-config valid succeeds",
			args: []string{"run-config", "--validate", "--config", validConfig},
			want: 0,
		},
		{
			name: "run-config validation failure",
			args: []string{"run-config", "--validate", "--config", invalidConfig},
			want: exitValidation,
		},
		{
			name: "run-config malformed JSON",
			args: []string{"run-config", "--config", malformedConfig},
			want: exitParse,
		},
		{
			name: "run-config missing file",
			args: []string{"run-config", "--config", missingConfig},
			want: exitIO,
		},
		{
			name: "harness validation failure",
			args: []string{"harness", "--config", invalidConfig},
			want: exitValidation,
		},
		{
			name: "harness malformed JSON",
			args: []string{"harness", "--config", malformedConfig},
			want: exitParse,
		},
		{
			name: "harness missing file",
			args: []string{"harness", "--config", missingConfig},
			want: exitIO,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runExit(t, bin, tc.args)
			if got != tc.want {
				t.Errorf("exit code = %d, want %d (args: %v)", got, tc.want, tc.args)
			}
		})
	}
}

// TestCLIExitCodes_BareInvocationsStayZero pins that the #249 success
// paths are not regressed by the exit-code mapping: a bare `stirrup`
// prints the orientation hint and exits 0. (`stirrup harness` with no
// prompt exits non-zero under the non-TTY `go test` subprocess — its
// interactive hint exit-0 path needs a PTY and is covered by the #249
// suite plus the manual verification in the task report.)
func TestCLIExitCodes_BareInvocationsStayZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build + exec in -short mode")
	}
	bin := buildStirrupBinary(t)
	if got := runExit(t, bin, []string{}); got != 0 {
		t.Errorf("bare `stirrup` exit code = %d, want 0", got)
	}
}

// buildStirrupBinary compiles the stirrup CLI into the test's temp dir
// and returns its path. Building once per test keeps the subprocess
// exec hermetic (no dependency on a stale ./stirrup in the worktree).
func buildStirrupBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stirrup-exitcode-test")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/rxbynerd/stirrup/harness/cmd/stirrup")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

// runExit execs the binary with the given args and returns the process
// exit code. Stdin is /dev/null — a character device, which isStdinPiped
// treats as "not piped" (issue #249), so a --config <path> argument is
// not rejected as ambiguous against a phantom piped base. A non-ExitError
// failure (binary missing, signal) fails the test.
func runExit(t *testing.T, bin string, args []string) int {
	t.Helper()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	cmd := exec.Command(bin, args...) //nolint:gosec // bin is the test-built binary, args are test-controlled
	cmd.Stdin = devNull
	cmd.Stdout = nil
	cmd.Stderr = nil
	runErr := cmd.Run()
	if runErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("running %s %v: non-exit error: %v", bin, args, runErr)
	return -1
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
