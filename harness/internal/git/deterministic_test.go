package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

const (
	testSHA1   = "0123456789abcdef0123456789abcdef01234567"
	testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// emptyHostPath leaves the test process with a PATH holding no binaries,
// so any attempt to start a host process (sh, git) fails outright.
func emptyHostPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestDeterministic_EveryCommandGoesThroughExecutor(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: testSHA1, branch: "stirrup/run-1", staged: true}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)
	ctx := context.Background()

	must.NoError(gs.Setup(ctx, "/ignored/host/path", "run-1"))
	must.NoError(gs.Checkpoint(ctx, "Turn 1: 2 tool calls"))
	result, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	want := types.GitHandoff{Branch: "stirrup/run-1", SHA: testSHA1, BaseSHA: testSHA1}
	must.Equal(want, *result)

	wantSequence := [][]string{
		{"pwd -P"},
		{"rev-parse", "--show-toplevel"},
		{"checkout", "-b", "stirrup/run-1"},
		{"rev-parse", "--verify", "--quiet", "HEAD"},
		{"add", "-A"},
		{"diff", "--cached", "--quiet"},
		{"commit", "--no-verify", "-m", "Turn 1: 2 tool calls"},
		{"add", "-A"},
		{"diff", "--cached", "--quiet"},
		{"commit", "--no-verify", "-m", "Final checkpoint (outcome: success)"},
		{"rev-parse", "HEAD"},
		{"rev-parse", "--abbrev-ref", "HEAD"},
		{"status", "--porcelain"},
	}
	must.Len(exec.calls, len(wantSequence), "executor commands:\n%s", strings.Join(exec.calls, "\n"))
	for i, want := range wantSequence {
		t.Run(fmt.Sprintf("%d %s", i, strings.Join(want, " ")), func(t *testing.T) {
			must := require.New(t)
			got := exec.calls[i]
			if i == 0 {
				must.Equal(want[0], got)
				return
			}
			must.True(strings.HasPrefix(got, "git "), "not a git command: %q", got)
			must.True(hasArgs(got, want...), "call %q lacks arguments %q", got, want)
			for j := 0; j < len(HardeningFlags); j += 2 {
				must.True(hasArgs(got, HardeningFlags[j], HardeningFlags[j+1]),
					"call lacks hardening flag %s %s: %q", HardeningFlags[j], HardeningFlags[j+1], got)
			}
			must.True(hasArgs(got, "-c", "user.name="+HarnessIdentityName), "call lacks the harness author name: %q", got)
			must.True(hasArgs(got, "-c", "user.email="+HarnessIdentityEmail), "call lacks the harness author email: %q", got)
			must.True(hasArgs(got, "-c", "safe.directory=/workspace"), "call lacks safe.directory for the workspace root: %q", got)
		})
	}
}

func TestSetup_RejectsEnclosingRepository(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{root: "/parent/ws", toplevel: "/parent"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)

	err := gs.Setup(context.Background(), "", "run-2")
	must.ErrorContains(err, "/parent/ws is inside repository /parent")

	var mutations []string
	for _, call := range exec.calls {
		for _, mutating := range []string{"checkout", "init", "add", "commit"} {
			if hasArgs(call, mutating) {
				mutations = append(mutations, call)
			}
		}
	}
	must.Empty(mutations, "Setup issued mutating commands after detecting the enclosing repository")

	must.Error(gs.Checkpoint(context.Background(), "x"), "Checkpoint succeeded after a failed Setup")
	_, err = gs.Finalise(context.Background(), "success")
	must.ErrorContains(err, "has not run")
}

