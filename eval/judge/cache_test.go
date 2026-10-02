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

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
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

func TestCacheKey(t *testing.T) {
	cfg, input := strings.Repeat("c", 64), strings.Repeat("d", 64)
	key := CacheKey(cfg, input, 0)
	if !validCacheKey(key) {
		t.Fatalf("key %q is not a lowercase hex SHA-256", key)
	}
	if again := CacheKey(cfg, input, 0); again != key {
		t.Errorf("key is not stable: %s then %s", key, again)
	}
	for name, other := range map[string]string{
		"config hash":  CacheKey(strings.Repeat("e", 64), input, 0),
		"input hash":   CacheKey(cfg, strings.Repeat("e", 64), 0),
		"sample index": CacheKey(cfg, input, 1),
		"swapped":      CacheKey(input, cfg, 0),
		"shifted":      CacheKey(cfg+input[:1], input[1:], 0),
	} {
		if other == key {
			t.Errorf("changing the %s does not change the key", name)
		}
	}
}

func TestCacheKey_Golden(t *testing.T) {
	if got, want := CacheKey(strings.Repeat("a", 64), strings.Repeat("b", 64), 0), "da30bf31d9fa084d90ac6545e096346b2b189f7ee91d970d2eac92aec75aff19"; got != want {
		t.Errorf("CacheKey = %s, want %s", got, want)
	}
}

func TestEvaluateDiffReview_CacheKeyIgnoresTheNonceButNotTheConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	evaluate := func(j types.EvalJudge) (string, string) {
		t.Helper()
		fake := &fakeClient{resp: okResponse(verdictPass)}
		v, err := Evaluate(context.Background(), j, JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, newMemCache(), fake)})
		if err != nil {
			t.Fatal(err)
		}
		if want := CacheKey(v.Record.ConfigHash, v.Record.InputSHA256, 0); v.Record.CacheKey != want {
			t.Errorf("cache key %s, want the key of sample 0 of the recorded hashes, %s", v.Record.CacheKey, want)
		}
		return v.Record.CacheKey, promptNonce(fake.got.User)
	}

	key1, nonce1 := evaluate(diffReviewJudge())
	key2, nonce2 := evaluate(diffReviewJudge())
	if nonce1 == nonce2 {
		t.Fatal("the fence nonce repeated across calls")
	}
	if key1 != key2 || !validCacheKey(key1) {
		t.Errorf("keys %q and %q differ for the same configuration and input", key1, key2)
	}

	otherModel := diffReviewJudge()
	otherModel.LLM = &types.JudgeLLMConfig{Model: "another-model"}
	otherCriteria := diffReviewJudge()
	otherCriteria.Criteria = "a.txt gains a third line"
	temp := 0.0
	otherTemperature := diffReviewJudge()
	otherTemperature.LLM = &types.JudgeLLMConfig{Model: DefaultLLMModel, Temperature: &temp}
	for name, j := range map[string]types.EvalJudge{"model": otherModel, "criteria": otherCriteria, "temperature": otherTemperature} {
		if key, _ := evaluate(j); key == key1 {
			t.Errorf("changing the %s does not change the key", name)
		}
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
			cache := newMemCache()
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(tc.mode, cache, fake)
			opts.CacheStats = stats
			for i, s := range tc.steps {
				v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if v.Status != types.JudgeStatusPass || v.Reason != "meets the criteria" {
					t.Errorf("step %d: verdict = %+v", i, v)
				}
				if v.Record.CacheStatus != s.wantStatus || fake.calls != s.wantCalls {
					t.Errorf("step %d: cache status %q after %d model calls, want %q after %d", i, v.Record.CacheStatus, fake.calls, s.wantStatus, s.wantCalls)
				}
				if wantKey := tc.mode != CacheLive; (v.Record.CacheKey != "") != wantKey {
					t.Errorf("step %d: cache key %q, want one set: %v", i, v.Record.CacheKey, wantKey)
				}
			}
			if cache.gets != tc.wantGets || cache.puts != tc.wantPuts {
				t.Errorf("cache saw %d gets and %d puts, want %d and %d", cache.gets, cache.puts, tc.wantGets, tc.wantPuts)
			}
			if got := stats.Summary(tc.mode); got != tc.wantStats {
				t.Errorf("stats = %+v, want %+v", got, tc.wantStats)
			}
			if raw, _ := json.Marshal(stats.Summary(tc.mode)); strings.Contains(string(raw), "replaced") {
				t.Errorf("summary JSON %s carries replaced without an unusable entry", raw)
			}
		})
	}
}

