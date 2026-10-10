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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitFinaliseLoop wires a loop with the deterministic git strategy on
// repo and writeProbeTool backed by probe.
func newGitFinaliseLoop(t *testing.T, repo string, prov *toolLoopProvider, probe *writeProbe) (*AgenticLoop, *types.RunConfig, *recordingTransport) {
	t.Helper()
	loop := buildTestLoop(nil)
	loop.Provider = prov
	exec, err := executor.NewLocalExecutor(repo)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
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
	if rt == nil {
		t.Fatal("Run returned a nil trace")
	}
	if rt.Outcome != outcome {
		t.Fatalf("outcome = %q, want %q", rt.Outcome, outcome)
	}
	head := hostGit(t, repo, "rev-parse", "HEAD")
	if got := hostGit(t, repo, "ls-tree", "--name-only", "HEAD", file); got != file {
		t.Errorf("%s is not committed at HEAD (ls-tree = %q)", file, got)
	}
	if got, want := hostGit(t, repo, "log", "-1", "--format=%s"), "Final checkpoint (outcome: "+outcome+")"; got != want {
		t.Errorf("HEAD commit message = %q, want %q", got, want)
	}
	if status := hostGit(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("working tree still dirty after the final checkpoint:\n%s", status)
	}
	want := types.GitHandoff{Branch: "stirrup/test-run-1", SHA: head, BaseSHA: base}
	if rt.Git == nil {
		t.Fatal("RunTrace.Git = nil, want the git handoff")
	}
	if *rt.Git != want {
		t.Errorf("RunTrace.Git = %+v, want %+v", *rt.Git, want)
	}
}

func TestRun_BudgetExceededCommitsLastToolTurn(t *testing.T) {
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{inputTokens: 1000}, probe)
	budget := 10
	config.MaxTokenBudget = &budget

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "budget_exceeded", rt)
}

func TestRun_StalledCommitsLastToolTurn(t *testing.T) {
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	prov := &toolLoopProvider{input: func(int) map[string]any { return map[string]any{"n": 0} }}
	loop, config, _ := newGitFinaliseLoop(t, repo, prov, probe)

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCommittedOnRunBranch(t, repo, base, probeFile(2*stallRepeats), "stalled", rt)
}

func TestRun_ToolFailuresCommitsLastToolTurn(t *testing.T) {
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: func(context.Context, int) error {
		return errors.New("tool failed after writing")
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{}, probe)

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCommittedOnRunBranch(t, repo, base, probeFile(maxConsecutiveFailures), "tool_failures", rt)
}

func TestRun_TimeoutCommitsOnDetachedContext(t *testing.T) {
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{}, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rt, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "timeout", rt)
}

func TestRun_CancellationCommitsOnDetachedContext(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCommittedOnRunBranch(t, repo, base, probeFile(1), "cancelled", rt)
}

func TestRun_NoGitIdentityCommitsAsHarnessAndSucceeds(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	repo, base := newGitRepo(t)
	hostGit(t, repo, "config", "user.useConfigOnly", "true")

	probe := &writeProbe{repo: repo}
	loop, config, rec := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", rt.Outcome)
	}
	if rt.Git == nil || rt.Git.Error != "" || rt.Git.Dirty || rt.Git.SHA == base {
		t.Fatalf("RunTrace.Git = %+v, want a clean handoff past base %s", rt.Git, base)
	}
	if got := hostGit(t, repo, "ls-tree", "--name-only", "HEAD", probeFile(1)); got != probeFile(1) {
		t.Errorf("%s is not committed at HEAD", probeFile(1))
	}
	want := git.HarnessIdentityName + " <" + git.HarnessIdentityEmail + ">"
	if got := hostGit(t, repo, "log", "--format=%an <%ae>", base+"..HEAD"); got == "" || strings.Trim(strings.ReplaceAll(got, want, ""), "\n") != "" {
		t.Errorf("run commits authored as %q, want only %q", got, want)
	}
	for _, ev := range rec.events {
		if ev.Type == "warning" {
			t.Errorf("unexpected warning event: %s", ev.Message)
		}
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
	repo, base := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: lockIndexAfterWrite(repo)}
	loop, config, rec := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)
	hooks := &fakeHookRunner{}
	loop.Hooks = hooks

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "handoff_failed" {
		t.Fatalf("outcome = %q, want handoff_failed", rt.Outcome)
	}
	if rt.Git == nil || !strings.Contains(rt.Git.Error, "final checkpoint") {
		t.Fatalf("RunTrace.Git = %+v, want the final checkpoint failure text", rt.Git)
	}
	if rt.Git.SHA != base || rt.Git.Branch != "stirrup/test-run-1" || !rt.Git.Dirty {
		t.Errorf("RunTrace.Git = %+v, want the run branch at base %s, dirty", *rt.Git, base)
	}
	if len(hooks.postCalls) != 1 || hooks.postCalls[0] != "handoff_failed" {
		t.Errorf("postRun hooks saw outcomes %v, want [handoff_failed]", hooks.postCalls)
	}
	if !hasEvent(rec, "done", "handoff_failed") {
		t.Error("done event does not carry handoff_failed")
	}
}

