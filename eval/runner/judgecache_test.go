package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunSuite_ReadThroughCacheSkipsTheModelForAnUnchangedWorkspace(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	nested := diffReviewJudgeFor(stub)
	nested.Criteria = "the agent wrote created.txt with its output"
	suite := types.EvalSuite{
		ID: "cache-suite",
		Tasks: []types.EvalTask{
			{ID: "reviewed", Prompt: "p", Files: map[string]string{"a.txt": "seed\n"}, Judge: diffReviewJudgeFor(stub)},
			{ID: "nested", Prompt: "p", Judge: types.EvalJudge{
				Type:   "composite",
				Judges: []types.EvalJudge{{Type: "file-exists", Paths: []string{"created.txt"}}, nested},
			}},
		},
	}
	cache, err := judge.NewFileCache(filepath.Join(t.TempDir(), "judge-cache"), judge.FileCacheOptions{Mode: judge.CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	runOnce := func(mode judge.CacheMode) eval.SuiteResult {
		t.Helper()
		result, err := RunSuite(context.Background(), suite, RunConfig{
			HarnessPath:  harness,
			OutputDir:    t.TempDir(),
			JudgeOptions: judge.Options{Cache: cache, CacheMode: mode},
		})
		if err != nil {
			t.Fatalf("RunSuite: %v", err)
		}
		for _, tr := range result.Tasks {
			if tr.Outcome != "pass" {
				t.Fatalf("task %s: outcome %q (%s)", tr.TaskID, tr.Outcome, tr.Error)
			}
		}
		return result
	}
	cacheStatuses := func(result eval.SuiteResult) []string {
		return []string{result.Tasks[0].JudgeVerdict.Record.CacheStatus, result.Tasks[1].JudgeVerdict.Details[1].Record.CacheStatus}
	}

	first := runOnce(judge.CacheReadThrough)
	if n := len(stub.requests()); n != 2 {
		t.Fatalf("first run made %d model calls, want 2", n)
	}
	if got := cacheStatuses(first); got[0] != types.JudgeCacheStored || got[1] != types.JudgeCacheStored {
		t.Errorf("first run cache statuses = %v, want both stored", got)
	}
	if want := (eval.JudgeCacheSummary{Mode: "read-through", Misses: 2, Stored: 2}); first.JudgeCache == nil || *first.JudgeCache != want {
		t.Errorf("first run summary = %+v, want %+v", first.JudgeCache, want)
	}

	second := runOnce(judge.CacheReadThrough)
	if n := len(stub.requests()); n != 2 {
		t.Errorf("second run made %d model calls, want none", n-2)
	}
	if got := cacheStatuses(second); got[0] != types.JudgeCacheHit || got[1] != types.JudgeCacheHit {
		t.Errorf("second run cache statuses = %v, want both hits", got)
	}
	if want := (eval.JudgeCacheSummary{Mode: "read-through", Hits: 2}); second.JudgeCache == nil || *second.JudgeCache != want {
		t.Errorf("second run summary = %+v, want %+v", second.JudgeCache, want)
	}
	for i := range first.Tasks {
		a, b := first.Tasks[i].JudgeVerdict, second.Tasks[i].JudgeVerdict
		if a.Status != b.Status || a.Reason != b.Reason {
			t.Errorf("task %d: cached verdict %+v differs from the recorded %+v", i, b, a)
		}
	}

	t.Setenv("JUDGE_E2E_KEY", "")
	strict := runOnce(judge.CacheReplayStrict)
	if n := len(stub.requests()); n != 2 {
		t.Errorf("replay-strict run made %d model calls", n-2)
	}
	if strict.JudgeCache == nil || strict.JudgeCache.Hits != 2 {
		t.Errorf("replay-strict summary = %+v, want two hits without a credential", strict.JudgeCache)
	}
}

func TestRunSuite_LiveRunsOmitTheCacheSummary(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{ID: "live-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if result.JudgeCache != nil {
		t.Errorf("live run reported a cache summary: %+v", result.JudgeCache)
	}
	if rec := result.Tasks[0].JudgeVerdict.Record; rec == nil || rec.CacheStatus != types.JudgeCacheBypass || rec.CacheKey != "" {
		t.Errorf("record = %+v, want the cache bypassed", rec)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["judgeCache"]; ok {
		t.Error("live result JSON carries a judgeCache field")
	}
}

func TestReplayRecording_ReplayStrictServesTheCacheWithoutACredential(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")
	if err := os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache, err := judge.NewFileCache(t.TempDir(), judge.FileCacheOptions{Mode: judge.CacheRecord})
	if err != nil {
		t.Fatal(err)
	}
	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}

	recorded, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheRecord}, "")
	if err != nil || recorded.JudgeVerdict.Record.CacheStatus != types.JudgeCacheStored {
		t.Fatalf("record replay: %+v, %v", recorded, err)
	}

	t.Setenv("JUDGE_E2E_KEY", "")
	replayed, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheReplayStrict}, "")
	if err != nil || replayed.Outcome != "pass" || replayed.JudgeVerdict.Record.CacheStatus != types.JudgeCacheHit {
		t.Fatalf("strict replay: %+v, %v", replayed, err)
	}
	if n := len(stub.requests()); n != 1 {
		t.Errorf("judge saw %d requests, want only the recording call", n)
	}
}

func TestRunSuite_ACacheModeWithoutACacheFailsBeforeAnyHarnessRun(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harness := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	suite := types.EvalSuite{ID: "no-cache-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}

	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}})
	if err == nil || !strings.Contains(err.Error(), "needs a cache") {
		t.Fatalf("err = %v, want the missing cache reported", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the harness ran without the cache its judge mode needs")
	}
	if _, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, DryRun: true, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}}); err != nil {
		t.Errorf("dry run: %v; a dry run judges nothing and needs no cache", err)
	}
}