func TestEvaluateDiffReview_CacheHitCarriesTheStoredVerdict(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cache := newMemCache()
	recorder := &fakeClient{resp: okResponse(verdictFail)}
	stored, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, cache, recorder)})
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.entries[stored.Record.CacheKey]
	if entry.Record == nil || entry.Record.CacheStatus != "" || entry.Record.CacheKey != "" {
		t.Errorf("stored entry carries per-evaluation cache fields: %+v", entry.Record)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	built := 0
	hit, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: Options{
		Cache: cache, CacheMode: CacheReplayStrict, ClientFactory: countingFactory(recorder, &built),
	}})
	if err != nil {
		t.Fatalf("a hit must not need the credential: %v", err)
	}
	if built != 0 || recorder.calls != 1 {
		t.Errorf("client built %d times and called %d times, want no client for a hit", built, recorder.calls)
	}
	if hit.Passed || hit.Status != types.JudgeStatusFail || hit.Reason != "no test added" {
		t.Errorf("hit verdict = %+v, want the stored fail", hit)
	}
	rec, want := hit.Record, stored.Record
	if rec.CacheStatus != types.JudgeCacheHit || rec.CacheKey != want.CacheKey ||
		rec.ServedModel != want.ServedModel || rec.InputTokens != want.InputTokens || rec.OutputTokens != want.OutputTokens ||
		rec.ParseStatus != want.ParseStatus || rec.StopReason != want.StopReason ||
		rec.ConfigHash != want.ConfigHash || rec.InputSHA256 != want.InputSHA256 || rec.BaselineSource != want.BaselineSource {
		t.Errorf("hit record = %+v\nstored record = %+v", rec, want)
	}
	if rec.LatencyMs > 1000 {
		t.Errorf("hit latency %d ms is not a lookup time", rec.LatencyMs)
	}
}

func TestEvaluateDiffReview_ReplayStrictMissIsAnErrorWithoutAModelCall(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	built := 0
	stats := &CacheStats{}
	jctx := changedJudgeContext(t, Options{Cache: cache, CacheMode: CacheReplayStrict, ClientFactory: countingFactory(fake, &built), CacheStats: stats})

	v, err := Evaluate(context.Background(), diffReviewJudge(), jctx)
	if err == nil {
		t.Fatalf("expected an error, got %+v", v)
	}
	if v.Passed || v.Status != types.JudgeStatusError {
		t.Errorf("verdict = %+v, want error status", v)
	}
	for _, want := range []string{"no verdict for key " + v.Record.CacheKey, "replay-strict never calls the model"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q should contain %q", v.Reason, want)
		}
	}
	if v.Record.CacheStatus != types.JudgeCacheMiss || !validCacheKey(v.Record.CacheKey) {
		t.Errorf("record = %+v", v.Record)
	}
	if built != 0 || fake.calls != 0 || cache.puts != 0 {
		t.Errorf("replay-strict built %d clients, made %d calls and %d puts", built, fake.calls, cache.puts)
	}
	if got := stats.Summary(CacheReplayStrict); got.Misses != 1 || got.Hits != 0 || got.Stored != 0 {
		t.Errorf("stats = %+v", got)
	}
}