func TestRun_FinalCheckpointFailureKeepsEarlierFailureOutcome(t *testing.T) {
	repo, _ := newGitRepo(t)
	probe := &writeProbe{repo: repo, after: lockIndexAfterWrite(repo)}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{inputTokens: 1000}, probe)
	budget := 10
	config.MaxTokenBudget = &budget

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "budget_exceeded" {
		t.Fatalf("outcome = %q, want budget_exceeded kept", rt.Outcome)
	}
	if rt.Git == nil || rt.Git.Error == "" {
		t.Fatalf("RunTrace.Git = %+v, want the git failure recorded", rt.Git)
	}
}

func TestRun_DirtyTreeAfterFinalCheckpointYieldsHandoffFailed(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "handoff_failed" {
		t.Fatalf("outcome = %q, want handoff_failed", rt.Outcome)
	}
	if rt.Git == nil || !rt.Git.Dirty || rt.Git.Error != errDirtyAfterFinalCheckpoint.Error() {
		t.Errorf("RunTrace.Git = %+v, want dirty with %q", rt.Git, errDirtyAfterFinalCheckpoint)
	}
}

func TestRun_PostRunHookEditsAreNotCommitted(t *testing.T) {
	repo, _ := newGitRepo(t)
	probe := &writeProbe{repo: repo}
	loop, config, _ := newGitFinaliseLoop(t, repo, &toolLoopProvider{stopAfter: 1}, probe)
	loop.Hooks = &fakeHookRunner{onPost: func(context.Context) {
		_ = os.WriteFile(filepath.Join(repo, "hook.txt"), []byte("hook"), 0o644)
	}}

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", rt.Outcome)
	}
	if got := hostGit(t, repo, "status", "--porcelain"); got != "?? hook.txt" {
		t.Errorf("status = %q, want only the hook's file untracked", got)
	}
	if rt.Git == nil || rt.Git.Dirty {
		t.Errorf("RunTrace.Git = %+v, want the handoff taken before the hook ran", rt.Git)
	}
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
	loop := buildTestLoop(simpleSuccessProvider())
	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" || rt.Git != nil {
		t.Errorf("outcome = %q, Git = %+v; want success with no git handoff", rt.Outcome, rt.Git)
	}
}

func TestRun_FinaliseReceivesPreGitOutcome(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	fake := &fakeFinaliseGit{handoff: &types.GitHandoff{Branch: "b", SHA: "s", BaseSHA: "s"}}
	loop.Git = fake
	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.outcome != "success" {
		t.Errorf("Finalise outcome = %q, want success", fake.outcome)
	}
	if rt.Git == nil || *rt.Git != *fake.handoff {
		t.Errorf("RunTrace.Git = %+v, want %+v", rt.Git, fake.handoff)
	}
}

func TestRun_FinaliseErrorWithoutHandoffIsRecorded(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Git = &fakeFinaliseGit{err: errors.New("strategy broke")}
	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "handoff_failed" {
		t.Errorf("outcome = %q, want handoff_failed", rt.Outcome)
	}
	if rt.Git == nil || rt.Git.Error != "strategy broke" {
		t.Errorf("RunTrace.Git = %+v, want the error recorded", rt.Git)
	}
}

func TestRun_GitHandoffErrorIsScrubbedAndCapped(t *testing.T) {
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	long := "commit hook printed " + spanProbeKey + " " + strings.Repeat("x", 4*maxFailureTextBytes)
	loop.Git = &fakeFinaliseGit{handoff: &types.GitHandoff{}, err: errors.New(long)}
	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Git == nil {
		t.Fatal("RunTrace.Git = nil")
	}
	if strings.Contains(rt.Git.Error, spanProbeKey) {
		t.Error("RunTrace.Git.Error carries the unscrubbed key")
	}
	if len(rt.Git.Error) > maxFailureTextBytes+len("... [truncated by harness]") {
		t.Errorf("RunTrace.Git.Error is %d bytes, want it capped near %d", len(rt.Git.Error), maxFailureTextBytes)
	}
	for _, ev := range rec.events {
		if strings.Contains(ev.Message, spanProbeKey) {
			t.Errorf("%s event carries the unscrubbed key", ev.Type)
		}
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