func TestSetup_InitialisesNonRepository(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", notRepo: true, sha: testSHA1, branch: "stirrup/run-3"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)

	must.NoError(gs.Setup(context.Background(), "", "run-3"))
	must.True(containsCall(exec.calls, "test -e .git || test -L .git"),
		"Setup initialised without checking for a .git entry:\n%s", strings.Join(exec.calls, "\n"))
	want := [][]string{
		{"rev-parse", "--show-toplevel"},
		{"init", "--quiet"},
		{"rev-parse", "--show-toplevel"},
		{"add", "-A"},
		{"commit", "--no-verify", "--allow-empty", "-m", "Initial workspace state"},
		{"checkout", "-b", "stirrup/run-3"},
		{"rev-parse", "--verify", "--quiet", "HEAD"},
	}
	gitCalls := gitCommands(exec.calls)
	must.Len(gitCalls, len(want), "git commands:\n%s", strings.Join(gitCalls, "\n"))
	for i, args := range want {
		t.Run(fmt.Sprintf("%d %s", i, strings.Join(args, " ")), func(t *testing.T) {
			must := require.New(t)
			must.True(hasArgs(gitCalls[i], args...), "got %q", gitCalls[i])
		})
	}
}

func TestSetup_RefusesUnresolvableGitEntry(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{
		root:          "/workspace",
		notRepo:       true,
		notRepoStderr: "fatal: not a git repository: /Users/op/src/app/.git/worktrees/feature\n",
		gitEntry:      true,
	}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)

	err := gs.Setup(context.Background(), "", "run-3b")
	must.ErrorContains(err, "self-contained repository")

	var mutations []string
	for _, call := range exec.calls {
		for _, mutating := range []string{"init", "checkout", "add", "commit"} {
			if hasArgs(call, mutating) {
				mutations = append(mutations, call)
			}
		}
	}
	must.Empty(mutations, "Setup issued mutating commands despite an unresolvable .git entry")

	must.Error(gs.Checkpoint(context.Background(), "x"), "Checkpoint succeeded after a failed Setup")
}

func TestSetup_KeepsTrailingSpaceInWorkspaceRoot(t *testing.T) {
	emptyHostPath(t)
	for name, repo := range map[string]*fakeRepo{
		"existing repository": {root: "/work space ", toplevel: "/work space "},
		"initialised":         {root: "/work space ", notRepo: true},
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
			must.NoError(gs.Setup(context.Background(), "", "run-3c"))
		})
	}
}

func TestSetup_ReportsMissingGit(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", gitMissing: true}
	gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))

	err := gs.Setup(context.Background(), "", "run-4")
	must.ErrorContains(err, "git is not available in the sandbox")
}

func TestSetup_RejectsExecutorWithoutExec(t *testing.T) {
	must := require.New(t)
	exec := newScriptedExecutor(func(string) (*executor.ExecResult, error) {
		return nil, fmt.Errorf("must not be called")
	})
	exec.caps.CanExec = false
	gs := NewDeterministicGitStrategy(exec)
	must.Error(gs.Setup(context.Background(), "", "run-5"), "Setup succeeded on an executor that cannot run commands")
	must.Empty(exec.calls, "executor must not be called")
	must.Error(NewDeterministicGitStrategy(nil).Setup(context.Background(), "", "run-5"), "Setup succeeded with a nil executor")
}

func TestSetup_RejectsUnusableWorkspaceRoot(t *testing.T) {
	for _, pwd := range []string{"", "relative/path\n", "/ws\nextra\n"} {
		t.Run(fmt.Sprintf("pwd %q", pwd), func(t *testing.T) {
			must := require.New(t)
			exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
				if command == "pwd -P" {
					return &executor.ExecResult{Stdout: pwd}, nil
				}
				return &executor.ExecResult{}, nil
			})
			gs := NewDeterministicGitStrategy(exec)
			must.Error(gs.Setup(context.Background(), "", "run-6"))
			must.Len(exec.calls, 1, "only the pwd command should run")
		})
	}
}

