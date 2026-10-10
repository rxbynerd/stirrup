package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: testSHA1, branch: "stirrup/run-1", staged: true}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)
	ctx := context.Background()

	if err := gs.Setup(ctx, "/ignored/host/path", "run-1"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := gs.Checkpoint(ctx, "Turn 1: 2 tool calls"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	want := types.GitHandoff{Branch: "stirrup/run-1", SHA: testSHA1, BaseSHA: testSHA1}
	if *result != want {
		t.Errorf("Finalise = %+v, want %+v", *result, want)
	}

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
	if len(exec.calls) != len(wantSequence) {
		t.Fatalf("executor saw %d commands, want %d:\n%s", len(exec.calls), len(wantSequence), strings.Join(exec.calls, "\n"))
	}
	for i, want := range wantSequence {
		got := exec.calls[i]
		if i == 0 {
			if got != want[0] {
				t.Errorf("call[0] = %q, want %q", got, want[0])
			}
			continue
		}
		if !strings.HasPrefix(got, "git ") {
			t.Errorf("call[%d] = %q, want a git command", i, got)
		}
		if !hasArgs(got, want...) {
			t.Errorf("call[%d] = %q, want arguments %q", i, got, want)
		}
		for j := 0; j < len(HardeningFlags); j += 2 {
			if !hasArgs(got, HardeningFlags[j], HardeningFlags[j+1]) {
				t.Errorf("call[%d] lacks hardening flag %s %s: %q", i, HardeningFlags[j], HardeningFlags[j+1], got)
			}
		}
		if !hasArgs(got, "-c", "user.name="+HarnessIdentityName) || !hasArgs(got, "-c", "user.email="+HarnessIdentityEmail) {
			t.Errorf("call[%d] lacks the harness identity: %q", i, got)
		}
		if !hasArgs(got, "-c", "safe.directory=/workspace") {
			t.Errorf("call[%d] lacks safe.directory for the workspace root: %q", i, got)
		}
	}
}

func TestSetup_RejectsEnclosingRepository(t *testing.T) {
	emptyHostPath(t)
	repo := &fakeRepo{root: "/parent/ws", toplevel: "/parent"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)

	err := gs.Setup(context.Background(), "", "run-2")
	if err == nil {
		t.Fatal("Setup succeeded inside an enclosing repository")
	}
	if !strings.Contains(err.Error(), "/parent/ws is inside repository /parent") {
		t.Errorf("error = %v, want it to name both roots", err)
	}
	for _, call := range exec.calls {
		for _, mutating := range []string{"checkout", "init", "add", "commit"} {
			if hasArgs(call, mutating) {
				t.Errorf("Setup issued a mutating command after detecting the enclosing repository: %q", call)
			}
		}
	}
	if err := gs.Checkpoint(context.Background(), "x"); err == nil {
		t.Error("Checkpoint succeeded after a failed Setup")
	}
	if _, err := gs.Finalise(context.Background(), "success"); err == nil || !strings.Contains(err.Error(), "has not run") {
		t.Errorf("Finalise after a failed Setup error = %v, want the setup-has-not-run error", err)
	}
}

func TestSetup_InitialisesNonRepository(t *testing.T) {
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", notRepo: true, sha: testSHA1, branch: "stirrup/run-3"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)

	if err := gs.Setup(context.Background(), "", "run-3"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !containsCall(exec.calls, "test -e .git || test -L .git") {
		t.Errorf("Setup initialised without checking for a .git entry:\n%s", strings.Join(exec.calls, "\n"))
	}
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
	if len(gitCalls) != len(want) {
		t.Fatalf("git commands = %d, want %d:\n%s", len(gitCalls), len(want), strings.Join(gitCalls, "\n"))
	}
	for i, args := range want {
		if !hasArgs(gitCalls[i], args...) {
			t.Errorf("git call[%d] = %q, want %q", i, gitCalls[i], args)
		}
	}
}

func TestSetup_RefusesUnresolvableGitEntry(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "self-contained repository") {
		t.Fatalf("Setup error = %v, want the unresolvable .git entry message", err)
	}
	for _, call := range exec.calls {
		for _, mutating := range []string{"init", "checkout", "add", "commit"} {
			if hasArgs(call, mutating) {
				t.Errorf("Setup issued %q despite an unresolvable .git entry", call)
			}
		}
	}
	if err := gs.Checkpoint(context.Background(), "x"); err == nil {
		t.Error("Checkpoint succeeded after a failed Setup")
	}
}

func TestSetup_KeepsTrailingSpaceInWorkspaceRoot(t *testing.T) {
	emptyHostPath(t)
	for name, repo := range map[string]*fakeRepo{
		"existing repository": {root: "/work space ", toplevel: "/work space "},
		"initialised":         {root: "/work space ", notRepo: true},
	} {
		t.Run(name, func(t *testing.T) {
			gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
			if err := gs.Setup(context.Background(), "", "run-3c"); err != nil {
				t.Fatalf("Setup: %v", err)
			}
		})
	}
}

func TestSetup_ReportsMissingGit(t *testing.T) {
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", gitMissing: true}
	gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))

	err := gs.Setup(context.Background(), "", "run-4")
	if err == nil || !strings.Contains(err.Error(), "git is not available in the sandbox") {
		t.Fatalf("Setup error = %v, want the missing-git message", err)
	}
}

