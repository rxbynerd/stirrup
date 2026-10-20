package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const writeProbeTool = "write_probe"

// toolLoopProvider answers every turn with one call to writeProbeTool
// until stopAfter calls have been made, then ends the turn. input chooses
// each call's arguments; inputTokens is reported on every tool turn.
type toolLoopProvider struct {
	mu          sync.Mutex
	calls       int
	stopAfter   int
	input       func(n int) map[string]any
	inputTokens int
}

func (p *toolLoopProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	ch := make(chan types.StreamEvent, 2)
	if p.stopAfter > 0 && n > p.stopAfter {
		ch <- types.StreamEvent{Type: "text_delta", Text: "done"}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
		close(ch)
		return ch, nil
	}
	input := map[string]any{"n": n}
	if p.input != nil {
		input = p.input(n)
	}
	ch <- types.StreamEvent{Type: "tool_call", ID: fmt.Sprintf("tc_%d", n), Name: writeProbeTool, Input: input}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use", InputTokens: p.inputTokens}
	close(ch)
	return ch, nil
}

// writeProbe is the writeProbeTool handler state: each call writes
// probe-<n>.txt into the repository, then runs after (if set), whose
// error becomes the tool's result.
type writeProbe struct {
	repo  string
	mu    sync.Mutex
	calls int
	after func(ctx context.Context, n int) error
}

func (w *writeProbe) handler(ctx context.Context, _ json.RawMessage) (string, error) {
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	if err := os.WriteFile(filepath.Join(w.repo, probeFile(n)), []byte("probe"), 0o644); err != nil {
		return "", err
	}
	if w.after != nil {
		if err := w.after(ctx, n); err != nil {
			return "", err
		}
	}
	return "written", nil
}

func probeFile(n int) string { return fmt.Sprintf("probe-%d.txt", n) }

// newGitRepo creates a repository with one commit and returns its path and
// the commit's SHA.
func newGitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	hostGit(t, dir, "init", "--quiet")
	hostGit(t, dir, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--quiet", "--allow-empty", "-m", "base")
	return dir, hostGit(t, dir, "rev-parse", "HEAD")
}

func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %v\n%s", args, out)
	return strings.TrimSpace(string(out))
}

// newGitFinaliseLoop wires a loop with the deterministic git strategy on
// repo and writeProbeTool backed by probe.
func newGitFinaliseLoop(t *testing.T, repo string, prov *toolLoopProvider, probe *writeProbe) (*AgenticLoop, *types.RunConfig, *recordingTransport) {
	t.Helper()
	must := require.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = prov
	exec, err := executor.NewLocalExecutor(repo)
	must.NoError(err, "NewLocalExecutor")
	loop.Git = git.NewDeterministicGitStrategy(exec)
	rec := &recordingTransport{}
	loop.Transport = rec
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        writeProbeTool,
		Description: "writes a probe file into the workspace",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Handler:     probe.handler,
	})
	loop.Tools = registry
	config := buildTestConfig()
	config.Executor.Workspace = repo
	config.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	return loop, config, rec
}

// assertCommittedOnRunBranch checks that file is in the run branch's HEAD
// tree, that HEAD is the final checkpoint for outcome, and that the trace's
// git handoff names the branch, the new HEAD and base.
func assertCommittedOnRunBranch(t *testing.T, repo, base, file, outcome string, rt *types.RunTrace) {
	t.Helper()
	check := assert.New(t)
	must := require.New(t)
	must.NotNil(rt, "Run returned a nil trace")
	must.Equal(outcome, rt.Outcome)

	head := hostGit(t, repo, "rev-parse", "HEAD")
	check.Equal(file, hostGit(t, repo, "ls-tree", "--name-only", "HEAD", file), "%s is not committed at HEAD", file)
	check.Equal("Final checkpoint (outcome: "+outcome+")", hostGit(t, repo, "log", "-1", "--format=%s"), "HEAD commit message")
	check.Empty(hostGit(t, repo, "status", "--porcelain"), "working tree still dirty after the final checkpoint")

	must.NotNil(rt.Git, "RunTrace.Git should hold the git handoff")
	check.Equal(types.GitHandoff{Branch: "stirrup/test-run-1", SHA: head, BaseSHA: base}, *rt.Git)
}

