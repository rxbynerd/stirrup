package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

// memCache is an in-memory Cache that counts its calls.
type memCache struct {
	mu      sync.Mutex
	entries map[string]eval.JudgeVerdict
	corrupt map[string]bool
	putErr  error
	gets    int
	puts    int
}

func newMemCache() *memCache {
	return &memCache{entries: map[string]eval.JudgeVerdict{}, corrupt: map[string]bool{}}
}

func (c *memCache) Get(key string) (eval.JudgeVerdict, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.corrupt[key] {
		return eval.JudgeVerdict{}, true, errors.New("corrupt entry")
	}
	v, ok := c.entries[key]
	return v, ok, nil
}

func (c *memCache) Put(key string, v eval.JudgeVerdict) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	delete(c.corrupt, key)
	c.entries[key] = v
	return nil
}

// countingFactory hands out client and counts how often a client was built.
func countingFactory(client JudgeClient, built *int) ClientFactory {
	return func(types.JudgeLLMConfig, string) (JudgeClient, error) {
		*built++
		return client, nil
	}
}

func cachedOptions(mode CacheMode, cache Cache, fake *fakeClient) Options {
	return Options{Cache: cache, CacheMode: mode, ClientFactory: fake.factory(nil, nil)}
}

func TestCacheKey_IsStableAndSensitiveToEachInput(t *testing.T) {
	must := require.New(t)
	cfg, input := strings.Repeat("c", 64), strings.Repeat("d", 64)
	key := CacheKey(cfg, input, 0)
	must.True(validCacheKey(key), "key %q is not a lowercase hex SHA-256", key)
	must.Equal(key, CacheKey(cfg, input, 0), "key is not stable")

	for name, other := range map[string]string{
		"config hash":  CacheKey(strings.Repeat("e", 64), input, 0),
		"input hash":   CacheKey(cfg, strings.Repeat("e", 64), 0),
		"sample index": CacheKey(cfg, input, 1),
		"swapped":      CacheKey(input, cfg, 0),
		"shifted":      CacheKey(cfg+input[:1], input[1:], 0),
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.NotEqual(key, other)
		})
	}
}

func TestCacheKey_Golden(t *testing.T) {
	must := require.New(t)
	want := "da30bf31d9fa084d90ac6545e096346b2b189f7ee91d970d2eac92aec75aff19"
	must.Equal(want, CacheKey(strings.Repeat("a", 64), strings.Repeat("b", 64), 0), "CacheKey output changed for fixed inputs")
}

func TestEvaluateDiffReview_CacheKey_IgnoresNonceButNotConfig(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	evaluate := func(t *testing.T, j types.EvalJudge) (string, string) {
		t.Helper()
		must := require.New(t)
		fake := &fakeClient{resp: okResponse(verdictPass)}
		v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, newMemCache(), fake)})
		must.NoError(err)
		must.Equal(CacheKey(v.Record.ConfigHash, v.Record.InputSHA256, 0), v.Record.CacheKey, "cache key is not the key of sample 0")
		return v.Record.CacheKey, promptNonce(fake.got.User)
	}

	key1, nonce1 := evaluate(t, diffReviewJudge())
	key2, nonce2 := evaluate(t, diffReviewJudge())
	must.NotEqual(nonce1, nonce2, "the fence nonce repeated across calls")
	must.Equal(key1, key2, "keys differ for the same configuration and input")
	must.True(validCacheKey(key1), "key %q is not a lowercase hex SHA-256", key1)

	otherModel := diffReviewJudge()
	otherModel.LLM = &types.JudgeLLMConfig{Model: "another-model"}
	otherCriteria := diffReviewJudge()
	otherCriteria.Criteria = "a.txt gains a third line"
	temp := 0.0
	otherTemperature := diffReviewJudge()
	otherTemperature.LLM = &types.JudgeLLMConfig{Model: DefaultLLMModel, Temperature: &temp}
	for name, j := range map[string]types.EvalJudge{"model": otherModel, "criteria": otherCriteria, "temperature": otherTemperature} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			key, _ := evaluate(t, j)
			must.NotEqual(key1, key)
		})
	}
}

func TestEvaluateDiffReview_CacheModes(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)

	type step struct {
		wantStatus string
		wantCalls  int
	}
	cases := []struct {
		mode      CacheMode
		steps     []step
		wantGets  int
		wantPuts  int
		wantStats eval.JudgeCacheSummary
	}{
		{
			mode:      CacheLive,
			steps:     []step{{types.JudgeCacheBypass, 1}, {types.JudgeCacheBypass, 2}},
			wantStats: eval.JudgeCacheSummary{Mode: "live", Bypassed: 2},
		},
		{
			mode:      CacheRecord,
			steps:     []step{{types.JudgeCacheStored, 1}, {types.JudgeCacheStored, 2}},
			wantPuts:  2,
			wantStats: eval.JudgeCacheSummary{Mode: "record", Misses: 2, Stored: 2},
		},
		{
			mode:      CacheReadThrough,
			steps:     []step{{types.JudgeCacheStored, 1}, {types.JudgeCacheHit, 1}, {types.JudgeCacheHit, 1}},
			wantGets:  3,
			wantPuts:  1,
			wantStats: eval.JudgeCacheSummary{Mode: "read-through", Hits: 2, Misses: 1, Stored: 1},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			must := require.New(t)
			cache := newMemCache()
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(tc.mode, cache, fake)
			opts.CacheStats = stats
			for i, s := range tc.steps {
				v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
				must.NoError(err, "step %d", i)
				must.Equal(types.JudgeStatusPass, v.Status, "step %d", i)
				must.Equal("meets the criteria", v.Reason, "step %d", i)
				must.Equal(s.wantStatus, v.Record.CacheStatus, "step %d", i)
				must.Equal(s.wantCalls, fake.calls, "step %d", i)
				must.Equal(tc.mode != CacheLive, v.Record.CacheKey != "", "step %d: cache key set", i)
			}
			must.Equal(tc.wantGets, cache.gets)
			must.Equal(tc.wantPuts, cache.puts)
			must.Equal(tc.wantStats, stats.Summary(tc.mode))
			raw, _ := json.Marshal(stats.Summary(tc.mode))
			must.NotContains(string(raw), "replaced", "summary JSON %s carries replaced without an unusable entry", raw)
		})
	}
}