func TestEvaluateDiffReview_UncacheableVerdictsAreNeverStored(t *testing.T) {
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
				cache := newMemCache()
				fake := &fakeClient{resp: tc.resp, err: tc.callErr}
				jctx := changedJudgeContext(t, cachedOptions(mode, cache, fake))
				for range 2 {
					v, err := Evaluate(context.Background(), diffReviewJudge(), jctx)
					if err == nil || v.Status != types.JudgeStatusError {
						t.Fatalf("verdict = %+v, err = %v, want an error", v, err)
					}
					if v.Record.CacheStatus != types.JudgeCacheMiss {
						t.Errorf("cache status = %q, want miss", v.Record.CacheStatus)
					}
				}
				if cache.puts != 0 || len(cache.entries) != 0 {
					t.Errorf("an error verdict was stored: %d puts", cache.puts)
				}
				if fake.calls != 2 {
					t.Errorf("model called %d times, want every attempt to reach the model", fake.calls)
				}
			})
		}
	}
}

func TestCacheableVerdict(t *testing.T) {
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
		if got := cacheableVerdict(tc.v); got != tc.want {
			t.Errorf("%s: cacheable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEvaluateDiffReview_UnusableEntries(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	recorder := &fakeClient{resp: okResponse(verdictPass)}
	good, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, newMemCache(), recorder)})
	if err != nil {
		t.Fatal(err)
	}
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
			cache := newMemCache()
			corrupt(cache)
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReadThrough, cache, fake)
			opts.CacheStats = stats
			v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			if err != nil || v.Status != types.JudgeStatusPass {
				t.Fatalf("verdict = %+v, err = %v", v, err)
			}
			if fake.calls != 1 || v.Record.CacheStatus != types.JudgeCacheStored {
				t.Errorf("%d model calls, cache status %q; want the entry treated as a miss and replaced", fake.calls, v.Record.CacheStatus)
			}
			if err := checkCachedVerdict(cache.entries[key], v.Record); err != nil || cache.corrupt[key] {
				t.Errorf("entry not replaced with a usable one: %v", err)
			}
			summary := stats.Summary(CacheReadThrough)
			if want := (eval.JudgeCacheSummary{Mode: "read-through", Misses: 1, Stored: 1, Replaced: 1}); summary != want {
				t.Errorf("stats = %+v, want %+v", summary, want)
			}
			if err := stats.UnusableError(); err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "is unusable") {
				t.Errorf("UnusableError = %v, want one naming key %s", err, key)
			}
			if raw, err := json.Marshal(summary); err != nil || !strings.Contains(string(raw), `"replaced":1`) {
				t.Errorf("summary JSON %s (err %v) lacks the replaced count", raw, err)
			}
		})
		t.Run(name+"/replay-strict errors", func(t *testing.T) {
			cache := newMemCache()
			corrupt(cache)
			fake := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReplayStrict, cache, fake)
			opts.CacheStats = stats
			v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "is unusable") || !strings.Contains(v.Reason, key) {
				t.Fatalf("verdict = %+v, err = %v, want an unusable-entry error naming the key", v, err)
			}
			if fake.calls != 0 || cache.puts != 0 {
				t.Errorf("%d model calls and %d puts in replay-strict", fake.calls, cache.puts)
			}
			if got := stats.Summary(CacheReplayStrict); got.Replaced != 0 || stats.UnusableError() != nil {
				t.Errorf("stats = %+v; replay-strict replaces nothing", got)
			}
		})
	}
}

func TestEvaluateDiffReview_CacheWriteFailureKeepsTheVerdict(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	cache := newMemCache()
	cache.putErr = errors.New("disk full")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	stats := &CacheStats{}
	opts := cachedOptions(CacheRecord, cache, fake)
	opts.CacheStats = stats

	v, err := Evaluate(context.Background(), diffReviewJudge(), changedJudgeContext(t, opts))
	if err != nil || v.Status != types.JudgeStatusPass {
		t.Fatalf("verdict = %+v, err = %v", v, err)
	}
	if v.Record.CacheStatus != types.JudgeCacheMiss {
		t.Errorf("cache status = %q, want miss for an entry that was not written", v.Record.CacheStatus)
	}
	if got := stats.Summary(CacheRecord); got.WriteErrors != 1 || got.Stored != 0 || got.Misses != 1 {
		t.Errorf("stats = %+v", got)
	}
	if err := stats.WriteError(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteError = %v", err)
	}
}