func TestSetup_RejectsTaskIDThatIsNotABranchName(t *testing.T) {
	must := require.New(t)
	exec := newScriptedExecutor(func(string) (*executor.ExecResult, error) {
		return nil, fmt.Errorf("must not be called")
	})
	gs := NewDeterministicGitStrategy(exec)
	for _, id := range []string{"", "a..b", "x y", "lock.lock", "a\nb"} {
		t.Run(fmt.Sprintf("task id %q", id), func(t *testing.T) {
			must := require.New(t)
			must.Error(gs.Setup(context.Background(), "", id))
		})
	}
	must.Empty(exec.calls, "executor must not be called")
}

func TestCheckpoint_NoChanges_IssuesNoCommit(t *testing.T) {
	must := require.New(t)
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)
	ctx := context.Background()
	must.NoError(gs.Setup(ctx, "", "run-7"))
	must.NoError(gs.Checkpoint(ctx, "nothing"))

	var commits []string
	for _, call := range exec.calls {
		if hasArgs(call, "commit") {
			commits = append(commits, call)
		}
	}
	must.Empty(commits, "Checkpoint committed with nothing staged")
}

func TestFinalise_RejectsHostileOutput(t *testing.T) {
	cases := []struct {
		name   string
		sha    string
		branch string
	}{
		{"sha with shell syntax", "abc; rm -rf /", "stirrup/x"},
		{"short sha", "abc123", "stirrup/x"},
		{"uppercase sha", strings.ToUpper(testSHA1), "stirrup/x"},
		{"multi-line sha", testSHA1 + "\nevil", "stirrup/x"},
		{"branch with newline", testSHA1, "stirrup/x\nevil"},
		{"branch option injection", testSHA1, "--upload-pack=/bin/sh"},
		{"branch with dotdot", testSHA1, "a..b"},
		{"branch with space", testSHA1, "a b"},
		{"empty branch", testSHA1, ""},
		{"detached HEAD", testSHA1, "HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: tc.sha, branch: tc.branch}
			gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
			ctx := context.Background()
			must.NoError(gs.Setup(ctx, "", "run-8"))
			result, err := gs.Finalise(ctx, "success")
			must.Error(err, "Finalise accepted %+v", result)
		})
	}
}

func TestFinalise_AcceptsSHA256(t *testing.T) {
	must := require.New(t)
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: testSHA256, branch: "stirrup/run-9"}
	gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
	ctx := context.Background()
	must.NoError(gs.Setup(ctx, "", "run-9"))
	result, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	must.Equal(testSHA256, result.SHA)
}

func TestFinalise_BeforeSetupFails(t *testing.T) {
	must := require.New(t)
	gs := NewDeterministicGitStrategy(newScriptedExecutor((&fakeRepo{}).respond))
	handoff, err := gs.Finalise(context.Background(), "success")
	must.Error(err, "Finalise succeeded before Setup")
	must.NotNil(handoff, "Finalise before Setup returned a nil handoff")
}

func TestProbe_ReportsGitAvailability(t *testing.T) {
	cases := []struct {
		name    string
		res     *executor.ExecResult
		wantErr string
	}{
		{"git present", &executor.ExecResult{Stdout: "git version 2.54.0\n"}, ""},
		{"git missing", &executor.ExecResult{ExitCode: 127, Stderr: "sh: git: not found"}, "git is not available in the sandbox"},
		{"unexpected output", &executor.ExecResult{Stdout: "something else"}, "unexpected output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var issued []string
			exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
				issued = append(issued, command)
				return tc.res, nil
			})
			err := NewDeterministicGitStrategy(exec).Probe(context.Background())
			for _, command := range issued {
				must.True(hasArgs(command, "--version"), "Probe issued %q, want git --version", command)
			}
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestValidBranchName_AcceptsValidAndRejectsInvalid(t *testing.T) {
	valid := []string{"main", "stirrup/run-1", "feature/a.b", "a/b/c", "HEAD", "v1.0"}
	for _, name := range valid {
		t.Run(fmt.Sprintf("accepts %q", name), func(t *testing.T) {
			must := require.New(t)
			must.True(ValidBranchName(name))
		})
	}
	invalid := []string{
		"", "@", "-x", "/x", "x/", "x.", ".x", "a/.b", "a..b", "a//b", "a@{b", "a.lock", "a.lock/b",
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a\x01b", "a\x7fb", "a\nb",
		strings.Repeat("x", maxBranchNameLen+1),
	}
	for _, name := range invalid {
		t.Run(fmt.Sprintf("rejects %q", name), func(t *testing.T) {
			must := require.New(t)
			must.False(ValidBranchName(name))
		})
	}
}

func TestCommandLine_QuotesEveryArgument(t *testing.T) {
	line := CommandLine("commit", "-m", "it's; rm -rf /")
	t.Run("begins with hardening flags", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasPrefix(line, "git '-c' 'core.hooksPath=/dev/null'"), "CommandLine = %q", line)
	})
	t.Run("quotes trailing arguments", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasSuffix(line, `'commit' '-m' 'it'\''s; rm -rf /'`), "CommandLine = %q", line)
	})
	t.Run("keeps the semicolon inside quotes", func(t *testing.T) {
		must := require.New(t)
		must.Equal(1, strings.Count(line, ";"), "CommandLine = %q", line)
	})
}

