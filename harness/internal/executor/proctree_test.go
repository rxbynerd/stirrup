package executor

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, cmd.Start(), "start wrapper")
	go func() { ch <- cmd.Wait() }()
	return out, ch
}

func runKill(t *testing.T, g groupedCommand, env []string) {
	t.Helper()
	argv := g.killArgv()
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "kill script output: %s", out)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(path)
		return err == nil && strings.TrimSpace(string(raw)) != ""
	}, 5*time.Second, 20*time.Millisecond, "%s was not written", path)
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
		if assert.ErrorAs(t, err, &exitErr, "wrapper err = %v, want a non-zero exit after the kill", err) {
			assert.NotZero(t, exitErr.ExitCode(), "wrapper err = %v, want a non-zero exit after the kill", err)
		}
	case <-time.After(10 * time.Second):
		require.FailNow(t, "wrapper still running after the kill script")
	}
	time.Sleep(3 * time.Second)
	assert.NoFileExists(t, filepath.Join(dir, "after-kill"), "grandchild survived the kill script and wrote its marker")
	assert.NoFileExists(t, g.pidFile, "pid file was not removed")
}

// skipUnlessProvided skips when a tool the test needs is missing, except on
// Linux CI where the tool is expected and its absence must fail the build.
func skipUnlessProvided(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		require.FailNow(t, fmt.Sprintf(format, args...))
	}
	t.Skipf(format, args...)
}

func TestGroupedCommand_KillScriptKillsSessionWithSetsid(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		skipUnlessProvided(t, "setsid not available")
	}
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	require.NoError(t, err)
	g.pidFile = filepath.Join(dir, "pid")
	assertWrapperKilledTree(t, g, dir, os.Environ())
}

func TestGroupedCommand_KillScriptKillsDescendantsWithoutSetsid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant walk requires /proc")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))
	for _, tool := range []string{"sh", "sleep", "touch", "rm"} {
		path, err := osexec.LookPath(tool)
		if err != nil {
			skipUnlessProvided(t, "%s not available", tool)
		}
		require.NoError(t, os.Symlink(path, filepath.Join(bin, tool)))
	}
	env := []string{"PATH=" + bin}
	work := filepath.Join(dir, "work")
	require.NoError(t, os.Mkdir(work, 0o755))
	g, err := newGroupedCommand("/bin/sh", "")
	require.NoError(t, err)
	g.pidFile = filepath.Join(dir, "pid")
	assertWrapperKilledTree(t, g, work, env)
}

func TestGroupedCommand_PreservesExitStatusAndOutput(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "echo out; exit 7")
	require.NoError(t, err)
	g.pidFile = filepath.Join(dir, "pid")

	out, done := startGrouped(t, g, dir, os.Environ())
	var werr error
	select {
	case werr = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "wrapper did not finish")
	}
	var exitErr *osexec.ExitError
	if assert.ErrorAs(t, werr, &exitErr, "wrapper err = %v, want exit status 7", werr) {
		assert.Equal(t, 7, exitErr.ExitCode())
	}
	assert.Equal(t, "out\n", out.String())
	assert.NoFileExists(t, g.pidFile, "pid file was not removed after the command finished")
}

func TestGroupedCommand_KillScriptIgnoresBogusPidFile(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	require.NoError(t, err)
	g.pidFile = filepath.Join(dir, "pid")
	require.NoError(t, os.WriteFile(g.pidFile, []byte("not-a-pid\n"), 0o600))
	runKill(t, g, os.Environ())
}

func TestGroupedCommand_KillScriptExitsNonZeroWithoutPidFile(t *testing.T) {
	dir := t.TempDir()
	g, err := newGroupedCommand("/bin/sh", "")
	require.NoError(t, err)
	g.pidFile = filepath.Join(dir, "missing")

	argv := g.killArgv()
	err = osexec.Command(argv[0], argv[1:]...).Run()
	var exitErr *osexec.ExitError
	require.ErrorAs(t, err, &exitErr, "kill script err = %v, want exit status 3", err)
	require.Equal(t, 3, exitErr.ExitCode())
}

func TestGroupedCommand_KillScriptNeverSignalsPidZeroOrOne(t *testing.T) {
	cases := []struct {
		name string
		pid  string
	}{
		{name: "pid zero", pid: "0"},
		{name: "pid one", pid: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			g, err := newGroupedCommand("/bin/sh", "")
			require.NoError(t, err)
			g.pidFile = filepath.Join(dir, "pid")
			require.NoError(t, os.WriteFile(g.pidFile, []byte(tc.pid+"\n"), 0o600))
			// kill is replaced by a recording function so a regression cannot
			// signal the host.
			signalLog := filepath.Join(dir, "signals")
			argv := g.killArgv()
			argv[2] = `kill() { echo "$*" >>"` + signalLog + `"; }
` + argv[2]
			out, err := osexec.Command(argv[0], argv[1:]...).CombinedOutput()
			require.NoError(t, err, "kill script output: %s", out)
			require.NoFileExists(t, signalLog, "kill script signalled pid %s", tc.pid)
		})
	}
}

func TestNewGroupedCommand_UniquePidFiles(t *testing.T) {
	a, err := newGroupedCommand("sh", "true")
	require.NoError(t, err)
	b, err := newGroupedCommand("sh", "true")
	require.NoError(t, err)
	assert.NotEqual(t, a.pidFile, b.pidFile, "pid files collide")
	got := a.runArgv()
	assert.Equal(t, "sh", got[0])
	assert.Equal(t, "true", got[len(got)-1], "runArgv = %v, want the original command as the last argument", got)
}
