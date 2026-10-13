package executor

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startGrouped runs g's wrapper on the host in dir and returns a channel that
// reports the wrapper's exit once it has finished.
func startGrouped(t *testing.T, g groupedCommand, dir string, env []string) (stdout *strings.Builder, done <-chan error) {
	t.Helper()
	argv := g.runArgv()
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	out := &strings.Builder{}
	cmd.Stdout = out
	ch := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	go func() { ch <- cmd.Wait() }()
	return out, ch
}

func runKill(t *testing.T, g groupedCommand, env []string) {
	t.Helper()
	argv := g.killArgv()
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kill script: %v: %s", err, out)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was not written", path)
}

func assertWrapperKilledTree(t *testing.T, g groupedCommand, dir string, env []string) {
	t.Helper()
	// The detached grandchild holds no pipe; only a signal reaches it.
	g.command = "sh -c 'sleep 2; touch after-kill' >/dev/null 2>&1 & sleep 600"
	_, done := startGrouped(t, g, dir, env)
	waitForFile(t, g.pidFile)

	runKill(t, g, env)

	select {
	case err := <-done:
		var exitErr *osexec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Errorf("wrapper err = %v, want a non-zero exit after the kill", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper still running after the kill script")
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "after-kill")); err == nil {
		t.Error("grandchild survived the kill script and wrote its marker")
	}
	if _, err := os.Stat(g.pidFile); err == nil {
		t.Error("pid file was not removed")
	}
}

// skipUnlessProvided skips when a tool the test needs is missing, except on
// Linux CI where the tool is expected and its absence must fail the build.
func skipUnlessProvided(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func TestGroupedCommand_KillScriptKillsSessionWithSetsid(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		skipUnlessProvided(t, "setsid not available")
	}
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	if err != nil {
		t.Fatal(err)
	}
	g.pidFile = filepath.Join(dir, "pid")
	assertWrapperKilledTree(t, g, dir, os.Environ())
}

func TestGroupedCommand_KillScriptKillsDescendantsWithoutSetsid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant walk requires /proc")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "sleep", "touch", "rm"} {
		path, err := osexec.LookPath(tool)
		if err != nil {
			skipUnlessProvided(t, "%s not available", tool)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + bin}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	g, err := newGroupedCommand("/bin/sh", "")
	if err != nil {
		t.Fatal(err)
	}
	g.pidFile = filepath.Join(dir, "pid")
	assertWrapperKilledTree(t, g, work, env)
}

func TestGroupedCommand_PreservesExitStatusAndOutput(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "echo out; exit 7")
	if err != nil {
		t.Fatal(err)
	}
	g.pidFile = filepath.Join(dir, "pid")

	out, done := startGrouped(t, g, dir, os.Environ())
	var werr error
	select {
	case werr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper did not finish")
	}
	var exitErr *osexec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Errorf("wrapper err = %v, want exit status 7", werr)
	}
	if out.String() != "out\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "out\n")
	}
	if _, err := os.Stat(g.pidFile); err == nil {
		t.Error("pid file was not removed after the command finished")
	}
}

func TestGroupedCommand_KillScriptIgnoresBogusPidFile(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	if err != nil {
		t.Fatal(err)
	}
	g.pidFile = filepath.Join(dir, "pid")
	if err := os.WriteFile(g.pidFile, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runKill(t, g, os.Environ())
}

func TestGroupedCommand_KillScriptExitsNonZeroWithoutPidFile(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	if err != nil {
		t.Fatal(err)
	}
	g.pidFile = filepath.Join(dir, "missing")

	argv := g.killArgv()
	err = osexec.Command(argv[0], argv[1:]...).Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("kill script err = %v, want exit status 3", err)
	}
}

func TestGroupedCommand_KillScriptNeverSignalsPidZeroOrOne(t *testing.T) {
	for _, pid := range []string{"0", "1"} {
		t.Run(pid, func(t *testing.T) {
			dir := t.TempDir()
			g, err := newGroupedCommand("/bin/sh", "")
			if err != nil {
				t.Fatal(err)
			}
			g.pidFile = filepath.Join(dir, "pid")
			if err := os.WriteFile(g.pidFile, []byte(pid+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// kill is replaced by a recording function so a regression cannot
			// signal the host.
			signalLog := filepath.Join(dir, "signals")
			argv := g.killArgv()
			argv[2] = `kill() { echo "$*" >>"` + signalLog + `"; }
` + argv[2]
			if out, err := osexec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
				t.Fatalf("kill script: %v: %s", err, out)
			}
			if raw, err := os.ReadFile(signalLog); err == nil {
				t.Errorf("kill script signalled pid %s: %q", pid, raw)
			}
		})
	}
}

func TestNewGroupedCommand_UniquePidFiles(t *testing.T) {
	a, err := newGroupedCommand("sh", "true")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newGroupedCommand("sh", "true")
	if err != nil {
		t.Fatal(err)
	}
	if a.pidFile == b.pidFile {
		t.Errorf("pid files collide: %s", a.pidFile)
	}
	if got := a.runArgv(); got[len(got)-1] != "true" || got[0] != "sh" {
		t.Errorf("runArgv = %v, want the original command as the last argument", got)
	}
}