func TestSetup_RejectsExecutorWithoutExec(t *testing.T) {
	exec := newScriptedExecutor(func(string) (*executor.ExecResult, error) {
		return nil, fmt.Errorf("must not be called")
	})
	exec.caps.CanExec = false
	gs := NewDeterministicGitStrategy(exec)
	if err := gs.Setup(context.Background(), "", "run-5"); err == nil {
		t.Fatal("Setup succeeded on an executor that cannot run commands")
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor was called %d times", len(exec.calls))
	}
	if err := NewDeterministicGitStrategy(nil).Setup(context.Background(), "", "run-5"); err == nil {
		t.Fatal("Setup succeeded with a nil executor")
	}
}

func TestSetup_RejectsUnusableWorkspaceRoot(t *testing.T) {
	for _, pwd := range []string{"", "relative/path\n", "/ws\nextra\n"} {
		exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
			if command == "pwd -P" {
				return &executor.ExecResult{Stdout: pwd}, nil
			}
			return &executor.ExecResult{}, nil
		})
		gs := NewDeterministicGitStrategy(exec)
		if err := gs.Setup(context.Background(), "", "run-6"); err == nil {
			t.Errorf("Setup accepted pwd output %q", pwd)
		}
		if len(exec.calls) != 1 {
			t.Errorf("pwd output %q: %d commands issued, want only pwd", pwd, len(exec.calls))
		}
	}
}

func TestSetup_RejectsTaskIDThatIsNotABranchName(t *testing.T) {
	exec := newScriptedExecutor(func(string) (*executor.ExecResult, error) {
		return nil, fmt.Errorf("must not be called")
	})
	gs := NewDeterministicGitStrategy(exec)
	for _, id := range []string{"", "a..b", "x y", "lock.lock", "a\nb"} {
		if err := gs.Setup(context.Background(), "", id); err == nil {
			t.Errorf("Setup accepted task id %q", id)
		}
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor was called %d times", len(exec.calls))
	}
}

func TestCheckpoint_NoChanges_IssuesNoCommit(t *testing.T) {
	emptyHostPath(t)
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace"}
	exec := newScriptedExecutor(repo.respond)
	gs := NewDeterministicGitStrategy(exec)
	ctx := context.Background()
	if err := gs.Setup(ctx, "", "run-7"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := gs.Checkpoint(ctx, "nothing"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	for _, call := range exec.calls {
		if hasArgs(call, "commit") {
			t.Errorf("Checkpoint committed with nothing staged: %q", call)
		}
	}
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
			repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: tc.sha, branch: tc.branch}
			gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
			ctx := context.Background()
			if err := gs.Setup(ctx, "", "run-8"); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if result, err := gs.Finalise(ctx, "success"); err == nil {
				t.Fatalf("Finalise accepted %+v", result)
			}
		})
	}
}