// The tests below drive the strategy through a LocalExecutor against real
// repositories. Fixtures are prepared with the host's git; the strategy
// itself still only sees the executor.

func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %v\n%s", args, out)
	return strings.TrimSpace(string(out))
}

func initHostRepo(t *testing.T, dir string) {
	t.Helper()
	hostGit(t, dir, "init", "-q")
	hostGit(t, dir, "config", "user.email", "test@test.com")
	hostGit(t, dir, "config", "user.name", "Test")
	hostGit(t, dir, "commit", "--allow-empty", "-q", "-m", "initial")
}

func localStrategy(t *testing.T, dir string) *DeterministicGitStrategy {
	t.Helper()
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err)
	return NewDeterministicGitStrategy(exec)
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	must := require.New(t)
	var n int
	_, err := fmt.Sscanf(hostGit(t, dir, "rev-list", "--count", "HEAD"), "%d", &n)
	must.NoError(err, "parse commit count")
	return n
}

func TestLocal_SetupCheckpointFinalise_CommitsToTaskBranch(t *testing.T) {
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	gs := localStrategy(t, repo)
	ctx := context.Background()

	must.NoError(gs.Setup(ctx, repo, "task-42"))
	must.Equal("stirrup/task-42", hostGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))

	before := commitCount(t, repo)
	must.NoError(gs.Checkpoint(ctx, "nothing"))
	must.Equal(before, commitCount(t, repo), "checkpoint with no changes must not commit")

	must.NoError(os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello"), 0o644))
	must.NoError(gs.Checkpoint(ctx, "add hello"))
	must.Equal("add hello", hostGit(t, repo, "log", "-1", "--format=%s"))

	result, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	must.Equal("stirrup/task-42", result.Branch)
	must.Equal(hostGit(t, repo, "rev-parse", "HEAD"), result.SHA)
}

