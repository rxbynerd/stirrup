package judge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
)

// fileCacheEntryVersion is the FileCache entry format version.
const fileCacheEntryVersion = 1

// maxCacheEntryBytes bounds how much of an entry file is read.
const maxCacheEntryBytes = 1 << 20

// fileCacheDirPerm keeps cache directories private to their owner; entries
// are created 0600 by os.CreateTemp.
const fileCacheDirPerm = 0o700

// errOwnerUnchecked is returned by checkPrivateDir on platforms that cannot
// read a directory's owner.
var errOwnerUnchecked = errors.New("this platform cannot check a directory's owner and permissions")

// fileCacheEntry is the on-disk form of one cached verdict.
type fileCacheEntry struct {
	Key           string            `json:"key"`
	SchemaVersion int               `json:"schemaVersion"`
	CreatedAt     time.Time         `json:"createdAt"`
	Verdict       eval.JudgeVerdict `json:"verdict"`
}

// FileCacheOptions configures NewFileCache.
type FileCacheOptions struct {
	// Mode is the cache mode the directory is opened for. Modes that write
	// create the directory and its shard directories; replay-strict needs
	// an existing directory and creates nothing.
	Mode CacheMode

	// ForbiddenRoots are directories the agent under test can write, such
	// as task workspaces. The cache directory must not lie inside any.
	ForbiddenRoots []string
}

// FileCache is a Cache holding one JSON file per verdict at
// <dir>/<key[0:2]>/<key[2:4]>/<key>.json. Entries are written to a temporary
// file and renamed into place, so a reader sees a whole entry or none.
type FileCache struct {
	dir    string
	create bool
}

var _ Cache = (*FileCache)(nil)

// NewFileCache returns a FileCache rooted at dir. It refuses a directory
// inside any of opts.ForbiddenRoots, one not owned by the current user, and
// one that is group- or world-writable, since whoever can write the
// directory decides the verdicts served from it.
func NewFileCache(dir string, opts FileCacheOptions) (*FileCache, error) {
	if dir == "" {
		return nil, errors.New("judge cache directory is empty")
	}
	mode, err := ParseCacheMode(string(opts.Mode))
	if err != nil {
		return nil, err
	}
	if mode.IsLive() {
		return nil, errors.New("judge cache mode live uses no cache directory")
	}
	planned, err := resolveAbsPath(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving judge cache directory %s: %w", dir, err)
	}
	if err := checkOutsideRoots(planned, opts.ForbiddenRoots); err != nil {
		return nil, err
	}
	if mode.writes() {
		if err := os.MkdirAll(dir, fileCacheDirPerm); err != nil {
			return nil, fmt.Errorf("creating judge cache directory: %w", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("judge cache directory %s does not exist; %s reads an existing cache and creates none", dir, mode)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving judge cache directory %s: %w", dir, err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return nil, fmt.Errorf("resolving judge cache directory %s: %w", dir, err)
	}
	if err := checkOutsideRoots(resolved, opts.ForbiddenRoots); err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("judge cache directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("judge cache directory %s is not a directory", dir)
	}
	switch err := checkPrivateDir(info); {
	case errors.Is(err, errOwnerUnchecked):
		if mode.Reads() {
			return nil, fmt.Errorf("judge cache mode %s serves stored verdicts, but %w", mode, err)
		}
	case err != nil:
		return nil, fmt.Errorf("judge cache directory %s: %w", dir, err)
	}
	return &FileCache{dir: resolved, create: mode.writes()}, nil
}

// Dir returns the cache's root directory, absolute and with symlinks
// resolved.
func (c *FileCache) Dir() string { return c.dir }

// Get implements Cache. The entry must be a regular file reached without
// following a symlink at any level below the root; anything else is an
// unusable entry rather than a miss.
func (c *FileCache) Get(key string) (eval.JudgeVerdict, bool, error) {
	if !validCacheKey(key) {
		return eval.JudgeVerdict{}, false, fmt.Errorf("invalid judge cache key %q", key)
	}
	dir, err := c.shardDir(key, false)
	if errors.Is(err, fs.ErrNotExist) {
		return eval.JudgeVerdict{}, false, nil
	}
	if err != nil {
		return eval.JudgeVerdict{}, true, err
	}
	// O_NONBLOCK keeps a FIFO planted at the entry path from blocking the
	// open; the fstat below then rejects it.
	f, err := os.OpenFile(filepath.Join(dir, key+".json"), os.O_RDONLY|entryOpenFlags, 0) //nolint:gosec // path is built from a validated hex key under checked shard directories
	if errors.Is(err, fs.ErrNotExist) {
		return eval.JudgeVerdict{}, false, nil
	}
	if err != nil {
		return eval.JudgeVerdict{}, true, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return eval.JudgeVerdict{}, true, err
	}
	if !info.Mode().IsRegular() {
		return eval.JudgeVerdict{}, true, fmt.Errorf("entry is not a regular file (%s)", info.Mode().Type())
	}
	if info.Size() > maxCacheEntryBytes {
		return eval.JudgeVerdict{}, true, fmt.Errorf("entry exceeds %d bytes", maxCacheEntryBytes)
	}
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
	if !c.create {
		return errors.New("judge cache is opened read-only")
	}
	if !validCacheKey(key) {
		return fmt.Errorf("invalid judge cache key %q", key)
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
	dir, err := c.shardDir(key, true)
	if err != nil {
		return fmt.Errorf("judge cache directory: %w", err)
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
	if err := os.Rename(tmpPath, filepath.Join(dir, key+".json")); err != nil {
		return fmt.Errorf("writing judge cache entry: %w", err)
	}
	committed = true
	return nil
}

// shardDir returns the directory holding key's entry, checking that each
// level below the root is a real directory, not a symlink, private to the
// current user. With create it makes missing levels; otherwise a missing
// level is reported as fs.ErrNotExist. A same-uid process can still swap a
// level between this check and its use.
func (c *FileCache) shardDir(key string, create bool) (string, error) {
	dir := c.dir
	for _, part := range []string{key[:2], key[2:4]} {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err = os.Mkdir(dir, fileCacheDirPerm); err == nil || errors.Is(err, fs.ErrExist) {
				info, err = os.Lstat(dir)
			}
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link", dir)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", dir)
		}
		if err := checkPrivateDir(info); err != nil && !errors.Is(err, errOwnerUnchecked) {
			return "", fmt.Errorf("%s: %w", dir, err)
		}
	}
	return dir, nil
}

// validCacheKey accepts only a lowercase hex SHA-256, so a key can never
// name a path outside the cache.
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

// checkOutsideRoots refuses a cache directory, resolved, that is or lies
// inside any of roots.
func checkOutsideRoots(dir string, roots []string) error {
	for _, root := range roots {
		if root == "" {
			continue
		}
		resolved, err := resolveAbsPath(root)
		if err != nil {
			resolved = filepath.Clean(root)
		}
		if withinDir(dir, resolved) {
			return fmt.Errorf("judge cache directory %s is inside %s, which the agent under test can write", dir, root)
		}
	}
	return nil
}

// withinDir reports whether path is root or lies inside it. Both are clean
// absolute paths.
func withinDir(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// resolveAbsPath returns p made absolute, with symlinks resolved in its
// longest existing prefix, so a directory that does not exist yet is
// compared where it will be created.
func resolveAbsPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs, nil
	}
	resolvedParent, err := resolveAbsPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}