func TestEvaluateDiffReview_CacheHit_ServesStoredVerdict(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cache := newMemCache()
	recorder := &fakeClient{resp: okResponse(verdictFail)}
	stored, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, cache, recorder)})
	must.NoError(err)
	entry := cache.entries[stored.Record.CacheKey]
	must.NotNil(entry.Record, "stored entry has no record")
	must.Empty(entry.Record.CacheStatus, "stored entry carries a per-evaluation cache status: %+v", entry.Record)
	must.Empty(entry.Record.CacheKey, "stored entry carries a per-evaluation cache key: %+v", entry.Record)

	t.Setenv("ANTHROPIC_API_KEY", "")
	built := 0
	hit, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{
		Cache: cache, CacheMode: CacheReplayStrict, ClientFactory: countingFactory(recorder, &built),
	}})
	must.NoError(err, "a hit must not need the credential")
	must.Zero(built, "a hit must not build a client")
	must.Equal(1, recorder.calls, "a hit must not call the model")
	must.False(hit.Passed)
	must.Equal(types.JudgeStatusFail, hit.Status)
	must.Equal("no test added", hit.Reason)

	rec, want := hit.Record, stored.Record
	must.Equal(types.JudgeCacheHit, rec.CacheStatus)
	must.Equal(want.CacheKey, rec.CacheKey)
	must.Equal(want.ServedModel, rec.ServedModel)
	must.Equal(want.InputTokens, rec.InputTokens)
	must.Equal(want.OutputTokens, rec.OutputTokens)
	must.Equal(want.ParseStatus, rec.ParseStatus)
	must.Equal(want.StopReason, rec.StopReason)
	must.Equal(want.ConfigHash, rec.ConfigHash)
	must.Equal(want.InputSHA256, rec.InputSHA256)
	must.Equal(want.BaselineSource, rec.BaselineSource)
	must.LessOrEqual(rec.LatencyMs, int64(1000), "hit latency is not a lookup time")
}

func TestEvaluateDiffReview_CacheKey_SeparatesSamples(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	judgeSample := func(sample int) eval.JudgeVerdict {
		t.Helper()
		v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{
			WorkspaceDir: dir, Baseline: &base, Sample: sample, Options: cachedOptions(CacheReadThrough, cache, fake),
		})
		must.NoError(err)
		return v
	}

	keys := map[string]bool{}
	for sample := range 3 {
		v := judgeSample(sample)
		must.Equal(types.JudgeCacheStored, v.Record.CacheStatus, "sample %d", sample)
		must.Equal(CacheKey(v.Record.ConfigHash, v.Record.InputSHA256, sample), v.Record.CacheKey, "sample %d", sample)
		keys[v.Record.CacheKey] = true
	}
	must.Len(keys, 3, "distinct keys for 3 samples")
	must.Equal(3, fake.calls, "model calls for 3 samples")
	for sample := range 3 {
		v := judgeSample(sample)
		must.Equal(types.JudgeCacheHit, v.Record.CacheStatus, "repeat of sample %d", sample)
	}
	must.Equal(3, fake.calls, "model calls after repeating every sample")
}

func TestEvaluateDiffReview_ReplayStrictMiss_ErrorsWithoutModelCall(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	built := 0
	stats := &CacheStats{}
	jctx := changedJudgeContext(t, Options{Cache: cache, CacheMode: CacheReplayStrict, ClientFactory: countingFactory(fake, &built), CacheStats: stats})

	v, err := Evaluate(context.Background(), diffReviewJudge(), jctx)
	must.Error(err, "verdict = %+v", v)
	must.False(v.Passed)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "no verdict for key "+v.Record.CacheKey)
	must.Contains(v.Reason, "replay-strict never calls the model")
	must.Equal(types.JudgeCacheMiss, v.Record.CacheStatus)
	must.True(validCacheKey(v.Record.CacheKey), "record = %+v", v.Record)
	must.Zero(built, "replay-strict built a client")
	must.Zero(fake.calls, "replay-strict made a model call")
	must.Zero(cache.puts, "replay-strict stored a verdict")
	got := stats.Summary(CacheReplayStrict)
	must.Equal(1, got.Misses, "stats = %+v", got)
	must.Zero(got.Hits, "stats = %+v", got)
	must.Zero(got.Stored, "stats = %+v", got)
}