func TestRun_BudgetExceededCommitsLastToolTurn(t *testing.T) {
	must := require.New(t)
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{inputTokens: 1000}, probe)
	budget := 10
	config.MaxTokenBudget = &budget

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "budget_exceeded", rt)
}

func TestRun_StalledCommitsLastToolTurn(t *testing.T) {
	must := require.New(t)
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	prov := &toolLoopProvider{input: func(int) map[string]any { return map[string]any{"n": 0} }}
	loop, config, _ := newGitFinaliseLoop(t, repo, prov, probe)

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	assertCommittedOnRunBranch(t, repo, base, probeFile(2*stallRepeats), "stalled", rt)
}

func TestRun_ToolFailuresCommitsLastToolTurn(t *testing.T) {
	must := require.New(t)
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: func(context.Context, int) error {
		return errors.New("tool failed after writing")
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{}, probe)

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	assertCommittedOnRunBranch(t, repo, base, probeFile(maxConsecutiveFailures), "tool_failures", rt)
}

func TestRun_TimeoutCommitsOnDetachedContext(t *testing.T) {
	must := require.New(t)
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{}, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rt, err := loop.Run(ctx, config)
	must.NoError(err)
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "timeout", rt)
}

func TestRun_CancellationCommitsOnDetachedContext(t *testing.T) {
	must := require.New(t)
	repo, base := newGitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &writeProbe{repo: repo, after: func(ctx context.Context, _ int) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{}, probe)

	rt, err := loop.Run(ctx, config)
	must.NoError(err)
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "cancelled", rt)
}

func TestRun_NoGitIdentityCommitsAsHarnessAndSucceeds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		must.NoError(os.Unsetenv(name), "unset %s", name)
	}
	repo, base := newGitRepo(t)
	hostGit(t, repo, "config", "user.useConfigOnly", "true")

	probe := &writeProbe{repo: repo}
	loop, config, rec := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", rt.Outcome)
	must.NotNil(rt.Git, "RunTrace.Git should hold the git handoff")
	check.Empty(rt.Git.Error, "handoff should not record an error")
	check.False(rt.Git.Dirty, "handoff should not report a dirty tree")
	check.NotEqual(base, rt.Git.SHA, "handoff should move past base %s", base)

	check.Equal(probeFile(1), hostGit(t, repo, "ls-tree", "--name-only", "HEAD", probeFile(1)), "%s should be committed at HEAD", probeFile(1))

	want := git.HarnessIdentityName + " <" + git.HarnessIdentityEmail + ">"
	got := hostGit(t, repo, "log", "--format=%an <%ae>", base+"..HEAD")
	check.NotEmpty(got, "run should add commits past base")
	check.Empty(strings.Trim(strings.ReplaceAll(got, want, ""), "\n"), "run commits authored as %q, want only %q", got, want)

	for _, ev := range rec.events {
		check.NotEqual("warning", ev.Type, "unexpected warning event: %s", ev.Message)
	}
}

// lockIndexAfterWrite makes every later git staging attempt fail, the way
// a commit failure in the sandbox would.
func lockIndexAfterWrite(repo string) func(context.Context, int) error {
	return func(context.Context, int) error {
		return os.WriteFile(filepath.Join(repo, ".git", "index.lock"), nil, 0o644)
	}
}

func TestRun_FinalCheckpointFailureOnSuccessYieldsHandoffFailed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: lockIndexAfterWrite(repo)}
	loop, config, rec := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)
	hooks := &fakeHookRunner{}
	loop.Hooks = hooks

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("handoff_failed", rt.Outcome)
	must.NotNil(rt.Git, "RunTrace.Git should hold the git handoff")
	must.Contains(rt.Git.Error, "final checkpoint")
	check.Equal(base, rt.Git.SHA, "handoff SHA should stay at base")
	check.Equal("stirrup/test-run-1", rt.Git.Branch)
	check.True(rt.Git.Dirty, "handoff should report a dirty tree")
	check.Equal([]string{"handoff_failed"}, hooks.postCalls, "postRun hooks should see the handoff_failed outcome")
	check.True(hasEvent(rec, "done", "handoff_failed"), "done event should carry handoff_failed")
}