func TestLocal_CommitsWithHarnessIdentityWhenNoneConfigured(t *testing.T) {
	must := require.New(t)
	// An empty HOME and XDG_CONFIG_HOME leave git with no global identity;
	// the local executor forwards both to the child.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	hostGit(t, repo, "init", "-q")
	hostGit(t, repo, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-q", "-m", "initial")

	gs := localStrategy(t, repo)
	ctx := context.Background()
	must.NoError(gs.Setup(ctx, repo, "identity"))
	must.NoError(os.WriteFile(filepath.Join(repo, "f.txt"), []byte("data"), 0o644))
	must.NoError(gs.Checkpoint(ctx, "checkpoint"))
	want := HarnessIdentityName + " <" + HarnessIdentityEmail + ">"
	must.Equal(want, hostGit(t, repo, "log", "-1", "--format=%an <%ae>"), "author")
	must.Equal(want, hostGit(t, repo, "log", "-1", "--format=%cn <%ce>"), "committer")
}

// TestLocal_PlantedRepositoryConfigDoesNotExecute protects the hardening
// flags: hooks, core.hooksPath, core.fsmonitor and commit signing planted in
// the repository must not run during a checkpoint.
func TestLocal_PlantedRepositoryConfigDoesNotExecute(t *testing.T) {
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	markers := t.TempDir()
	marker := func(name string) string { return filepath.Join(markers, name) }

	hook := "#!/bin/sh\ntouch " + marker("hook") + "\n"
	hooksDir := filepath.Join(repo, ".git", "hooks")
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-index-change", "reference-transaction"} {
		must.NoError(os.WriteFile(filepath.Join(hooksDir, name), []byte(hook), 0o755))
	}
	plantedHooks := filepath.Join(repo, "planted-hooks")
	must.NoError(os.MkdirAll(plantedHooks, 0o755))
	must.NoError(os.WriteFile(filepath.Join(plantedHooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker("hookspath")+"\n"), 0o755))
	fakeGPG := filepath.Join(repo, "fake-gpg")
	must.NoError(os.WriteFile(fakeGPG, []byte("#!/bin/sh\ntouch "+marker("gpg")+"\nexit 1\n"), 0o755))
	hostGit(t, repo, "config", "core.hooksPath", plantedHooks)
	hostGit(t, repo, "config", "core.fsmonitor", "touch "+marker("fsmonitor")+"; echo /")
	hostGit(t, repo, "config", "commit.gpgsign", "true")
	hostGit(t, repo, "config", "gpg.program", fakeGPG)

	// Control: an unhardened commit must trigger the planted hook, or the
	// absence of markers below would prove nothing.
	hostGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", "control")
	_, err := os.Stat(marker("hookspath"))
	must.NoError(err, "control commit did not run the planted core.hooksPath hook")
	must.NoError(os.RemoveAll(markers))
	must.NoError(os.MkdirAll(markers, 0o755))

	gs := localStrategy(t, repo)
	ctx := context.Background()
	must.NoError(gs.Setup(ctx, repo, "planted"))
	must.NoError(os.WriteFile(filepath.Join(repo, "change.txt"), []byte("x"), 0o644))
	must.NoError(gs.Checkpoint(ctx, "checkpoint"))
	_, err = gs.Finalise(ctx, "success")
	must.NoError(err)

	entries, err := os.ReadDir(markers)
	must.NoError(err)
	var executed []string
	for _, e := range entries {
		executed = append(executed, e.Name())
	}
	must.Empty(executed, "planted payloads executed during the checkpoint")
}

func TestLocal_SetupRejectsWorkspaceInsideRepository(t *testing.T) {
	must := require.New(t)
	parent := t.TempDir()
	initHostRepo(t, parent)
	ws := filepath.Join(parent, "ws")
	must.NoError(os.MkdirAll(ws, 0o755))
	must.NoError(os.WriteFile(filepath.Join(parent, "unrelated.txt"), []byte("operator work"), 0o644))
	branchBefore := hostGit(t, parent, "rev-parse", "--abbrev-ref", "HEAD")
	commitsBefore := commitCount(t, parent)

	gs := localStrategy(t, ws)
	err := gs.Setup(context.Background(), ws, "nested")
	must.ErrorContains(err, "inside repository")
	must.Equal(branchBefore, hostGit(t, parent, "rev-parse", "--abbrev-ref", "HEAD"), "enclosing repository branch changed")
	must.Equal(commitsBefore, commitCount(t, parent), "enclosing repository commit count changed")
	must.Contains(hostGit(t, parent, "status", "--porcelain"), "unrelated.txt",
		"enclosing repository's uncommitted work was staged or committed")
	_, err = os.Stat(filepath.Join(ws, ".git"))
	must.ErrorIs(err, os.ErrNotExist, "Setup created a repository inside the nested workspace")
}