func TestEvaluateDiffReview_UncacheableVerdict_IsNeverStored(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cases := []struct {
		name    string
		resp    JudgeResponse
		callErr error
	}{
		{name: "transport error", callErr: errors.New("provider returned HTTP 503")},
		{name: "refusal", resp: JudgeResponse{StopReason: "refusal", Text: verdictPass, Model: "m"}},
		{name: "truncated output", resp: JudgeResponse{StopReason: "max_tokens", Text: verdictPass, Model: "m"}},
		{name: "other stop reason", resp: JudgeResponse{StopReason: "tool_use", Text: verdictPass, Model: "m"}},
		{name: "no json", resp: okResponse("looks fine to me")},
		{name: "schema violation", resp: okResponse(`{"nonce":"` + nonceSlot + `","verdict":"pass"}`)},
	}
	for _, mode := range []CacheMode{CacheRecord, CacheReadThrough} {
		for _, tc := range cases {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				must := require.New(t)
				cache := newMemCache()
				fake := &fakeClient{resp: tc.resp, err: tc.callErr}
				jctx := changedJudgeContext(t, cachedOptions(mode, cache, fake))
				for range 2 {
					v, err := Evaluate(context.Background(), diffReviewJudge(), jctx)
					must.Error(err, "verdict = %+v", v)
					must.Equal(types.JudgeStatusError, v.Status)
					must.Equal(types.JudgeCacheMiss, v.Record.CacheStatus)
				}
				must.Zero(cache.puts, "an error verdict was stored")
				must.Empty(cache.entries, "an error verdict was stored")
				must.Equal(2, fake.calls, "every attempt should reach the model")
			})
		}
	}
}

func TestCacheableVerdict_OnlyUsableVerdictsAreCached(t *testing.T) {
	rec := func(parse string) *types.JudgeRecord { return &types.JudgeRecord{ParseStatus: parse} }
	cases := []struct {
		name string
		v    eval.JudgeVerdict
		want bool
	}{
		{"pass ok", eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusPass, Record: rec(types.JudgeParseOK)}, true},
		{"fail last match", eval.JudgeVerdict{Status: types.JudgeStatusFail, Record: rec(types.JudgeParseLastMatch)}, true},
		{"error", eval.JudgeVerdict{Status: types.JudgeStatusError, Record: rec(types.JudgeParseOK)}, false},
		{"refusal", eval.JudgeVerdict{Status: types.JudgeStatusFail, Record: rec(types.JudgeParseRefusal)}, false},
		{"truncated output", eval.JudgeVerdict{Status: types.JudgeStatusPass, Record: rec(types.JudgeParseTruncatedOutput)}, false},
		{"no json", eval.JudgeVerdict{Status: types.JudgeStatusFail, Record: rec(types.JudgeParseNoJSON)}, false},
		{"schema violation", eval.JudgeVerdict{Status: types.JudgeStatusFail, Record: rec(types.JudgeParseSchemaViolation)}, false},
		{"empty parse status", eval.JudgeVerdict{Status: types.JudgeStatusPass, Record: rec("")}, false},
		{"no record", eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusPass}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, cacheableVerdict(tc.v))
		})
	}
}

func TestEvaluateDiffReview_UnusableEntries_ReplacedOrRejected(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	recorder := &fakeClient{resp: okResponse(verdictPass)}
	good, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, newMemCache(), recorder)})
	must.NoError(err)
	key := good.Record.CacheKey

	tamper := map[string]func(*memCache){
		"unreadable": func(c *memCache) { c.corrupt[key] = true },
		"error verdict": func(c *memCache) {
			v := cacheEntry(good)
			v.Status, v.Passed = types.JudgeStatusError, false
			c.entries[key] = v
		},
		"passed disagrees with status": func(c *memCache) {
			v := cacheEntry(good)
			v.Passed = false
			c.entries[key] = v
		},
		"another configuration": func(c *memCache) {
			v := cacheEntry(good)
			v.Record.ConfigHash = strings.Repeat("0", 64)
			c.entries[key] = v
		},
		"another input": func(c *memCache) {
			v := cacheEntry(good)
			v.Record.InputSHA256 = strings.Repeat("0", 64)
			c.entries[key] = v
		},
	}
	for name, corrupt := range tamper {
		t.Run(name+"/read-through overwrites", func(t *testing.T) {
			must := require.New(t)
			cache := newMemCache()
			corrupt(cache)
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReadThrough, cache, fake)
			opts.CacheStats = stats
			v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			must.NoError(err, "verdict = %+v", v)
			must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
			must.Equal(1, fake.calls, "the entry should be treated as a miss")
			must.Equal(types.JudgeCacheStored, v.Record.CacheStatus)
			must.NoError(checkCachedVerdict(cache.entries[key], v.Record), "entry not replaced with a usable one")
			must.False(cache.corrupt[key], "entry is still marked unreadable")
			summary := stats.Summary(CacheReadThrough)
			must.Equal(eval.JudgeCacheSummary{Mode: "read-through", Misses: 1, Stored: 1, Replaced: 1}, summary)
			unusableErr := stats.UnusableError()
			must.ErrorContains(unusableErr, key)
			must.ErrorContains(unusableErr, "is unusable")
			raw, err := json.Marshal(summary)
			must.NoError(err)
			must.Contains(string(raw), `"replaced":1`, "summary JSON %s lacks the replaced count", raw)
		})
		t.Run(name+"/replay-strict errors", func(t *testing.T) {
			must := require.New(t)
			cache := newMemCache()
			corrupt(cache)
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReplayStrict, cache, fake)
			opts.CacheStats = stats
			v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			must.Error(err, "verdict = %+v", v)
			must.Equal(types.JudgeStatusError, v.Status)
			must.Contains(v.Reason, "is unusable")
			must.Contains(v.Reason, key)
			must.Zero(fake.calls, "replay-strict made a model call")
			must.Zero(cache.puts, "replay-strict stored a verdict")
			got := stats.Summary(CacheReplayStrict)
			must.Zero(got.Replaced, "stats = %+v; replay-strict replaces nothing", got)
			must.NoError(stats.UnusableError(), "replay-strict reports no unusable entry")
		})
	}
}

