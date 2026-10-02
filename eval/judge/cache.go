package judge

// The judge cache is documented in docs/eval.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// CacheMode selects how diff-review judges use Options.Cache.
type CacheMode string

const (
	// CacheLive neither reads nor writes the cache.
	CacheLive CacheMode = "live"

	// CacheRecord calls the model for every verdict and stores it.
	CacheRecord CacheMode = "record"

	// CacheReadThrough serves stored verdicts and, on a miss, calls the
	// model and stores its verdict.
	CacheReadThrough CacheMode = "read-through"

	// CacheReplayStrict serves stored verdicts and never calls the model:
	// a miss is an error verdict.
	CacheReplayStrict CacheMode = "replay-strict"
)

// CacheModes returns the accepted cache modes.
func CacheModes() []CacheMode {
	return []CacheMode{CacheLive, CacheRecord, CacheReadThrough, CacheReplayStrict}
}

// ParseCacheMode returns the mode named s. Empty is CacheLive.
func ParseCacheMode(s string) (CacheMode, error) {
	if s == "" {
		return CacheLive, nil
	}
	m := CacheMode(s)
	if !slices.Contains(CacheModes(), m) {
		return "", fmt.Errorf("unknown judge cache mode %q (want live, record, read-through or replay-strict)", s)
	}
	return m, nil
}

// IsLive reports whether m leaves the cache unused.
func (m CacheMode) IsLive() bool { return m == "" || m == CacheLive }

// Reads reports whether m serves stored verdicts.
func (m CacheMode) Reads() bool { return m == CacheReadThrough || m == CacheReplayStrict }

func (m CacheMode) writes() bool { return m == CacheRecord || m == CacheReadThrough }

// Cache stores diff-review verdicts under content-addressed keys (see
// CacheKey). Implementations must be safe for concurrent use.
type Cache interface {
	// Get returns the verdict stored under key. found is false, with a nil
	// error, when key has no entry; a non-nil error means an entry exists
	// but cannot be read or decoded. The record may carry the entry's
	// provenance in CacheRecordedAt and CacheRecordedBy.
	Get(key string) (v eval.JudgeVerdict, found bool, err error)

	// Put stores v under key, replacing any existing entry.
	Put(key string, v eval.JudgeVerdict) error
}

// cacheKeyVersion separates this key derivation from any later one.
const cacheKeyVersion = "stirrup-judge-cache/v1"

// CacheKey is the content address of the verdict a judge configuration
// (JudgeRecord.ConfigHash) gives an input (JudgeRecord.InputSHA256). sample
// distinguishes repeated judgments of the same input; single-sample judges
// use 0. Each value is quoted, so no two distinct triples share a preimage.
func CacheKey(configHash, inputSHA256 string, sample int) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n%q\n%q\n%d\n", cacheKeyVersion, configHash, inputSHA256, sample)
	return hex.EncodeToString(h.Sum(nil))
}

// cacheMode returns the mode diff-review judges run in. A mode other than
// live without a Cache is an error rather than a silent live run.
func (o Options) cacheMode() (CacheMode, error) {
	mode, err := ParseCacheMode(string(o.CacheMode))
	if err != nil {
		return "", err
	}
	if mode != CacheLive && o.Cache == nil {
		return "", fmt.Errorf("judge cache mode %q needs a cache", mode)
	}
	return mode, nil
}

// CheckCacheAvailable returns an error when o's cache mode needs a cache
// that o lacks and tasks contain a diff-review judge, so the run fails
// before any agent runs rather than on every judged task after it.
func (o Options) CheckCacheAvailable(tasks []types.EvalTask) error {
	mode, err := ParseCacheMode(string(o.CacheMode))
	if err != nil {
		return err
	}
	if mode.IsLive() || o.Cache != nil {
		return nil
	}
	for _, task := range tasks {
		if ContainsType(task.Judge, "diff-review") {
			return fmt.Errorf("task %q: judge cache mode %q needs a cache", task.ID, mode)
		}
	}
	return nil
}

// CheckCacheOutside returns an error when o's cache is stored in dir or
// inside it. Callers pass each directory the agent under test can write,
// once it exists.
func (o Options) CheckCacheOutside(dir string) error {
	if o.CacheMode.IsLive() {
		return nil
	}
	c, ok := o.Cache.(interface{ Dir() string })
	if !ok {
		return nil
	}
	return checkOutsideRoots(c.Dir(), []string{dir})
}

// cacheableVerdict reports whether v may be stored: a pass or fail parsed
// from a conforming reply. Errors, refusals, truncated output, and every
// other parse status are never cached.
func cacheableVerdict(v eval.JudgeVerdict) bool {
	if v.Record == nil {
		return false
	}
	if v.Status != types.JudgeStatusPass && v.Status != types.JudgeStatusFail {
		return false
	}
	return v.Record.ParseStatus == types.JudgeParseOK || v.Record.ParseStatus == types.JudgeParseLastMatch
}

