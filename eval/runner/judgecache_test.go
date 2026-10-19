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
	require.NoError(t, err)

	runOnce := func(mode judge.CacheMode) eval.SuiteResult {
		t.Helper()
		result, err := RunSuite(context.Background(), suite, RunConfig{
			HarnessPath:  harness,
			OutputDir:    t.TempDir(),
			JudgeOptions: judge.Options{Cache: cache, CacheMode: mode},
		})
		require.NoError(t, err, "RunSuite")
		for _, tr := range result.Tasks {
			require.Equal(t, "pass", tr.Outcome, "task %s: %s", tr.TaskID, tr.Error)
		}
		return result
	}
	cacheStatuses := func(result eval.SuiteResult) []string {
		return []string{result.Tasks[0].JudgeVerdict.Record.CacheStatus, result.Tasks[1].JudgeVerdict.Details[1].Record.CacheStatus}
	}

	first := runOnce(judge.CacheReadThrough)
	require.Len(t, stub.requests(), 2, "first run model calls")
	assert.Equal(t, []string{types.JudgeCacheStored, types.JudgeCacheStored}, cacheStatuses(first), "first run cache statuses")
	require.NotNil(t, first.JudgeCache, "first run summary")
	assert.Equal(t, eval.JudgeCacheSummary{Mode: "read-through", Misses: 2, Stored: 2}, *first.JudgeCache, "first run summary")

	second := runOnce(judge.CacheReadThrough)
	assert.Len(t, stub.requests(), 2, "second run should make no model calls")
	assert.Equal(t, []string{types.JudgeCacheHit, types.JudgeCacheHit}, cacheStatuses(second), "second run cache statuses")
	require.NotNil(t, second.JudgeCache, "second run summary")
	assert.Equal(t, eval.JudgeCacheSummary{Mode: "read-through", Hits: 2}, *second.JudgeCache, "second run summary")
	for i := range first.Tasks {
		recorded, cached := first.Tasks[i].JudgeVerdict, second.Tasks[i].JudgeVerdict
		assert.Equal(t, recorded.Status, cached.Status, "task %d cached status", i)
		assert.Equal(t, recorded.Reason, cached.Reason, "task %d cached reason", i)
	}

	t.Setenv("JUDGE_E2E_KEY", "")
	strict := runOnce(judge.CacheReplayStrict)
	assert.Len(t, stub.requests(), 2, "replay-strict run should make no model calls")
	require.NotNil(t, strict.JudgeCache, "replay-strict summary")
	assert.Equal(t, 2, strict.JudgeCache.Hits, "replay-strict run should hit the cache for both tasks without a credential")
}

func TestRunSuite_LiveRunsOmitTheCacheSummary(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{ID: "live-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	require.NoError(t, err, "RunSuite")

	t.Run("reports no cache summary", func(t *testing.T) {
		require.Nil(t, result.JudgeCache)
	})

	t.Run("bypasses the cache for the record", func(t *testing.T) {
		rec := result.Tasks[0].JudgeVerdict.Record
		require.NotNil(t, rec)
		assert.Equal(t, types.JudgeCacheBypass, rec.CacheStatus)
		assert.Empty(t, rec.CacheKey)
	})

	t.Run("omits judgeCache from the JSON", func(t *testing.T) {
		data, err := json.Marshal(result)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &fields))
		require.NotContains(t, fields, "judgeCache")
	})
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
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644))
	cache, err := judge.NewFileCache(t.TempDir(), judge.FileCacheOptions{Mode: judge.CacheRecord})
	require.NoError(t, err)
	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}

	recorded, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheRecord}, "")
	require.NoError(t, err, "record replay")
	require.Equal(t, types.JudgeCacheStored, recorded.JudgeVerdict.Record.CacheStatus, "record replay cache status")

	t.Setenv("JUDGE_E2E_KEY", "")
	replayed, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{Cache: cache, CacheMode: judge.CacheReplayStrict}, "")
	require.NoError(t, err, "strict replay")
	require.Equal(t, "pass", replayed.Outcome, "strict replay outcome")
	require.Equal(t, types.JudgeCacheHit, replayed.JudgeVerdict.Record.CacheStatus, "strict replay cache status")
	assert.Len(t, stub.requests(), 1, "judge should see only the recording call")
}

func TestRunSuite_CacheModeWithoutCacheFailsBeforeAnyHarnessRun(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harness := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	suite := types.EvalSuite{ID: "no-cache-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}

	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}})
	require.ErrorContains(t, err, "needs a cache", "missing cache should be reported")

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the harness ran without the cache its judge mode needs")

	_, err = RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, DryRun: true, JudgeOptions: judge.Options{CacheMode: judge.CacheReadThrough}})
	assert.NoError(t, err, "a dry run judges nothing and needs no cache")
}