func TestEvaluateDiffReview_CacheWriteFailure_KeepsVerdict(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cache := newMemCache()
	cache.putErr = errors.New("disk full")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	stats := &CacheStats{}
	opts := cachedOptions(CacheRecord, cache, fake)
	opts.CacheStats = stats

	v, err := Evaluate(context.Background(), diffReviewJudge(), changedJudgeContext(t, opts))
	must.NoError(err, "verdict = %+v", v)
	must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
	must.Equal(types.JudgeCacheMiss, v.Record.CacheStatus, "an entry that was not written reports a miss")
	got := stats.Summary(CacheRecord)
	must.Equal(1, got.WriteErrors, "stats = %+v", got)
	must.Zero(got.Stored, "stats = %+v", got)
	must.Equal(1, got.Misses, "stats = %+v", got)
	must.ErrorContains(stats.WriteError(), "disk full")
}

func TestEvaluateDiffReview_CacheMode_RequiresCache(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	for _, mode := range []CacheMode{CacheRecord, CacheReadThrough, CacheReplayStrict, "bogus"} {
		t.Run(string(mode), func(t *testing.T) {
			must := require.New(t)
			v, err := Evaluate(context.Background(), diffReviewJudge(), changedJudgeContext(t, Options{CacheMode: mode, ClientFactory: fake.factory(nil, nil)}))
			must.Error(err, "verdict = %+v", v)
			must.Equal(types.JudgeStatusError, v.Status)
		})
	}
	must.Zero(fake.calls, "model called under an unusable cache configuration")
}

func TestEvaluateDiffReview_PreLookupError_BypassesCache(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	stats := &CacheStats{}
	opts := cachedOptions(CacheReadThrough, cache, fake)
	opts.CacheStats = stats

	v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
	must.Error(err, "verdict = %+v", v)
	must.Contains(v.Reason, "no reviewable change")
	must.Equal(types.JudgeCacheBypass, v.Record.CacheStatus, "record = %+v", v.Record)
	must.Empty(v.Record.CacheKey)
	must.Zero(cache.gets, "the cache was consulted")
	got := stats.Summary(CacheReadThrough)
	must.Equal(1, got.Bypassed, "stats = %+v", got)
}

func TestComposite_Children_UseCache(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	j := composite("all", types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt"}}, diffReviewJudge())
	jctx := JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReadThrough, cache, fake)}

	statuses := make([]string, 0, 2)
	for range 2 {
		v := evaluateOK(t, j, jctx)
		must.Equal(types.JudgeStatusPass, v.Status, "verdict = %+v", v)
		must.NotNil(v.Details[1].Record, "verdict = %+v", v)
		statuses = append(statuses, v.Details[1].Record.CacheStatus)
	}
	must.Equal([]string{types.JudgeCacheStored, types.JudgeCacheHit}, statuses, "sub-judge cache statuses after %d model calls", fake.calls)
	must.Equal(1, fake.calls, "model calls")
}

func TestPreflightSuite_CacheModes(t *testing.T) {
	must := require.New(t)
	t.Setenv("PREFLIGHT_UNSET_KEY", "")
	tasks := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{
		Type: "diff-review", Criteria: "c",
		LLM: &types.JudgeLLMConfig{Model: "m", APIKeyRef: "secret://PREFLIGHT_UNSET_KEY", BaseURL: "https://judge.invalid"},
	}}}

	must.NoError(PreflightSuite(context.Background(), tasks, Options{CacheMode: CacheReplayStrict}), "replay-strict checks no credential or endpoint")
	for _, tc := range []struct {
		name string
		mode CacheMode
	}{
		{"unset", ""},
		{"live", CacheLive},
		{"record", CacheRecord},
		{"read-through", CacheReadThrough},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := PreflightSuite(context.Background(), tasks, Options{CacheMode: tc.mode})
			must.ErrorContains(err, "secret://PREFLIGHT_UNSET_KEY")
		})
	}
	err := PreflightSuite(context.Background(), tasks, Options{CacheMode: "bogus"})
	must.ErrorContains(err, "bogus")

	invalid := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m"}}}}
	must.Error(PreflightSuite(context.Background(), invalid, Options{CacheMode: CacheReplayStrict}), "replay-strict skipped configuration validation")
}

func TestParseCacheMode_AcceptsKnownRejectsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want CacheMode
	}{
		{"unset defaults to live", "", CacheLive},
		{"live", "live", CacheLive},
		{"record", "record", CacheRecord},
		{"read-through", "read-through", CacheReadThrough},
		{"replay-strict", "replay-strict", CacheReplayStrict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := ParseCacheMode(tc.in)
			must.NoError(err)
			must.Equal(tc.want, got)
		})
	}
	for _, in := range []string{"Live", "readthrough", "replay", "off"} {
		t.Run("rejects "+in, func(t *testing.T) {
			must := require.New(t)
			_, err := ParseCacheMode(in)
			must.Error(err)
		})
	}
}