func TestRun_FinalCheckpointFailureKeepsEarlierFailureOutcome(t *testing.T) {
	must := require.New(t)
	repo, _ := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: lockIndexAfterWrite(repo)}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{inputTokens: 1000}, probe)
	budget := 10
	config.MaxTokenBudget = &budget

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("budget_exceeded", rt.Outcome, "the earlier failure outcome should be kept")
	must.NotNil(rt.Git, "RunTrace.Git should record the git failure")
	must.NotEmpty(rt.Git.Error, "RunTrace.Git should record the git failure")
}

func TestRun_DirtyTreeAfterFinalCheckpointYieldsHandoffFailed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	repo, _ := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: func(context.Context, int) error {
		nested := filepath.Join(repo, "nested")
		if err := os.Mkdir(nested, 0o755); err != nil {
			return err
		}
		cmd := exec.Command("sh", "-c", "git init --quiet && git -c user.name=n -c user.email=n@example.com commit --quiet --allow-empty -m n && echo x > inner.txt")
		cmd.Dir = nested
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("handoff_failed", rt.Outcome)
	must.NotNil(rt.Git, "RunTrace.Git should hold the git handoff")
	check.True(rt.Git.Dirty, "handoff should report a dirty tree")
	check.Equal(errDirtyAfterFinalCheckpoint.Error(), rt.Git.Error)
}

func TestRun_PostRunHookEditsAreNotCommitted(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	repo, _ := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)
	loop.Hooks = &fakeHookRunner{onPost: func(context.Context) {
		_ = os.WriteFile(filepath.Join(repo, "hook.txt"), []byte("hook"), 0o644)
	}}

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", rt.Outcome)
	check.Equal("?? hook.txt", hostGit(t, repo, "status", "--porcelain"), "only the hook's file should be untracked")
	must.NotNil(rt.Git, "RunTrace.Git should hold the git handoff")
	check.False(rt.Git.Dirty, "the handoff should be taken before the hook runs")
}

// fakeFinaliseGit returns a fixed Finalise result and records the outcome
// it was given.
type fakeFinaliseGit struct {
	handoff *types.GitHandoff
	err     error
	outcome string
}

func (g *fakeFinaliseGit) Setup(context.Context, string, string) error { return nil }
func (g *fakeFinaliseGit) Checkpoint(context.Context, string) error    { return nil }
func (g *fakeFinaliseGit) Finalise(_ context.Context, outcome string) (*types.GitHandoff, error) {
	g.outcome = outcome
	return g.handoff, g.err
}

func TestRun_NoneGitStrategyOmitsGitHandoff(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rt, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("success", rt.Outcome)
	must.Nil(rt.Git, "the none strategy should not produce a git handoff")
}

func TestRun_FinaliseReceivesPreGitOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	fake := &fakeFinaliseGit{handoff: &types.GitHandoff{Branch: "b", SHA: "s", BaseSHA: "s"}}
	loop.Git = fake
	rt, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", fake.outcome, "Finalise should receive the pre-git outcome")
	must.Equal(fake.handoff, rt.Git, "RunTrace.Git should be the handoff from Finalise")
}

func TestRun_FinaliseErrorWithoutHandoffIsRecorded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Git = &fakeFinaliseGit{err: errors.New("strategy broke")}
	rt, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("handoff_failed", rt.Outcome)
	must.NotNil(rt.Git, "RunTrace.Git should record the error")
	check.Equal("strategy broke", rt.Git.Error)
}

func TestRun_GitHandoffErrorIsScrubbedAndCapped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	long := "commit hook printed " + spanProbeKey + " " + strings.Repeat("x", 4*maxFailureTextBytes)
	loop.Git = &fakeFinaliseGit{handoff: &types.GitHandoff{}, err: errors.New(long)}
	rt, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.NotNil(rt.Git, "RunTrace.Git should record the error")
	check.NotContains(rt.Git.Error, spanProbeKey, "RunTrace.Git.Error should not carry the unscrubbed key")
	check.LessOrEqual(len(rt.Git.Error), maxFailureTextBytes+len("... [truncated by harness]"),
		"RunTrace.Git.Error should be capped near %d bytes", maxFailureTextBytes)
	for _, ev := range rec.events {
		check.NotContains(ev.Message, spanProbeKey, "%s event should not carry the unscrubbed key", ev.Type)
	}
}

func hasEvent(rec *recordingTransport, typ, stopReason string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if ev.Type == typ && ev.StopReason == stopReason {
			return true
		}
	}
	return false
}