func TestLocal_SetupRefusesWorktreeWithUnreachableGitdir(t *testing.T) {
	must := require.New(t)
	ws := t.TempDir()
	gitFile := filepath.Join(ws, ".git")
	const pointer = "gitdir: /nonexistent/host/repo/.git/worktrees/feature\n"
	must.NoError(os.WriteFile(gitFile, []byte(pointer), 0o644))
	gs := localStrategy(t, ws)
	err := gs.Setup(context.Background(), ws, "worktree")
	must.ErrorContains(err, "self-contained repository")
	got, err := os.ReadFile(gitFile)
	must.NoError(err)
	must.Equal(pointer, string(got), ".git file changed")
}

func TestLocal_SetupInitialisesPlainDirectory(t *testing.T) {
	must := require.New(t)
	ws := t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("seed"), 0o644))
	gs := localStrategy(t, ws)
	ctx := context.Background()
	must.NoError(gs.Setup(ctx, ws, "fresh"))
	must.Equal("stirrup/fresh", hostGit(t, ws, "rev-parse", "--abbrev-ref", "HEAD"))
	must.Equal("seed.txt", hostGit(t, ws, "ls-files"), "base commit tracked files")
	result, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	must.True(ValidSHA(result.SHA), "Finalise SHA = %q", result.SHA)
}

func gitCommands(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "git ") {
			out = append(out, c)
		}
	}
	return out
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func TestLocal_Finalise_CommitsOutstandingWork(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()
	base := hostGit(t, repo, "rev-parse", "HEAD")

	gs := localStrategy(t, repo)
	must.NoError(gs.Setup(ctx, repo, "task-final"))
	must.NoError(os.WriteFile(filepath.Join(repo, "partial.txt"), []byte("partial"), 0o644))

	handoff, err := gs.Finalise(ctx, "budget_exceeded")
	must.NoError(err)

	head := hostGit(t, repo, "rev-parse", "HEAD")
	must.NotEqual(base, head, "HEAD did not move; outstanding work was not committed")
	check.Equal("Final checkpoint (outcome: budget_exceeded)", hostGit(t, repo, "log", "-1", "--format=%s"))
	check.Equal("partial.txt", hostGit(t, repo, "ls-tree", "--name-only", "HEAD", "partial.txt"), "partial.txt in HEAD tree")
	check.Equal(types.GitHandoff{Branch: "stirrup/task-final", SHA: head, BaseSHA: base}, *handoff)
}

func TestLocal_Finalise_CleanTreeReportsHEADWithoutCommit(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()

	gs := localStrategy(t, repo)
	must.NoError(gs.Setup(ctx, repo, "task-clean"))
	before := commitCount(t, repo)

	handoff, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	check.Equal(before, commitCount(t, repo), "commit count on a clean tree")
	check.Equal(handoff.BaseSHA, handoff.SHA, "SHA is the base commit")
	check.NotEmpty(handoff.SHA)
	check.False(handoff.Dirty, "Dirty on a clean tree")
}