func TestCacheStats_NilIsSafe(t *testing.T) {
	must := require.New(t)
	var s *CacheStats
	s.count(types.JudgeCacheHit)
	s.writeFailed(errors.New("x"))
	s.replacing(errors.New("y"))
	must.Equal(eval.JudgeCacheSummary{Mode: "read-through"}, s.Summary(CacheReadThrough))
	must.NoError(s.WriteError(), "a nil CacheStats reported an error")
	must.NoError(s.UnusableError(), "a nil CacheStats reported an error")
}

func TestOptions_CheckCacheAvailable(t *testing.T) {
	must := require.New(t)
	reviewed := []types.EvalTask{{ID: "plain", Judge: types.EvalJudge{Type: "file-exists"}}, {ID: "nested", Judge: composite("all", diffReviewJudge())}}
	unreviewed := reviewed[:1]
	for _, mode := range []CacheMode{CacheRecord, CacheReadThrough, CacheReplayStrict} {
		t.Run(string(mode), func(t *testing.T) {
			must := require.New(t)
			err := Options{CacheMode: mode}.CheckCacheAvailable(reviewed)
			must.ErrorContains(err, `task "nested"`, "without a cache")
			must.NoError(Options{CacheMode: mode, Cache: newMemCache()}.CheckCacheAvailable(reviewed), "with a cache")
			must.NoError(Options{CacheMode: mode}.CheckCacheAvailable(unreviewed), "with no diff-review judge")
		})
	}
	for _, tc := range []struct {
		name string
		mode CacheMode
	}{
		{"unset", ""},
		{"live", CacheLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NoError(Options{CacheMode: tc.mode}.CheckCacheAvailable(reviewed))
		})
	}
	must.Error(Options{CacheMode: "bogus"}.CheckCacheAvailable(reviewed), "an unknown mode passed")
}

// fileCacheVerdict is a cacheable verdict for FileCache tests.
func fileCacheVerdict(reason string) eval.JudgeVerdict {
	return eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusPass, Reason: reason, Record: &types.JudgeRecord{
		SchemaVersion: types.JudgeRecordSchemaVersion, Kind: types.JudgeKindDiffReview, ParseStatus: types.JudgeParseOK,
		ConfigHash: strings.Repeat("a", 64), InputSHA256: strings.Repeat("b", 64),
	}}
}

func TestFileCache_RoundTripAndLayout(t *testing.T) {
	must := require.New(t)
	dir := filepath.Join(t.TempDir(), "judge-cache")
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	key := CacheKey(strings.Repeat("a", 64), strings.Repeat("b", 64), 0)
	_, found, err := c.Get(key)
	must.NoError(err)
	must.False(found, "empty cache reported an entry")

	want := fileCacheVerdict("stored reason")
	before := time.Now().UTC().Truncate(time.Second)
	must.NoError(c.Put(key, want))

	path := filepath.Join(dir, key[:2], key[2:4], key+".json")
	raw, err := os.ReadFile(path)
	must.NoError(err, "entry not at the fan-out path")
	for _, field := range []string{`"key": "` + key + `"`, `"schemaVersion": 1`, fmt.Sprintf(`"parserVersion": %d`, diffReviewParserVersion), `"evalVersion": "` + version.Full() + `"`, `"createdAt"`, `"verdict"`} {
		must.Contains(string(raw), field, "entry is missing %s:\n%s", field, raw)
	}

	got, found, err := c.Get(key)
	must.NoError(err)
	must.True(found, "stored entry not found")
	must.NotNil(got.Record, "entry has no record")
	must.False(got.Record.CacheRecordedAt.Before(before), "entry recorded at %v, before %v", got.Record.CacheRecordedAt, before)
	must.Equal(version.Full(), got.Record.CacheRecordedBy, "entry lacks the record provenance")

	rec := *got.Record
	rec.CacheRecordedAt, rec.CacheRecordedBy = time.Time{}, ""
	must.Equal(want.Reason, got.Reason)
	must.Equal(want.Status, got.Status)
	must.True(got.Passed)
	must.Equal(*want.Record, rec)
}

func TestFileCache_RejectsInvalidKeys(t *testing.T) {
	must := require.New(t)
	c, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"three characters", "abc"},
		{"six characters", "abcdef"},
		{"63 characters", strings.Repeat("a", 63)},
		{"65 characters", strings.Repeat("a", 65)},
		{"path traversal", "../" + strings.Repeat("a", 61)},
		{"uppercase hex", strings.Repeat("A", 64)},
		{"non-hex characters", strings.Repeat("g", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Error(c.Put(tc.key, fileCacheVerdict("x")), "Put accepted the key")
			_, _, err := c.Get(tc.key)
			must.Error(err, "Get accepted the key")
		})
	}
	_, err = NewFileCache("", FileCacheOptions{Mode: CacheRecord})
	must.Error(err, "NewFileCache accepted an empty directory")
}

