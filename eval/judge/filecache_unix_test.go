//go:build unix

package judge

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewFileCache_RefusesADirectoryOthersCanWrite(t *testing.T) {
	for _, perm := range []os.FileMode{0o777, 0o775, 0o757, 0o720, 0o702} {
		for _, mode := range cacheModesWithADirectory {
			dir := filepath.Join(t.TempDir(), "judge-cache")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, perm); err != nil {
				t.Fatal(err)
			}
			_, err := NewFileCache(dir, FileCacheOptions{Mode: mode})
			if err == nil || !strings.Contains(err.Error(), "lets other users write") {
				t.Errorf("%04o %s: err = %v, want the directory refused", perm, mode, err)
			}
		}
	}
	for _, perm := range []os.FileMode{0o700, 0o750, 0o755} {
		for _, mode := range cacheModesWithADirectory {
			dir := t.TempDir()
			if err := os.Chmod(dir, perm); err != nil {
				t.Fatal(err)
			}
			if _, err := NewFileCache(dir, FileCacheOptions{Mode: mode}); err != nil {
				t.Errorf("%04o %s: %v", perm, mode, err)
			}
		}
	}
}

func TestNewFileCache_RefusesADirectoryOwnedByAnotherUser(t *testing.T) {
	owner := fileOwner
	t.Cleanup(func() { fileOwner = owner })
	fileOwner = func(fs.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
	for _, mode := range cacheModesWithADirectory {
		_, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: mode})
		if err == nil || !strings.Contains(err.Error(), "not the current user") {
			t.Errorf("%s: err = %v, want the directory refused", mode, err)
		}
	}
}

func TestFileCache_KeepsDirectoriesAndEntriesPrivate(t *testing.T) {
	for _, umask := range []int{0o022, 0o077, 0o002} {
		old := syscall.Umask(umask)
		dir := filepath.Join(t.TempDir(), "judge-cache")
		c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheRecord})
		if err == nil {
			key := CacheKey("c", "i", 0)
			err = c.Put(key, fileCacheVerdict("x"))
			for path, want := range map[string]os.FileMode{
				dir:                                   0o700,
				filepath.Join(dir, key[:2]):           0o700,
				filepath.Join(dir, key[:2], key[2:4]): 0o700,
				filepath.Join(dir, key[:2], key[2:4], key+".json"): 0o600,
			} {
				if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != want {
					t.Errorf("umask %04o: %s has mode %v (%v), want %04o", umask, path, info.Mode().Perm(), statErr, want)
				}
			}
		}
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("umask %04o: %v", umask, err)
		}
	}
}

// entryPath returns key's entry file under dir after creating its shard
// directories.
func entryPath(t *testing.T, dir, key string) string {
	t.Helper()
	shard := filepath.Join(dir, key[:2], key[2:4])
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(shard, key+".json")
}

func TestFileCache_FIFOEntryIsUnusableWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey("c", "i", 0)
	if err := syscall.Mkfifo(entryPath(t, dir, key), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	type result struct {
		found bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		_, found, err := c.Get(key)
		done <- result{found, err}
	}()
	select {
	case r := <-done:
		if !r.found || r.err == nil || !strings.Contains(r.err.Error(), "not a regular file") {
			t.Errorf("found %v, err %v; want an unusable entry", r.found, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked on a FIFO entry")
	}
}

func TestFileCache_SymlinkedEntryIsUnusable(t *testing.T) {
	src := t.TempDir()
	writer, err := NewFileCache(src, FileCacheOptions{Mode: CacheRecord})
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey("c", "i", 0)
	if err := writer.Put(key, fileCacheVerdict("planted")); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, key[:2], key[2:4], key+".json"), entryPath(t, dir, key)); err != nil {
		t.Fatal(err)
	}
	if v, found, err := c.Get(key); !found || err == nil {
		t.Errorf("found %v, err %v, verdict %+v; want a symlinked entry refused", found, err, v)
	}
}

func TestFileCache_SymlinkedShardDirectoryIsRefused(t *testing.T) {
	for _, level := range []int{1, 2} {
		dir := t.TempDir()
		c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
		if err != nil {
			t.Fatal(err)
		}
		key := CacheKey("c", "i", 0)
		target := t.TempDir()
		link := filepath.Join(dir, key[:2])
		if level == 2 {
			if err := os.Mkdir(link, 0o700); err != nil {
				t.Fatal(err)
			}
			link = filepath.Join(link, key[2:4])
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := c.Put(key, fileCacheVerdict("x")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("level %d: Put err = %v, want the symlink refused", level, err)
		}
		if _, found, err := c.Get(key); !found || err == nil {
			t.Errorf("level %d: Get found %v, err %v; want an error", level, found, err)
		}
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Errorf("level %d: the link target holds %v (err %v)", level, entries, err)
		}
	}
}