func TestFinalise_AcceptsSHA256(t *testing.T) {
	repo := &fakeRepo{root: "/workspace", toplevel: "/workspace", sha: testSHA256, branch: "stirrup/run-9"}
	gs := NewDeterministicGitStrategy(newScriptedExecutor(repo.respond))
	ctx := context.Background()
	if err := gs.Setup(ctx, "", "run-9"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if result.SHA != testSHA256 {
		t.Errorf("SHA = %q, want %q", result.SHA, testSHA256)
	}
}

func TestFinalise_BeforeSetupFails(t *testing.T) {
	gs := NewDeterministicGitStrategy(newScriptedExecutor((&fakeRepo{}).respond))
	handoff, err := gs.Finalise(context.Background(), "success")
	if err == nil {
		t.Fatal("Finalise succeeded before Setup")
	}
	if handoff == nil {
		t.Fatal("Finalise before Setup returned a nil handoff")
	}
}

func TestProbe(t *testing.T) {
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
			exec := newScriptedExecutor(func(command string) (*executor.ExecResult, error) {
				if !hasArgs(command, "--version") {
					t.Errorf("Probe issued %q, want git --version", command)
				}
				return tc.res, nil
			})
			err := NewDeterministicGitStrategy(exec).Probe(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Probe error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidBranchName(t *testing.T) {
	valid := []string{"main", "stirrup/run-1", "feature/a.b", "a/b/c", "HEAD", "v1.0"}
	for _, name := range valid {
		if !ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"", "@", "-x", "/x", "x/", "x.", ".x", "a/.b", "a..b", "a//b", "a@{b", "a.lock", "a.lock/b",
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a\x01b", "a\x7fb", "a\nb",
		strings.Repeat("x", maxBranchNameLen+1),
	}
	for _, name := range invalid {
		if ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = true, want false", name)
		}
	}
}

func TestCommandLine_QuotesEveryArgument(t *testing.T) {
	line := CommandLine("commit", "-m", "it's; rm -rf /")
	if !strings.HasPrefix(line, "git '-c' 'core.hooksPath=/dev/null'") {
		t.Errorf("CommandLine = %q, want it to begin with the hardening flags", line)
	}
	if !strings.HasSuffix(line, `'commit' '-m' 'it'\''s; rm -rf /'`) {
		t.Errorf("CommandLine = %q, want quoted trailing arguments", line)
	}
	if strings.Count(line, ";") != 1 {
		t.Errorf("CommandLine = %q, want the only `;` inside quotes", line)
	}
}

// The tests below drive the strategy through a LocalExecutor against real
// repositories. Fixtures are prepared with the host's git; the strategy
// itself still only sees the executor.

func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
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
	exec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
	return NewDeterministicGitStrategy(exec)
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(hostGit(t, dir, "rev-list", "--count", "HEAD"), "%d", &n); err != nil {
		t.Fatalf("parse count: %v", err)
	}
	return n
}

func TestLocal_SetupCheckpointFinalise(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	gs := localStrategy(t, repo)
	ctx := context.Background()

	if err := gs.Setup(ctx, repo, "task-42"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := hostGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "stirrup/task-42" {
		t.Errorf("branch = %q, want stirrup/task-42", got)
	}

	before := commitCount(t, repo)
	if err := gs.Checkpoint(ctx, "nothing"); err != nil {
		t.Fatalf("Checkpoint (no changes): %v", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count changed from %d to %d with no changes", before, after)
	}

	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "add hello"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := hostGit(t, repo, "log", "-1", "--format=%s"); got != "add hello" {
		t.Errorf("commit message = %q, want %q", got, "add hello")
	}

	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if result.Branch != "stirrup/task-42" {
		t.Errorf("Finalise branch = %q", result.Branch)
	}
	if want := hostGit(t, repo, "rev-parse", "HEAD"); result.SHA != want {
		t.Errorf("Finalise SHA = %q, want %q", result.SHA, want)
	}
}

func TestLocal_CommitsWithHarnessIdentityWhenNoneConfigured(t *testing.T) {
	// An empty HOME and XDG_CONFIG_HOME leave git with no global identity;
	// the local executor forwards both to the child.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	hostGit(t, repo, "init", "-q")
	hostGit(t, repo, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-q", "-m", "initial")

	gs := localStrategy(t, repo)
	ctx := context.Background()
	if err := gs.Setup(ctx, repo, "identity"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	want := HarnessIdentityName + " <" + HarnessIdentityEmail + ">"
	if got := hostGit(t, repo, "log", "-1", "--format=%an <%ae>"); got != want {
		t.Errorf("author = %q, want %q", got, want)
	}
	if got := hostGit(t, repo, "log", "-1", "--format=%cn <%ce>"); got != want {
		t.Errorf("committer = %q, want %q", got, want)
	}
}

// TestLocal_PlantedRepositoryConfigDoesNotExecute protects the hardening
// flags: hooks, core.hooksPath, core.fsmonitor and commit signing planted in
// the repository must not run during a checkpoint.
func TestLocal_PlantedRepositoryConfigDoesNotExecute(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	markers := t.TempDir()
	marker := func(name string) string { return filepath.Join(markers, name) }

	hook := "#!/bin/sh\ntouch " + marker("hook") + "\n"
	hooksDir := filepath.Join(repo, ".git", "hooks")
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-index-change", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hooksDir, name), []byte(hook), 0o755); err != nil {
			t.Fatalf("write hook: %v", err)
		}
	}
	plantedHooks := filepath.Join(repo, "planted-hooks")
	if err := os.MkdirAll(plantedHooks, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plantedHooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker("hookspath")+"\n"), 0o755); err != nil {
		t.Fatalf("write planted hook: %v", err)
	}
	fakeGPG := filepath.Join(repo, "fake-gpg")
	if err := os.WriteFile(fakeGPG, []byte("#!/bin/sh\ntouch "+marker("gpg")+"\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gpg: %v", err)
	}
	hostGit(t, repo, "config", "core.hooksPath", plantedHooks)
	hostGit(t, repo, "config", "core.fsmonitor", "touch "+marker("fsmonitor")+"; echo /")
	hostGit(t, repo, "config", "commit.gpgsign", "true")
	hostGit(t, repo, "config", "gpg.program", fakeGPG)

	// Control: an unhardened commit must trigger the planted hook, or the
	// absence of markers below would prove nothing.
	hostGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", "control")
	if _, err := os.Stat(marker("hookspath")); err != nil {
		t.Fatalf("control commit did not run the planted core.hooksPath hook: %v", err)
	}
	if err := os.RemoveAll(markers); err != nil {
		t.Fatalf("reset markers: %v", err)
	}
	if err := os.MkdirAll(markers, 0o755); err != nil {
		t.Fatalf("recreate markers: %v", err)
	}

	gs := localStrategy(t, repo)
	ctx := context.Background()
	if err := gs.Setup(ctx, repo, "planted"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "checkpoint"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := gs.Finalise(ctx, "success"); err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("planted %s payload executed during the checkpoint", e.Name())
	}
}

func TestLocal_SetupRejectsWorkspaceInsideRepository(t *testing.T) {
	parent := t.TempDir()
	initHostRepo(t, parent)
	ws := filepath.Join(parent, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "unrelated.txt"), []byte("operator work"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	branchBefore := hostGit(t, parent, "rev-parse", "--abbrev-ref", "HEAD")
	commitsBefore := commitCount(t, parent)

	gs := localStrategy(t, ws)
	err := gs.Setup(context.Background(), ws, "nested")
	if err == nil {
		t.Fatal("Setup succeeded for a workspace inside another repository")
	}
	if !strings.Contains(err.Error(), "inside repository") {
		t.Errorf("error = %v, want the enclosing-repository message", err)
	}
	if got := hostGit(t, parent, "rev-parse", "--abbrev-ref", "HEAD"); got != branchBefore {
		t.Errorf("enclosing repository branch changed from %q to %q", branchBefore, got)
	}
	if got := commitCount(t, parent); got != commitsBefore {
		t.Errorf("enclosing repository commit count changed from %d to %d", commitsBefore, got)
	}
	if status := hostGit(t, parent, "status", "--porcelain"); !strings.Contains(status, "unrelated.txt") {
		t.Errorf("enclosing repository's uncommitted work was staged or committed; status = %q", status)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); !os.IsNotExist(err) {
		t.Errorf("Setup created a repository inside the nested workspace (stat err = %v)", err)
	}
}

func TestLocal_SetupRefusesWorktreeWithUnreachableGitdir(t *testing.T) {
	ws := t.TempDir()
	gitFile := filepath.Join(ws, ".git")
	const pointer = "gitdir: /nonexistent/host/repo/.git/worktrees/feature\n"
	if err := os.WriteFile(gitFile, []byte(pointer), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	gs := localStrategy(t, ws)
	err := gs.Setup(context.Background(), ws, "worktree")
	if err == nil || !strings.Contains(err.Error(), "self-contained repository") {
		t.Fatalf("Setup error = %v, want the unresolvable .git entry message", err)
	}
	got, err := os.ReadFile(gitFile)
	if err != nil || string(got) != pointer {
		t.Errorf(".git file changed: content %q, err %v", got, err)
	}
}

func TestLocal_SetupInitialisesPlainDirectory(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gs := localStrategy(t, ws)
	ctx := context.Background()
	if err := gs.Setup(ctx, ws, "fresh"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := hostGit(t, ws, "rev-parse", "--abbrev-ref", "HEAD"); got != "stirrup/fresh" {
		t.Errorf("branch = %q, want stirrup/fresh", got)
	}
	if got := hostGit(t, ws, "ls-files"); got != "seed.txt" {
		t.Errorf("base commit tracks %q, want seed.txt", got)
	}
	result, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if !ValidSHA(result.SHA) {
		t.Errorf("Finalise SHA = %q", result.SHA)
	}
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

func TestLocal_FinaliseCommitsOutstandingWork(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()
	base := hostGit(t, repo, "rev-parse", "HEAD")

	gs := localStrategy(t, repo)
	if err := gs.Setup(ctx, repo, "task-final"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "partial.txt"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "budget_exceeded")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	head := hostGit(t, repo, "rev-parse", "HEAD")
	if head == base {
		t.Fatal("HEAD did not move; outstanding work was not committed")
	}
	if got := hostGit(t, repo, "log", "-1", "--format=%s"); got != "Final checkpoint (outcome: budget_exceeded)" {
		t.Errorf("commit message = %q", got)
	}
	if got := hostGit(t, repo, "ls-tree", "--name-only", "HEAD", "partial.txt"); got != "partial.txt" {
		t.Errorf("partial.txt not in HEAD tree (ls-tree = %q)", got)
	}
	want := types.GitHandoff{Branch: "stirrup/task-final", SHA: head, BaseSHA: base}
	if *handoff != want {
		t.Errorf("handoff = %+v, want %+v", *handoff, want)
	}
}

func TestLocal_FinaliseNothingOutstandingReportsHEADWithoutCommit(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()

	gs := localStrategy(t, repo)
	if err := gs.Setup(ctx, repo, "task-clean"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	before := commitCount(t, repo)

	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count changed from %d to %d on a clean tree", before, after)
	}
	if handoff.SHA != handoff.BaseSHA || handoff.SHA == "" {
		t.Errorf("handoff = %+v, want SHA equal to a non-empty BaseSHA", *handoff)
	}
	if handoff.Dirty {
		t.Error("Dirty = true on a clean tree")
	}
}

func TestLocal_NoConfiguredIdentityCommitsAsHarness(t *testing.T) {
	isolateGitConfig(t)
	dir := t.TempDir()
	hostGit(t, dir, "init", "-q")
	hostGit(t, dir, "config", "user.useConfigOnly", "true")
	hostGit(t, dir, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-q", "-m", "initial")

	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	hostGit(t, dir, "add", "probe.txt")
	probe := exec.Command("git", "commit", "-m", "probe")
	probe.Dir = dir
	if out, err := probe.CombinedOutput(); err == nil {
		t.Fatalf("precondition: plain git commit succeeded without an identity:\n%s", out)
	}

	ctx := context.Background()
	gs := localStrategy(t, dir)
	if err := gs.Setup(ctx, dir, "task-noid"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gs.Checkpoint(ctx, "turn"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "more.txt"), []byte("more"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if handoff.Dirty {
		t.Errorf("Dirty = true after the final checkpoint")
	}
	want := HarnessIdentityName + " <" + HarnessIdentityEmail + ">"
	for _, rev := range []string{"HEAD", "HEAD~1"} {
		if got := hostGit(t, dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>", rev); got != want+"|"+want {
			t.Errorf("%s author|committer = %q, want %q for both", rev, got, want)
		}
	}
}

func TestLocal_FinaliseCommitFailureReturnsErrorWithState(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()
	base := hostGit(t, repo, "rev-parse", "HEAD")

	gs := localStrategy(t, repo)
	if err := gs.Setup(ctx, repo, "task-fail"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stuck.txt"), []byte("stuck"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// A held index lock makes staging fail the way a concurrent git
	// process would.
	if err := os.WriteFile(filepath.Join(repo, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatalf("write index.lock: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "success")
	if err == nil {
		t.Fatal("Finalise returned nil error with the index locked")
	}
	if !strings.Contains(err.Error(), "final checkpoint") {
		t.Errorf("error = %q, want it to name the final checkpoint", err)
	}
	if handoff == nil {
		t.Fatal("Finalise returned a nil handoff alongside its error")
	}
	if handoff.SHA != base || handoff.BaseSHA != base || handoff.Branch != "stirrup/task-fail" {
		t.Errorf("handoff = %+v, want branch stirrup/task-fail at base %s", *handoff, base)
	}
	if !handoff.Dirty {
		t.Error("Dirty = false with an uncommitted file")
	}
}

func TestLocal_FinaliseEmbeddedRepositoryChangesReportDirty(t *testing.T) {
	repo := t.TempDir()
	initHostRepo(t, repo)
	ctx := context.Background()

	gs := localStrategy(t, repo)
	if err := gs.Setup(ctx, repo, "task-dirty"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hostGit(t, nested, "init", "-q")
	hostGit(t, nested, "-c", "user.name=n", "-c", "user.email=n@example.com", "commit", "--allow-empty", "-q", "-m", "nested")
	if err := os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	handoff, err := gs.Finalise(ctx, "success")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if !handoff.Dirty {
		t.Error("Dirty = false although the embedded repository has uncommitted content")
	}
	if handoff.SHA == handoff.BaseSHA {
		t.Error("final checkpoint made no commit for the embedded repository")
	}
}

func TestLocal_FinaliseUnbornBranchCommitsWithEmptyBase(t *testing.T) {
	dir := t.TempDir()
	hostGit(t, dir, "init", "-q")
	ctx := context.Background()

	gs := localStrategy(t, dir)
	if err := gs.Setup(ctx, dir, "task-unborn"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handoff, err := gs.Finalise(ctx, "max_turns")
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if handoff.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty for an unborn branch", handoff.BaseSHA)
	}
	if handoff.SHA != hostGit(t, dir, "rev-parse", "HEAD") || handoff.Branch != "stirrup/task-unborn" {
		t.Errorf("handoff = %+v, want the new HEAD on stirrup/task-unborn", *handoff)
	}
}

// isolateGitConfig hides the user's and the system's git configuration, and
// any identity in the environment, from git commands the test runs.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}