func TestFileCache_UnusableEntriesAreErrors(t *testing.T) {
	key := CacheKey("c", "i", 0)
	other := CacheKey("c", "i", 1)
	entryWith := func(k string, version int, parser, reason string) string {
		return fmt.Sprintf(`{"key":%q,"schemaVersion":%d,%s"createdAt":"2026-10-02T00:00:00Z","verdict":{"passed":true,"status":"pass","reason":%q}}`, k, version, parser, reason)
	}
	current := fmt.Sprintf(`"parserVersion":%d,`, diffReviewParserVersion)
	entry := func(k string, version int) string { return entryWith(k, version, current, "r") }
	cases := map[string]string{
		"not json":             "{not json",
		"empty":                "",
		"another key":          entry(other, 1),
		"unknown version":      entry(key, 2),
		"older parser version": entryWith(key, 1, fmt.Sprintf(`"parserVersion":%d,`, diffReviewParserVersion-1), "r"),
		"newer parser version": entryWith(key, 1, fmt.Sprintf(`"parserVersion":%d,`, diffReviewParserVersion+1), "r"),
		"no parser version":    entryWith(key, 1, "", "r"),
		"oversized":            entryWith(key, 1, current, strings.Repeat("r", maxCacheEntryBytes)),
		"truncated object":     entry(key, 1)[:40],
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			dir := t.TempDir()
			c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
			must.NoError(err)
			path := filepath.Join(dir, key[:2], key[2:4], key+".json")
			must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
			must.NoError(os.WriteFile(path, []byte(content), 0o644))
			_, found, err := c.Get(key)
			must.True(found, "unusable entry reported as a miss")
			must.Error(err, "unusable entry was served")
		})
	}

	t.Run("control: a current entry is served", func(t *testing.T) {
		must := require.New(t)
		dir := t.TempDir()
		c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
		must.NoError(err)
		path := filepath.Join(dir, key[:2], key[2:4], key+".json")
		must.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		must.NoError(os.WriteFile(path, []byte(entryWith(key, 1, current, strings.Repeat("r", 1000))), 0o600))
		_, found, err := c.Get(key)
		must.NoError(err, "want the entry served")
		must.True(found, "want the entry served")
	})
}

func TestFileCache_DirectoryAtTheEntryPathIsUnusable(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	key := CacheKey("c", "i", 0)
	must.NoError(os.MkdirAll(filepath.Join(dir, key[:2], key[2:4], key+".json"), 0o700))
	_, found, err := c.Get(key)
	must.True(found, "a directory at the entry path was a miss, want an unusable entry")
	must.Error(err, "a directory at the entry path was served")
}

func TestFileCache_FailedPutLeavesNoPartialFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	key := CacheKey("c", "i", 0)
	target := filepath.Join(dir, key[:2], key[2:4], key+".json")
	must.NoError(os.MkdirAll(filepath.Join(target, "occupied"), 0o755))
	must.Error(c.Put(key, fileCacheVerdict("x")), "Put succeeded over a directory")

	entries, err := os.ReadDir(filepath.Dir(target))
	must.NoError(err)
	var leftovers []string
	for _, e := range entries {
		if e.Name() != key+".json" {
			leftovers = append(leftovers, e.Name())
		}
	}
	must.Empty(leftovers, "failed Put left files beside the entry")
}

func TestFileCache_ConcurrentPutsOfOneKey(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	key := CacheKey("c", "i", 0)
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for i := range writers {
		wg.Go(func() {
			if err := c.Put(key, fileCacheVerdict(fmt.Sprintf("writer %d", i))); err != nil {
				errs <- err
			}
			if _, found, err := c.Get(key); err != nil || !found {
				errs <- fmt.Errorf("concurrent Get: found %v, err %v", found, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	must.NoError(errors.Join(failures...), "concurrent cache operations")

	got, found, err := c.Get(key)
	must.NoError(err, "final Get")
	must.True(found, "final entry not found")
	must.True(strings.HasPrefix(got.Reason, "writer "), "final entry %+v", got)

	entries, err := os.ReadDir(filepath.Join(dir, key[:2], key[2:4]))
	must.NoError(err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	must.Len(names, 1, "directory holds %v, want only the entry", names)
}

func TestEvaluateDiffReview_CorruptFileCacheEntry_IsRejectedThenReplaced(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cacheDir := t.TempDir()
	c, err := NewFileCache(cacheDir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	recorder := &fakeClient{resp: okResponse(verdictPass)}
	first, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, c, recorder)})
	must.NoError(err)
	key := first.Record.CacheKey
	path := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
	must.NoError(os.WriteFile(path, []byte(`{"key":"`), 0o644))

	strict := &fakeClient{resp: okResponse(verdictPass)}
	v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReplayStrict, c, strict)})
	must.Error(err, "replay-strict served a corrupt entry")
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "is unusable")
	must.Zero(strict.calls, "replay-strict called the model")

	through := &fakeClient{resp: okResponse(verdictPass)}
	v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReadThrough, c, through)})
	must.NoError(err, "read-through over a corrupt entry")
	must.Equal(types.JudgeCacheStored, v.Record.CacheStatus)
	must.Equal(1, through.calls)

	v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReplayStrict, c, strict)})
	must.NoError(err, "replay-strict after the overwrite")
	must.Equal(types.JudgeCacheHit, v.Record.CacheStatus)
	must.Zero(strict.calls, "replay-strict called the model after the overwrite")
}

// cacheModesWithADirectory are the modes that open a FileCache.
var cacheModesWithADirectory = []CacheMode{CacheRecord, CacheReadThrough, CacheReplayStrict}

func TestNewFileCache_RefusesDirectoryInsideForbiddenRoot(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	must.NoError(os.Symlink(workspace, link))
	for _, mode := range cacheModesWithADirectory {
		for name, dir := range map[string]string{
			"the root itself":   workspace,
			"inside the root":   filepath.Join(workspace, "nested", "judge-cache"),
			"through a symlink": filepath.Join(link, "judge-cache"),
		} {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				must := require.New(t)
				if mode == CacheReplayStrict {
					must.NoError(os.MkdirAll(dir, 0o700))
				}
				_, err := NewFileCache(dir, FileCacheOptions{Mode: mode, ForbiddenRoots: []string{t.TempDir(), workspace}})
				must.ErrorContains(err, "agent under test can write", "want the directory refused")
				if mode != CacheReplayStrict && dir != workspace {
					_, err = os.Stat(dir)
					must.ErrorIs(err, os.ErrNotExist, "a refused directory was created")
				}
			})
		}
	}

	beside := filepath.Join(filepath.Dir(workspace), filepath.Base(workspace)+"-cache")
	_, err := NewFileCache(beside, FileCacheOptions{Mode: CacheRecord, ForbiddenRoots: []string{workspace}})
	must.NoError(err, "a sibling sharing the root's name as a prefix was refused")
}

