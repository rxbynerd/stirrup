package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunSuite_ReadThroughCacheSkipsTheModelForAnUnchangedWorkspace(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)

	runOnce := func(mode judge.CacheMode) eval.SuiteResult {
		t.Helper()
		result, err := RunSuite(context.Background(), suite, RunConfig{
			HarnessPath:  harness,
			OutputDir:    t.TempDir(),
			JudgeOptions: judge.Options{Cache: cache, CacheMode: mode},
		})
		must.NoError(err, "RunSuite")
		for _, tr := range result.Tasks {
			must.Equal("pass", tr.Outcome, "task %s: %s", tr.TaskID, tr.Error)
		}
		return result
	}
	cacheStatuses := func(result eval.SuiteResult) []string {
		return []string{result.Tasks[0].JudgeVerdict.Record.CacheStatus, result.Tasks[1].JudgeVerdict.Details[1].Record.CacheStatus}
	}

	first := runOnce(judge.CacheReadThrough)
	must.Len(stub.requests(), 2, "first run model calls")
	check.Equal([]string{types.JudgeCacheStored, types.JudgeCacheStored}, cacheStatuses(first), "first run cache statuses")
	must.NotNil(first.JudgeCache, "first run summary")
	check.Equal(eval.JudgeCacheSummary{Mode: "read-through", Misses: 2, Stored: 2}, *first.JudgeCache, "first run summary")

	second := runOnce(judge.CacheReadThrough)
	check.Len(stub.requests(), 2, "second run should make no model calls")
	check.Equal([]string{types.JudgeCacheHit, types.JudgeCacheHit}, cacheStatuses(second), "second run cache statuses")
	must.NotNil(second.JudgeCache, "second run summary")
	check.Equal(eval.JudgeCacheSummary{Mode: "read-through", Hits: 2}, *second.JudgeCache, "second run summary")
	for i := range first.Tasks {
		recorded, cached := first.Tasks[i].JudgeVerdict, second.Tasks[i].JudgeVerdict
		check.Equal(recorded.Status, cached.Status, "task %d cached status", i)
		check.Equal(recorded.Reason, cached.Reason, "task %d cached reason", i)
	}

	t.Setenv("JUDGE_E2E_KEY", "")
	strict := runOnce(judge.CacheReplayStrict)
	check.Len(stub.requests(), 2, "replay-strict run should make no model calls")
	must.NotNil(strict.JudgeCache, "replay-strict summary")
	check.Equal(2, strict.JudgeCache.Hits, "replay-strict run should hit the cache for both tasks without a credential")
}

func TestRunSuite_LiveRunsOmitTheCacheSummary(t *testing.T) {
	must := require.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{ID: "live-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	must.NoError(err, "RunSuite")

	t.Run("reports no cache summary", func(t *testing.T) {
		must := require.New(t)
		must.Nil(result.JudgeCache)
	})

	t.Run("bypasses the cache for the record", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		rec := result.Tasks[0].JudgeVerdict.Record
		must.NotNil(rec)
		check.Equal(types.JudgeCacheBypass, rec.CacheStatus)
		check.Empty(rec.CacheKey)
	})

	t.Run("omits judgeCache from the JSON", func(t *testing.T) {
		must := require.New(t)
		data, err := json.Marshal(result)
		must.NoError(err)
		var fields map[string]json.RawMessage
		must.NoError(json.Unmarshal(data, &fields))
		must.NotContains(fields, "judgeCache")
	})
}

func TestReplayRecording_ReplayStrictServesTheCacheWithoutACredential(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")
	must.NoError(os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644))
	cache, err := judge.NewFileCache(t.TempDir(), judge.FileCacheOptions{Mode: judge.CacheRecord})
	must.NoError(err)
	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}

	recorded, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheRecord}, "")
	must.NoError(err, "record replay")
	must.Equal(types.JudgeCacheStored, recorded.JudgeVerdict.Record.CacheStatus, "record replay cache status")

	t.Setenv("JUDGE_E2E_KEY", "")
	replayed, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheReplayStrict}, "")
	must.NoError(err, "strict replay")
	must.Equal("pass", replayed.Outcome, "strict replay outcome")
	must.Equal(types.JudgeCacheHit, replayed.JudgeVerdict.Record.CacheStatus, "strict replay cache status")
	check.Len(stub.requests(), 1, "judge should see only the recording call")
}

func TestRunSuite_CacheModeWithoutCacheFailsBeforeAnyHarnessRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harness := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	suite := types.EvalSuite{ID: "no-cache-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}

	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}})
	must.ErrorContains(err, "needs a cache", "missing cache should be reported")

	_, statErr := os.Stat(marker)
	check.True(os.IsNotExist(statErr), "the harness ran without the cache its judge mode needs")

	_, err = RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, DryRun: true, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}})
	check.NoError(err, "a dry run judges nothing and needs no cache")
}
