package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// injectionSandboxExecutor is a CanExec-without-HostPathWorkspace fake, like
// sandboxExecExecutor, but its Exec runs the constructed command through a
// genuine `sh -c` in a scratch directory instead of returning scripted
// output. Fabricated stdout can prove parsing correctness, but only a real
// shell can prove a payload was never reinterpreted as shell syntax — this
// is the harness for the injection regression suite below. The rg probe is
// forced to report absent so every payload deterministically exercises the
// grepViaShellGrep/findViaExec command construction regardless of whether
// this host happens to have rg on PATH.
type injectionSandboxExecutor struct {
	dir      string
	commands []string
}

func (e *injectionSandboxExecutor) ReadFile(context.Context, string) (string, error) {
	return "", fmt.Errorf("not used by these tests")
}

func (e *injectionSandboxExecutor) WriteFile(context.Context, string, string) error {
	return fmt.Errorf("write operations not supported")
}

func (e *injectionSandboxExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("not used by these tests")
}

func (e *injectionSandboxExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	e.commands = append(e.commands, command)
	if strings.HasPrefix(command, "rg") {
		return &executor.ExecResult{ExitCode: 127, Stderr: "rg: not found"}, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := osexec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = e.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := &executor.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *osexec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return nil, err
	}
}

func (e *injectionSandboxExecutor) ResolvePath(relativePath string) (string, error) {
	if relativePath == "." || relativePath == "" {
		return e.dir, nil
	}
	return filepath.Join(e.dir, relativePath), nil
}

func (e *injectionSandboxExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanExec: true, MaxTimeout: time.Minute}
}

// TestGrepFilesTool_SandboxExecRejectsShellInjection runs shell-metacharacter
// payloads through pattern/include/exclude/path and executes the resulting
// grep_files sandbox command via a genuine `sh -c`, proving shellQuote's
// escaping holds under real shell evaluation rather than a scripted fake.
// Each payload embeds a uniquely-named canary command; if quoting ever
// broke, the canary file would appear in the scratch directory.
func TestGrepFilesTool_SandboxExecRejectsShellInjection(t *testing.T) {
	type payload struct {
		name    string
		pattern string
		path    string
		include []string
		exclude []string
		canary  string
	}
	payloads := []payload{
		{
			name:    "single-quote breakout with semicolon+touch",
			pattern: "'; touch INJECTED_A; '",
			canary:  "INJECTED_A",
		},
		{
			name:    "backtick command substitution",
			pattern: "`touch INJECTED_B`",
			canary:  "INJECTED_B",
		},
		{
			name:    "dollar-paren command substitution",
			pattern: "$(touch INJECTED_C)",
			canary:  "INJECTED_C",
		},
		{
			name:    "leading dash argument injection",
			pattern: "-rf /",
			canary:  "INJECTED_D",
		},
		{
			name:    "embedded newline",
			pattern: "needle\ntouch INJECTED_E",
			canary:  "INJECTED_E",
		},
		{
			name:    "injection via include glob",
			pattern: "needle",
			include: []string{"'; touch INJECTED_F; '"},
			canary:  "INJECTED_F",
		},
		{
			name:    "injection via exclude glob",
			pattern: "needle",
			exclude: []string{"$(touch INJECTED_G)"},
			canary:  "INJECTED_G",
		},
		{
			name:    "injection via search path segment",
			pattern: "needle",
			path:    "'; touch INJECTED_H; '",
			canary:  "INJECTED_H",
		},
	}

	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "benign.txt"), []byte("needle\n"), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			fe := &injectionSandboxExecutor{dir: dir}

			grep := GrepFilesTool(fe)
			args := map[string]any{"pattern": p.pattern}
			if p.path != "" {
				args["path"] = p.path
			}
			if p.include != nil {
				args["include"] = p.include
			}
			if p.exclude != nil {
				args["exclude"] = p.exclude
			}
			input, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			// The tool call itself may legitimately error (a bad regex) or
			// return no matches — the payload is search input, not shell
			// syntax, either way. What must never happen is the canary
			// side effect firing.
			_, _ = invokeText(context.Background(), grep, input)

			if _, statErr := os.Stat(filepath.Join(dir, p.canary)); statErr == nil {
				t.Fatalf("shell injection fired: canary file %q was created", p.canary)
			}
			if len(fe.commands) == 0 {
				t.Fatal("expected at least one Exec command to have been issued")
			}
		})
	}
}

// TestFindFilesTool_SandboxExecRejectsShellInjection mirrors the grep_files
// suite for find_files. find_files' `find <dir> -type f` command only ever
// interpolates the resolved search directory — name/include/exclude are
// applied client-side in Go, never passed to the shell — so path is the one
// field that reaches a real shellQuote call; name/include/exclude payloads
// are included anyway as cheap defense-in-depth against a future change
// that starts building shell flags from them.
func TestFindFilesTool_SandboxExecRejectsShellInjection(t *testing.T) {
	type payload struct {
		name     string
		globName string
		path     string
		include  []string
		exclude  []string
		canary   string
	}
	payloads := []payload{
		{
			name:     "injection via search path segment",
			globName: "*.txt",
			path:     "'; touch INJECTED_I; '",
			canary:   "INJECTED_I",
		},
		{
			name:     "injection via name glob (not shell-interpolated)",
			globName: "'; touch INJECTED_J; '",
			canary:   "INJECTED_J",
		},
		{
			name:     "injection via include glob (not shell-interpolated)",
			globName: "*.txt",
			include:  []string{"$(touch INJECTED_K)"},
			canary:   "INJECTED_K",
		},
	}

	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "benign.txt"), []byte("x"), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			fe := &injectionSandboxExecutor{dir: dir}

			find := FindFilesTool(fe)
			args := map[string]any{"name": p.globName}
			if p.path != "" {
				args["path"] = p.path
			}
			if p.include != nil {
				args["include"] = p.include
			}
			if p.exclude != nil {
				args["exclude"] = p.exclude
			}
			input, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			_, _ = invokeText(context.Background(), find, input)

			if _, statErr := os.Stat(filepath.Join(dir, p.canary)); statErr == nil {
				t.Fatalf("shell injection fired: canary file %q was created", p.canary)
			}
		})
	}
}