func TestEvaluateDiffReview_CacheModeNeedsACache(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	fake := &fakeClient{resp: okResponse(verdictPass)}
	for _, mode := range []CacheMode{CacheRecord, CacheReadThrough, CacheReplayStrict, "bogus"} {
		v, err := Evaluate(context.Background(), diffReviewJudge(), changedJudgeContext(t, Options{CacheMode: mode, ClientFactory: fake.factory(nil, nil)}))
		if err == nil || v.Status != types.JudgeStatusError {
			t.Errorf("%s: verdict = %+v, err = %v, want an error", mode, v, err)
		}
	}
	if fake.calls != 0 {
		t.Errorf("model called %d times under an unusable cache configuration", fake.calls)
	}
}

func TestEvaluateDiffReview_PreLookupErrorsBypassTheCache(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	stats := &CacheStats{}
	opts := cachedOptions(CacheReadThrough, cache, fake)
	opts.CacheStats = stats

	v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
	if err == nil || !strings.Contains(v.Reason, "no reviewable change") {
		t.Fatalf("verdict = %+v, err = %v, want the empty-diff error", v, err)
	}
	if v.Record.CacheStatus != types.JudgeCacheBypass || v.Record.CacheKey != "" || cache.gets != 0 {
		t.Errorf("record = %+v after %d gets, want the cache bypassed", v.Record, cache.gets)
	}
	if got := stats.Summary(CacheReadThrough); got.Bypassed != 1 {
		t.Errorf("stats = %+v", got)
	}
}

func TestComposite_ChildrenUseTheCache(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cache := newMemCache()
	fake := &fakeClient{resp: okResponse(verdictPass)}
	j := composite("all", types.EvalJudge{Type: "file-exists", Paths: []string{"a.txt"}}, diffReviewJudge())
	jctx := JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReadThrough, cache, fake)}

	statuses := make([]string, 0, 2)
	for range 2 {
		v := evaluateOK(t, j, jctx)
		if v.Status != types.JudgeStatusPass || v.Details[1].Record == nil {
			t.Fatalf("verdict = %+v", v)
		}
		statuses = append(statuses, v.Details[1].Record.CacheStatus)
	}
	if statuses[0] != types.JudgeCacheStored || statuses[1] != types.JudgeCacheHit || fake.calls != 1 {
		t.Errorf("sub-judge cache statuses %v after %d model calls, want [stored hit] after 1", statuses, fake.calls)
	}
}

func TestPreflightSuite_CacheModes(t *testing.T) {
	t.Setenv("PREFLIGHT_UNSET_KEY", "")
	tasks := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{
		Type: "diff-review", Criteria: "c",
		LLM: &types.JudgeLLMConfig{Model: "m", APIKeyRef: "secret://PREFLIGHT_UNSET_KEY", BaseURL: "https://judge.invalid"},
	}}}

	if err := PreflightSuite(context.Background(), tasks, Options{CacheMode: CacheReplayStrict}); err != nil {
		t.Errorf("replay-strict: %v, want no credential or endpoint check", err)
	}
	for _, mode := range []CacheMode{"", CacheLive, CacheRecord, CacheReadThrough} {
		err := PreflightSuite(context.Background(), tasks, Options{CacheMode: mode})
		if err == nil || !strings.Contains(err.Error(), "secret://PREFLIGHT_UNSET_KEY") {
			t.Errorf("%q: err = %v, want the unresolvable reference reported", mode, err)
		}
	}
	if err := PreflightSuite(context.Background(), tasks, Options{CacheMode: "bogus"}); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown mode: err = %v", err)
	}
	invalid := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m"}}}}
	if err := PreflightSuite(context.Background(), invalid, Options{CacheMode: CacheReplayStrict}); err == nil {
		t.Error("replay-strict skipped configuration validation")
	}
}