func TestLocal_Identity_NoneConfiguredCommitsAsHarness(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	isolateGitConfig(t)
	dir := t.TempDir()
	hostGit(t, dir, "init", "-q")
	hostGit(t, dir, "config", "user.useConfigOnly", "true")
	hostGit(t, dir, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-q", "-m", "initial")

	must.NoError(os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("x"), 0o644))
	hostGit(t, dir, "add", "probe.txt")
	probe := exec.Command("git", "commit", "-m", "probe")
	probe.Dir = dir
	out, err := probe.CombinedOutput()
	must.Error(err, "precondition: plain git commit succeeded without an identity:\n%s", out)

	ctx := context.Background()
	gs := localStrategy(t, dir)
	must.NoError(gs.Setup(ctx, dir, "task-noid"))
	must.NoError(os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work"), 0o644))
	must.NoError(gs.Checkpoint(ctx, "turn"))
	must.NoError(os.WriteFile(filepath.Join(dir, "more.txt"), []byte("more"), 0o644))
	handoff, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	check.False(handoff.Dirty, "Dirty after the final checkpoint")

	want := HarnessIdentityName + " <" + HarnessIdentityEmail + ">"
	for _, rev := range []string{"HEAD", "HEAD~1"} {
		t.Run(rev, func(t *testing.T) {
			must := require.New(t)
			must.Equal(want+"|"+want, hostGit(t, dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>", rev), "author|committer")
		})
	}
}

func TestLocal_Finalise_CommitFailureReturnsErrorWithState(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()
	base := hostGit(t, repo, "rev-parse", "HEAD")

	gs := localStrategy(t, repo)
	must.NoError(gs.Setup(ctx, repo, "task-fail"))
	must.NoError(os.WriteFile(filepath.Join(repo, "stuck.txt"), []byte("stuck"), 0o644))
	// A held index lock makes staging fail the way a concurrent git
	// process would.
	must.NoError(os.WriteFile(filepath.Join(repo, ".git", "index.lock"), nil, 0o644))

	handoff, err := gs.Finalise(ctx, "success")
	must.ErrorContains(err, "final checkpoint", "Finalise must fail with the index locked")
	must.NotNil(handoff, "Finalise returned a nil handoff alongside its error")
	check.Equal(base, handoff.SHA, "SHA stays at the base commit")
	check.Equal(base, handoff.BaseSHA, "BaseSHA is the base commit")
	check.Equal("stirrup/task-fail", handoff.Branch)
	check.True(handoff.Dirty, "Dirty with an uncommitted file")
}

func TestLocal_Finalise_EmbeddedRepositoryReportsDirty(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()

	gs := localStrategy(t, repo)
	must.NoError(gs.Setup(ctx, repo, "task-dirty"))
	nested := filepath.Join(repo, "nested")
	must.NoError(os.Mkdir(nested, 0o755))
	hostGit(t, nested, "init", "-q")
	hostGit(t, nested, "-c", "user.name=n", "-c", "user.email=n@example.com", "commit", "--allow-empty", "-q", "-m", "nested")
	must.NoError(os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("inner"), 0o644))

	handoff, err := gs.Finalise(ctx, "success")
	must.NoError(err)
	check.True(handoff.Dirty, "embedded repository has uncommitted content")
	check.NotEqual(handoff.BaseSHA, handoff.SHA, "final checkpoint made no commit for the embedded repository")
}

func TestLocal_Finalise_UnbornBranchCommitsWithEmptyBase(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	hostGit(t, dir, "init", "-q")
	ctx := context.Background()

	gs := localStrategy(t, dir)
	must.NoError(gs.Setup(ctx, dir, "task-unborn"))
	must.NoError(os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first"), 0o644))

	handoff, err := gs.Finalise(ctx, "max_turns")
	must.NoError(err)
	check.Empty(handoff.BaseSHA, "BaseSHA for an unborn branch")
	check.Equal(hostGit(t, dir, "rev-parse", "HEAD"), handoff.SHA, "SHA is the new HEAD")
	check.Equal("stirrup/task-unborn", handoff.Branch)
}

// isolateGitConfig hides the user's and the system's git configuration, and
// any identity in the environment, from git commands the test runs.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	must := require.New(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		must.NoError(os.Unsetenv(name), "unset %s", name)
	}
}

func TestIsNotRepository_RecognisesBothGitWordings(t *testing.T) {
	check := assert.New(t)
	for _, stderr := range []string{
		"fatal: not a git repository (or any of the parent directories): .git",
		"fatal: gitfile does not point to a valid repository: /ws/.git",
	} {
		check.True(isNotRepository(&executor.ExecResult{ExitCode: 128, Stderr: stderr}), stderr)
	}
	check.False(isNotRepository(&executor.ExecResult{ExitCode: 0}))
	check.False(isNotRepository(&executor.ExecResult{ExitCode: 128, Stderr: "fatal: unrelated"}))
}
