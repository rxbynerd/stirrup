package judge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rxbynerd/stirrup/eval"
)

// fileCacheEntryVersion is the FileCache entry format version.
const fileCacheEntryVersion = 1

// maxCacheEntryBytes bounds how much of an entry file is read.
const maxCacheEntryBytes = 1 << 20

// fileCacheEntry is the on-disk form of one cached verdict.
type fileCacheEntry struct {
	Key           string            `json:"key"`
	SchemaVersion int               `json:"schemaVersion"`
	CreatedAt     time.Time         `json:"createdAt"`
	Verdict       eval.JudgeVerdict `json:"verdict"`
}

// FileCache is a Cache holding one JSON file per verdict at
// <dir>/<key[0:2]>/<key[2:4]>/<key>.json. Entries are written to a temporary
// file and renamed into place, so a reader sees a whole entry or none.
type FileCache struct {
	dir string
}

var _ Cache = (*FileCache)(nil)

// NewFileCache returns a FileCache rooted at dir, creating dir if needed.
func NewFileCache(dir string) (*FileCache, error) {
	if dir == "" {
		return nil, errors.New("judge cache directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating judge cache directory: %w", err)
	}
	return &FileCache{dir: dir}, nil
}

// Dir returns the cache's root directory.
func (c *FileCache) Dir() string { return c.dir }

// Get implements Cache.
func (c *FileCache) Get(key string) (eval.JudgeVerdict, bool, error) {
	path, err := c.path(key)
	if err != nil {
		return eval.JudgeVerdict{}, false, err
	}
	f, err := os.Open(path) //nolint:gosec // path is built from a validated hex key under the operator's cache dir
	if errors.Is(err, fs.ErrNotExist) {
		return eval.JudgeVerdict{}, false, nil
	}
	if err != nil {
		return eval.JudgeVerdict{}, true, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxCacheEntryBytes+1))
	if err != nil {
		return eval.JudgeVerdict{}, true, err
	}
	if len(data) > maxCacheEntryBytes {
		return eval.JudgeVerdict{}, true, fmt.Errorf("entry exceeds %d bytes", maxCacheEntryBytes)
	}
	var entry fileCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return eval.JudgeVerdict{}, true, fmt.Errorf("decoding entry: %w", err)
	}
	if entry.SchemaVersion != fileCacheEntryVersion {
		return eval.JudgeVerdict{}, true, fmt.Errorf("entry schema version %d, want %d", entry.SchemaVersion, fileCacheEntryVersion)
	}
	if entry.Key != key {
		return eval.JudgeVerdict{}, true, errors.New("entry is stored under another key")
	}
	return entry.Verdict, true, nil
}

// Put implements Cache.
func (c *FileCache) Put(key string, v eval.JudgeVerdict) error {
	path, err := c.path(key)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(fileCacheEntry{
		Key:           key,
		SchemaVersion: fileCacheEntryVersion,
		CreatedAt:     time.Now().UTC(),
		Verdict:       v,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding judge cache entry: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating judge cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("creating judge cache entry: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing judge cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing judge cache entry: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("writing judge cache entry: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("writing judge cache entry: %w", err)
	}
	committed = true
	return nil
}

// path returns the entry file for key, rejecting anything but a lowercase
// hex SHA-256 so a key can never name a path outside the cache.
func (c *FileCache) path(key string) (string, error) {
	if !validCacheKey(key) {
		return "", fmt.Errorf("invalid judge cache key %q", key)
	}
	return filepath.Join(c.dir, key[:2], key[2:4], key+".json"), nil
}

func validCacheKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := range len(key) {
		if c := key[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