func TestParseCacheMode(t *testing.T) {
	for in, want := range map[string]CacheMode{"": CacheLive, "live": CacheLive, "record": CacheRecord, "read-through": CacheReadThrough, "replay-strict": CacheReplayStrict} {
		got, err := ParseCacheMode(in)
		if err != nil || got != want {
			t.Errorf("ParseCacheMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Live", "readthrough", "replay", "off"} {
		if _, err := ParseCacheMode(in); err == nil {
			t.Errorf("ParseCacheMode(%q) accepted an unknown mode", in)
		}
	}
}

func TestCacheStats_NilIsSafe(t *testing.T) {
	var s *CacheStats
	s.count(types.JudgeCacheHit)
	s.writeFailed(errors.New("x"))
}

// fileCacheVerdict is a cacheable verdict for FileCache tests.
func fileCacheVerdict(reason string) eval.JudgeVerdict {
	return eval.JudgeVerdict{Passed: true, Status: types.JudgeStatusPass, Reason: reason, Record: &types.JudgeRecord{
		SchemaVersion: types.JudgeRecordSchemaVersion, Kind: types.JudgeKindDiffReview, ParseStatus: types.JudgeParseOK,
		ConfigHash: strings.Repeat("a", 64), InputSHA256: strings.Repeat("b", 64),
	}}
}

func TestFileCache_RoundTripAndLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "judge-cache")
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey(strings.Repeat("a", 64), strings.Repeat("b", 64), 0)
	if _, found, err := c.Get(key); found || err != nil {
		t.Fatalf("empty cache: found %v, err %v", found, err)
	}
	want := fileCacheVerdict("stored reason")
	if err := c.Put(key, want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, key[:2], key[2:4], key+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("entry not at the fan-out path: %v", err)
	}
	for _, field := range []string{`"key": "` + key + `"`, `"schemaVersion": 1`, fmt.Sprintf(`"parserVersion": %d`, diffReviewParserVersion), `"createdAt"`, `"verdict"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("entry is missing %s:\n%s", field, raw)
		}
	}
	got, found, err := c.Get(key)
	if err != nil || !found {
		t.Fatalf("Get: found %v, err %v", found, err)
	}
	if got.Reason != want.Reason || got.Status != want.Status || !got.Passed || got.Record == nil || *got.Record != *want.Record {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFileCache_RejectsInvalidKeys(t *testing.T) {
	c, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "abc", "../" + strings.Repeat("a", 61), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if err := c.Put(key, fileCacheVerdict("x")); err == nil {
			t.Errorf("Put accepted key %q", key)
		}
		if _, _, err := c.Get(key); err == nil {
			t.Errorf("Get accepted key %q", key)
		}
	}
	if _, err := NewFileCache("", FileCacheOptions{Mode: CacheRecord}); err == nil {
		t.Error("NewFileCache accepted an empty directory")
	}
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
			dir := t.TempDir()
			c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, key[:2], key[2:4], key+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, found, err := c.Get(key); err == nil || !found {
				t.Errorf("found %v, err %v; want an unusable existing entry", found, err)
			}
		})
	}

	t.Run("control: a current entry is served", func(t *testing.T) {
		dir := t.TempDir()
		c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, key[:2], key[2:4], key+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(entryWith(key, 1, current, strings.Repeat("r", 1000))), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := c.Get(key); err != nil || !found {
			t.Errorf("found %v, err %v; want the entry served", found, err)
		}
	})
}

func TestFileCache_DirectoryAtTheEntryPathIsUnusable(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey("c", "i", 0)
	if err := os.MkdirAll(filepath.Join(dir, key[:2], key[2:4], key+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(key); !found || err == nil {
		t.Errorf("found %v, err %v; want an unusable entry, not a miss", found, err)
	}
}

func TestFileCache_FailedPutLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey("c", "i", 0)
	target := filepath.Join(dir, key[:2], key[2:4], key+".json")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, fileCacheVerdict("x")); err == nil {
		t.Fatal("Put succeeded over a directory")
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != key+".json" {
			t.Errorf("failed Put left %s behind", e.Name())
		}
	}
}

func TestFileCache_ConcurrentPutsOfOneKey(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
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
	for err := range errs {
		t.Error(err)
	}
	got, found, err := c.Get(key)
	if err != nil || !found || !strings.HasPrefix(got.Reason, "writer ") {
		t.Fatalf("final entry: %+v, found %v, err %v", got, found, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, key[:2], key[2:4]))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the entry", names)
	}
}

func TestEvaluateDiffReview_FileCacheCorruptEntry(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	dir, base := changedWorkspace(t)
	cacheDir := t.TempDir()
	c, err := NewFileCache(cacheDir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &fakeClient{resp: okResponse(verdictPass)}
	first, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheRecord, c, recorder)})
	if err != nil {
		t.Fatal(err)
	}
	key := first.Record.CacheKey
	path := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
	if err := os.WriteFile(path, []byte(`{"key":"`), 0o644); err != nil {
		t.Fatal(err)
	}

	strict := &fakeClient{resp: okResponse(verdictPass)}
	v, err := Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReplayStrict, c, strict)})
	if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "is unusable") || strict.calls != 0 {
		t.Fatalf("replay-strict over a corrupt entry: verdict %+v, err %v, %d calls", v, err, strict.calls)
	}

	through := &fakeClient{resp: okResponse(verdictPass)}
	v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReadThrough, c, through)})
	if err != nil || v.Record.CacheStatus != types.JudgeCacheStored || through.calls != 1 {
		t.Fatalf("read-through over a corrupt entry: verdict %+v, err %v, %d calls", v, err, through.calls)
	}
	v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: cachedOptions(CacheReplayStrict, c, strict)})
	if err != nil || v.Record.CacheStatus != types.JudgeCacheHit || strict.calls != 0 {
		t.Errorf("after the overwrite: verdict %+v, err %v, %d calls", v, err, strict.calls)
	}
}