func TestNewFileCache_ReplayStrictNeedsAnExistingDirectory(t *testing.T) {
	must := require.New(t)
	missing := filepath.Join(t.TempDir(), "absent", "judge-cache")
	_, err := NewFileCache(missing, FileCacheOptions{Mode: CacheReplayStrict})
	must.ErrorContains(err, missing, "want the missing directory named")
	must.ErrorContains(err, "does not exist")
	_, err = os.Stat(filepath.Dir(missing))
	must.ErrorIs(err, os.ErrNotExist, "replay-strict created %s", filepath.Dir(missing))

	file := filepath.Join(t.TempDir(), "judge-cache")
	must.NoError(os.WriteFile(file, nil, 0o600))
	for _, mode := range cacheModesWithADirectory {
		t.Run("regular file/"+string(mode), func(t *testing.T) {
			must := require.New(t)
			_, err := NewFileCache(file, FileCacheOptions{Mode: mode})
			must.Error(err, "a regular file was accepted as the cache directory")
		})
	}
	for _, mode := range []CacheMode{"", CacheLive, "bogus"} {
		t.Run(fmt.Sprintf("rejects mode %q", mode), func(t *testing.T) {
			must := require.New(t)
			_, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: mode})
			must.Error(err, "mode %q opened a FileCache", mode)
		})
	}
}

func TestFileCache_ReplayStrictIsReadOnly(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReplayStrict})
	must.NoError(err)
	must.Error(c.Put(CacheKey("c", "i", 0), fileCacheVerdict("x")), "Put succeeded on a replay-strict cache")

	entries, err := os.ReadDir(dir)
	must.NoError(err)
	must.Empty(entries, "replay-strict Put left entries")
}

func TestOptions_CheckCacheOutside_RefusesCacheInsideWorkspace(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	c, err := NewFileCache(filepath.Join(workspace, "judge-cache"), FileCacheOptions{Mode: CacheRecord})
	must.NoError(err)
	opts := Options{Cache: c, CacheMode: CacheReadThrough}

	t.Run("cache inside workspace is refused", func(t *testing.T) {
		must := require.New(t)
		must.ErrorContains(opts.CheckCacheOutside(workspace), "agent under test can write")
	})
	t.Run("cache outside workspace is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError(opts.CheckCacheOutside(t.TempDir()))
	})
	t.Run("live mode ignores its unused cache", func(t *testing.T) {
		must := require.New(t)
		live := opts
		live.CacheMode = CacheLive
		must.NoError(live.CheckCacheOutside(workspace))
	})
	t.Run("cache without a directory is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError((Options{Cache: newMemCache(), CacheMode: CacheReadThrough}).CheckCacheOutside(workspace))
	})
}

// rewriteEntry applies edit to the JSON object stored in the entry file at
// path.
func rewriteEntry(t *testing.T, path string, edit func(entry map[string]any)) {
	t.Helper()
	must := require.New(t)
	raw, err := os.ReadFile(path)
	must.NoError(err)
	var entry map[string]any
	must.NoError(json.Unmarshal(raw, &entry))
	edit(entry)
	raw, err = json.Marshal(entry)
	must.NoError(err)
	must.NoError(os.WriteFile(path, raw, 0o600))
}

func TestEvaluateDiffReview_EntriesFromAnotherParserVersionAreNotServed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	evaluate := func(mode CacheMode, c Cache, fake *fakeClient) (eval.JudgeVerdict, error) {
		return Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(mode, c, fake)})
	}
	for name, edit := range map[string]func(map[string]any){
		"older parser version": func(e map[string]any) { e["parserVersion"] = diffReviewParserVersion - 1 },
		"no parser version":    func(e map[string]any) { delete(e, "parserVersion") },
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			cacheDir := t.TempDir()
			c, err := NewFileCache(cacheDir, FileCacheOptions{Mode: CacheReadThrough})
			must.NoError(err)
			first, err := evaluate(CacheRecord, c, &fakeClient{resp: okResponse(verdictPass)})
			must.NoError(err)
			key := first.Record.CacheKey
			path := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
			rewriteEntry(t, path, edit)

			strict := &fakeClient{resp: okResponse(verdictPass)}
			v, err := evaluate(CacheReplayStrict, c, strict)
			must.Error(err, "replay-strict served the entry")
			must.Equal(types.JudgeStatusError, v.Status)
			must.Contains(v.Reason, "parser version")
			must.Zero(strict.calls, "replay-strict called the model")

			through := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReadThrough, c, through)
			opts.CacheStats = stats
			v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			must.NoError(err, "read-through over the entry")
			must.Equal(types.JudgeCacheStored, v.Record.CacheStatus, "want the entry judged again")
			must.Equal(1, through.calls)
			must.Equal(1, stats.Summary(CacheReadThrough).Replaced, "want the entry counted as replaced")

			_, found, err := c.Get(key)
			must.NoError(err, "replaced entry")
			must.True(found, "replaced entry not found")
		})
	}
}