// cacheEntry is the form of v that is stored: the record without the
// per-evaluation and provenance cache fields.
func cacheEntry(v eval.JudgeVerdict) eval.JudgeVerdict {
	rec := *v.Record
	rec.CacheStatus, rec.CacheKey = "", ""
	rec.CacheRecordedAt, rec.CacheRecordedBy = time.Time{}, ""
	v.Record = &rec
	return v
}

// checkCachedVerdict rejects an entry that is not a cacheable verdict, is
// internally inconsistent, or was recorded for another configuration or
// input than rec's.
func checkCachedVerdict(cached eval.JudgeVerdict, rec *types.JudgeRecord) error {
	switch {
	case !cacheableVerdict(cached):
		return errors.New("entry is not a pass or fail verdict from a conforming reply")
	case cached.Passed != (cached.Status == types.JudgeStatusPass):
		return errors.New("entry's passed and status disagree")
	case cached.Record.ConfigHash != rec.ConfigHash || cached.Record.InputSHA256 != rec.InputSHA256:
		return errors.New("entry was recorded for another judge configuration or input")
	}
	return nil
}

// lookupVerdict serves rec's verdict from cache, completing rec from the
// stored record. found reports whether an entry exists. A nil error with
// found is a hit; otherwise the error says why the entry is missing or
// unusable.
func lookupVerdict(cache Cache, key string, rec *types.JudgeRecord) (v eval.JudgeVerdict, found bool, err error) {
	start := time.Now()
	cached, found, err := cache.Get(key)
	if err == nil && found {
		err = checkCachedVerdict(cached, rec)
	}
	switch {
	case err != nil:
		return eval.JudgeVerdict{}, true, fmt.Errorf("judge cache entry %s is unusable: %w", key, err)
	case !found:
		return eval.JudgeVerdict{}, false, fmt.Errorf("judge cache has no verdict for key %s", key)
	}
	rec.ServedModel = printableText(cached.Record.ServedModel, maxRecordFieldBytes)
	rec.InputTokens = cached.Record.InputTokens
	rec.OutputTokens = cached.Record.OutputTokens
	rec.StopReason = printableText(cached.Record.StopReason, maxRecordFieldBytes)
	rec.ParseStatus = cached.Record.ParseStatus
	rec.LatencyMs = time.Since(start).Milliseconds()
	rec.CacheStatus = types.JudgeCacheHit
	rec.CacheRecordedAt = cached.Record.CacheRecordedAt
	rec.CacheRecordedBy = printableText(cached.Record.CacheRecordedBy, maxRecordFieldBytes)
	return eval.JudgeVerdict{
		Passed: cached.Status == types.JudgeStatusPass,
		Status: cached.Status,
		Reason: verdictReason(cached.Reason),
		Record: rec,
	}, true, nil
}

// CacheStats counts the cache status of diff-review verdicts across
// concurrent judge calls. The zero value is ready to use; a nil *CacheStats
// counts nothing.
type CacheStats struct {
	hits, misses, stored, bypassed, replaced, writeErrors atomic.Int64

	mu                    sync.Mutex
	writeErr, unusableErr error
}

func (s *CacheStats) count(status string) {
	if s == nil {
		return
	}
	switch status {
	case types.JudgeCacheHit:
		s.hits.Add(1)
	case types.JudgeCacheMiss:
		s.misses.Add(1)
	case types.JudgeCacheStored:
		s.stored.Add(1)
	case types.JudgeCacheBypass:
		s.bypassed.Add(1)
	}
}

func (s *CacheStats) writeFailed(err error) {
	if s == nil {
		return
	}
	s.writeErrors.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr == nil {
		s.writeErr = err
	}
}

// replacing counts an unusable entry that read-through judges again in its
// place.
func (s *CacheStats) replacing(err error) {
	if s == nil {
		return
	}
	s.replaced.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unusableErr == nil {
		s.unusableErr = err
	}
}

// Summary returns the counts so far, labelled with mode.
func (s *CacheStats) Summary(mode CacheMode) eval.JudgeCacheSummary {
	if s == nil {
		return eval.JudgeCacheSummary{Mode: string(mode)}
	}
	stored := int(s.stored.Load())
	return eval.JudgeCacheSummary{
		Mode:        string(mode),
		Hits:        int(s.hits.Load()),
		Misses:      int(s.misses.Load()) + stored,
		Stored:      stored,
		Bypassed:    int(s.bypassed.Load()),
		Replaced:    int(s.replaced.Load()),
		WriteErrors: int(s.writeErrors.Load()),
	}
}

// WriteError returns the first error the cache returned from Put, or nil.
func (s *CacheStats) WriteError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// UnusableError returns why the first replaced entry was unusable, or nil.
func (s *CacheStats) UnusableError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unusableErr
}