// cacheModesWithADirectory are the modes that open a FileCache.
var cacheModesWithADirectory = []CacheMode{CacheRecord, CacheReadThrough, CacheReplayStrict}

func TestNewFileCache_RefusesADirectoryInsideAForbiddenRoot(t *testing.T) {
	workspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	for _, mode := range cacheModesWithADirectory {
		for name, dir := range map[string]string{
			"the root itself":   workspace,
			"inside the root":   filepath.Join(workspace, "nested", "judge-cache"),
			"through a symlink": filepath.Join(link, "judge-cache"),
		} {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				if mode == CacheReplayStrict {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				_, err := NewFileCache(dir, FileCacheOptions{Mode: mode, ForbiddenRoots: []string{t.TempDir(), workspace}})
				if err == nil || !strings.Contains(err.Error(), "agent under test can write") {
					t.Fatalf("err = %v, want the directory refused", err)
				}
				if mode != CacheReplayStrict && dir != workspace {
					if _, err := os.Stat(dir); !os.IsNotExist(err) {
						t.Errorf("a refused directory was created: %v", err)
					}
				}
			})
		}
	}

	beside := filepath.Join(filepath.Dir(workspace), filepath.Base(workspace)+"-cache")
	if _, err := NewFileCache(beside, FileCacheOptions{Mode: CacheRecord, ForbiddenRoots: []string{workspace}}); err != nil {
		t.Errorf("a sibling sharing the root's name as a prefix was refused: %v", err)
	}
}