func TestEvaluateDiffReview_HitsCarrySanitisedProvenance(t *testing.T) {
	must := require.New(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cacheDir := t.TempDir()
	c, err := NewFileCache(cacheDir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	evaluate := func(mode CacheMode) eval.JudgeVerdict {
		t.Helper()
		v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(mode, c, &fakeClient{resp: okResponse(verdictPass)})})
		must.NoError(err)
		return v
	}
	recordFields := func(rec *types.JudgeRecord) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(rec)
		must.NoError(err)
		var fields map[string]json.RawMessage
		must.NoError(json.Unmarshal(raw, &fields))
		return fields
	}

	stored := evaluate(CacheRecord)
	storedFields := recordFields(stored.Record)
	must.NotContains(storedFields, "cacheRecordedAt", "a stored verdict's record carries cacheRecordedAt")
	must.NotContains(storedFields, "cacheRecordedBy", "a stored verdict's record carries cacheRecordedBy")

	key := stored.Record.CacheKey
	var createdAt time.Time
	rewriteEntry(t, filepath.Join(cacheDir, key[:2], key[2:4], key+".json"), func(e map[string]any) {
		e["evalVersion"] = "v9\x1b[2J"
		rec := e["verdict"].(map[string]any)["record"].(map[string]any)
		rec["servedModel"] = "\x1b[31mevil\x1b[0m"
		rec["stopReason"] = "end_turn\n\x00" + strings.Repeat("s", 2*maxRecordFieldBytes)
		must.NoError(createdAt.UnmarshalText([]byte(e["createdAt"].(string))))
	})

	hit := evaluate(CacheReplayStrict)
	rec := hit.Record
	must.Equal(types.JudgeCacheHit, rec.CacheStatus, "want a hit, got record %+v", rec)
	for name, got := range map[string]string{"servedModel": rec.ServedModel, "stopReason": rec.StopReason, "cacheRecordedBy": rec.CacheRecordedBy} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.False(strings.ContainsFunc(got, unicode.IsControl), "%s = %q, want control characters stripped", name, got)
			must.LessOrEqual(len(got), maxRecordFieldBytes+len("..."), "%s = %q, want the length capped", name, got)
		})
	}
	must.Equal("[31mevil [0m", rec.ServedModel, "servedModel")
	must.Equal("v9 [2J", rec.CacheRecordedBy, "cacheRecordedBy")
	must.True(rec.CacheRecordedAt.Equal(createdAt), "cacheRecordedAt = %v, want the entry's createdAt %v", rec.CacheRecordedAt, createdAt)

	hitFields := recordFields(rec)
	must.Contains(hitFields, "cacheRecordedAt", "a hit's record JSON lacks cacheRecordedAt")
	must.Contains(hitFields, "cacheRecordedBy", "a hit's record JSON lacks cacheRecordedBy")
}

func TestEvaluateDiffReview_DocumentedCacheableVerdicts(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	recordThenServe := func(t *testing.T, j types.EvalJudge, jctx JudgeContext, reply string) (stored, hit eval.JudgeVerdict) {
		t.Helper()
		must := require.New(t)
		t.Setenv("ANTHROPIC_API_KEY", "k")
		c, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: CacheRecord})
		must.NoError(err)
		jctx.Options = cachedOptions(CacheRecord, c, &fakeClient{resp: okResponse(reply)})
		stored, err = Evaluate(context.Background(), j, jctx)
		must.NoError(err, "record: verdict %+v", stored)
		must.Equal(types.JudgeCacheStored, stored.Record.CacheStatus, "record: want the verdict stored")

		t.Setenv("ANTHROPIC_API_KEY", "")
		strict := &fakeClient{resp: okResponse(reply)}
		jctx.Options = cachedOptions(CacheReplayStrict, c, strict)
		hit, err = Evaluate(context.Background(), j, jctx)
		must.NoError(err, "replay-strict: verdict %+v", hit)
		must.Equal(types.JudgeCacheHit, hit.Record.CacheStatus, "replay-strict: want a hit")
		must.Zero(strict.calls, "replay-strict called the model")
		return stored, hit
	}

	t.Run("last_match", func(t *testing.T) {
		must := require.New(t)
		stored, hit := recordThenServe(t, diffReviewJudge(), changedJudgeContext(t, Options{}), verdictFail+"\n"+verdictFail)
		must.Equal(types.JudgeParseLastMatch, stored.Record.ParseStatus, "stored verdict parse status")
		must.Equal(types.JudgeParseLastMatch, hit.Record.ParseStatus, "served verdict parse status")
		must.Equal(types.JudgeStatusFail, hit.Status, "served verdict status")
	})

	t.Run("truncated input with allow_truncated", func(t *testing.T) {
		must := require.New(t)
		dir, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
		writeFiles(t, dir, map[string]string{"big.txt": strings.Repeat("padding line\n", 500)})
		j := diffReviewJudge()
		j.LLM = &types.JudgeLLMConfig{Model: "m", MaxInputBytes: 400, AllowTruncated: true}
		_, hit := recordThenServe(t, j, JudgeContext{WorkspaceDir: dir, Baseline: &base}, verdictPass)
		must.True(hit.Record.Truncated, "served hit %+v: want the truncated flag", hit)
		must.Equal(types.JudgeStatusPass, hit.Status)
		must.Greater(hit.Record.InputBytes, 400)
	})
}