func TestNewFileCache_ReplayStrictNeedsAnExistingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "judge-cache")
	_, err := NewFileCache(missing, FileCacheOptions{Mode: CacheReplayStrict})
	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want the missing directory named", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Errorf("replay-strict created %s: %v", filepath.Dir(missing), err)
	}

	file := filepath.Join(t.TempDir(), "judge-cache")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range cacheModesWithADirectory {
		if _, err := NewFileCache(file, FileCacheOptions{Mode: mode}); err == nil {
			t.Errorf("%s: a regular file was accepted as the cache directory", mode)
		}
	}
	for _, mode := range []CacheMode{"", CacheLive, "bogus"} {
		if _, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: mode}); err == nil {
			t.Errorf("mode %q opened a FileCache", mode)
		}
	}
}

func TestFileCache_ReplayStrictIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReplayStrict})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(CacheKey("c", "i", 0), fileCacheVerdict("x")); err == nil {
		t.Error("Put succeeded on a replay-strict cache")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("replay-strict Put left %v (err %v)", entries, err)
	}
}

func TestOptions_CheckCacheOutside(t *testing.T) {
	workspace := t.TempDir()
	c, err := NewFileCache(filepath.Join(workspace, "judge-cache"), FileCacheOptions{Mode: CacheRecord})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Cache: c, CacheMode: CacheReadThrough}
	if err := opts.CheckCacheOutside(workspace); err == nil || !strings.Contains(err.Error(), "agent under test can write") {
		t.Errorf("err = %v, want the cache inside the workspace refused", err)
	}
	if err := opts.CheckCacheOutside(t.TempDir()); err != nil {
		t.Errorf("a cache outside the workspace was refused: %v", err)
	}
	opts.CacheMode = CacheLive
	if err := opts.CheckCacheOutside(workspace); err != nil {
		t.Errorf("live mode checked its unused cache: %v", err)
	}
	if err := (Options{Cache: newMemCache(), CacheMode: CacheReadThrough}).CheckCacheOutside(workspace); err != nil {
		t.Errorf("a cache with no directory was refused: %v", err)
	}
}

// rewriteEntry applies edit to the JSON object stored in the entry file at
// path.
func rewriteEntry(t *testing.T, path string, edit func(entry map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	edit(entry)
	if raw, err = json.Marshal(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
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
			cacheDir := t.TempDir()
			c, err := NewFileCache(cacheDir, FileCacheOptions{Mode: CacheReadThrough})
			if err != nil {
				t.Fatal(err)
			}
			first, err := evaluate(CacheRecord, c, &fakeClient{resp: okResponse(verdictPass)})
			if err != nil {
				t.Fatal(err)
			}
			key := first.Record.CacheKey
			path := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
			rewriteEntry(t, path, edit)

			strict := &fakeClient{resp: okResponse(verdictPass)}
			v, err := evaluate(CacheReplayStrict, c, strict)
			if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "parser version") || strict.calls != 0 {
				t.Fatalf("replay-strict: verdict %+v, err %v, %d calls; want an unusable-entry error", v, err, strict.calls)
			}

			through := &fakeClient{resp: okResponse(verdictPass)}
			stats := &CacheStats{}
			opts := cachedOptions(CacheReadThrough, c, through)
			opts.CacheStats = stats
			v, err = Evaluate(context.Background(), diffReviewJudge(), JudgeContext{WorkspaceDir: dir, Baseline: &base, Options: opts})
			if err != nil || v.Record.CacheStatus != types.JudgeCacheStored || through.calls != 1 {
				t.Fatalf("read-through: verdict %+v, err %v, %d calls; want the entry judged again and replaced", v, err, through.calls)
			}
			if got := stats.Summary(CacheReadThrough); got.Replaced != 1 {
				t.Errorf("stats = %+v, want the entry counted as replaced", got)
			}
			if _, found, err := c.Get(key); !found || err != nil {
				t.Errorf("replaced entry: found %v, err %v", found, err)
			}
		})
	}
}
