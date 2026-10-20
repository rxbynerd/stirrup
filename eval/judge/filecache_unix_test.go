//go:build unix

package judge

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewFileCache_RefusesDirectoryWritableByOthers(t *testing.T) {
	for _, perm := range []os.FileMode{0o777, 0o775, 0o757, 0o720, 0o702} {
		for _, mode := range cacheModesWithADirectory {
			t.Run(fmt.Sprintf("refuses perm %04o mode %v", perm, mode), func(t *testing.T) {
				must := require.New(t)
				dir := filepath.Join(t.TempDir(), "judge-cache")
				must.NoError(os.Mkdir(dir, 0o700))
				must.NoError(os.Chmod(dir, perm))

				_, err := NewFileCache(dir, FileCacheOptions{Mode: mode})
				must.ErrorContains(err, "lets other users write")
			})
		}
	}
	for _, perm := range []os.FileMode{0o700, 0o750, 0o755} {
		for _, mode := range cacheModesWithADirectory {
			t.Run(fmt.Sprintf("accepts perm %04o mode %v", perm, mode), func(t *testing.T) {
				must := require.New(t)
				dir := t.TempDir()
				must.NoError(os.Chmod(dir, perm))

				_, err := NewFileCache(dir, FileCacheOptions{Mode: mode})
				must.NoError(err)
			})
		}
	}
}

func TestNewFileCache_RefusesDirectoryOwnedByAnotherUser(t *testing.T) {
	owner := fileOwner
	t.Cleanup(func() { fileOwner = owner })
	fileOwner = func(fs.FileInfo) (int, bool) { return os.Geteuid() + 1, true }

	for _, mode := range cacheModesWithADirectory {
		t.Run(fmt.Sprintf("mode %v", mode), func(t *testing.T) {
			must := require.New(t)
			_, err := NewFileCache(t.TempDir(), FileCacheOptions{Mode: mode})
			must.ErrorContains(err, "not the current user")
		})
	}
}

func TestFileCache_KeepsDirectoriesAndEntriesPrivate(t *testing.T) {
	for _, umask := range []int{0o022, 0o077, 0o002} {
		t.Run(fmt.Sprintf("umask %04o", umask), func(t *testing.T) {
			must := require.New(t)
			old := syscall.Umask(umask)
			t.Cleanup(func() { syscall.Umask(old) })

			dir := filepath.Join(t.TempDir(), "judge-cache")
			c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheRecord})
			must.NoError(err)
			key := CacheKey("c", "i", 0)
			must.NoError(c.Put(key, fileCacheVerdict("x")))

			for _, tc := range []struct {
				name string
				path string
				want os.FileMode
			}{
				{"cache directory", dir, 0o700},
				{"first shard directory", filepath.Join(dir, key[:2]), 0o700},
				{"second shard directory", filepath.Join(dir, key[:2], key[2:4]), 0o700},
				{"entry file", filepath.Join(dir, key[:2], key[2:4], key+".json"), 0o600},
			} {
				t.Run(tc.name, func(t *testing.T) {
					must := require.New(t)
					info, err := os.Stat(tc.path)
					must.NoError(err)
					must.Equal(tc.want, info.Mode().Perm())
				})
			}
		})
	}
}

// entryPath returns key's entry file under dir after creating its shard
// directories.
func entryPath(t *testing.T, dir, key string) string {
	t.Helper()
	must := require.New(t)
	shard := filepath.Join(dir, key[:2], key[2:4])
	must.NoError(os.MkdirAll(shard, 0o700))
	return filepath.Join(shard, key+".json")
}

func TestFileCache_FIFOEntryIsUnusableWithoutBlocking(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)

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
		must.True(r.found)
		must.ErrorContains(r.err, "not a regular file")
	case <-time.After(2 * time.Second):
		must.FailNow("Get blocked on a FIFO entry")
	}
}

func TestFileCache_SymlinkedEntryIsUnusable(t *testing.T) {
	must := require.New(t)
	plantDir := t.TempDir()
	writer, err := NewFileCache(plantDir, FileCacheOptions{Mode: CacheRecord})
	must.NoError(err)
	key := CacheKey("c", "i", 0)
	must.NoError(writer.Put(key, fileCacheVerdict("planted")))

	dir := t.TempDir()
	c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
	must.NoError(err)
	must.NoError(os.Symlink(filepath.Join(plantDir, key[:2], key[2:4], key+".json"), entryPath(t, dir, key)))

	_, found, err := c.Get(key)
	must.True(found)
	must.Error(err)
}

func TestFileCache_SymlinkedShardDirectoryIsRefused(t *testing.T) {
	for _, level := range []int{1, 2} {
		t.Run(fmt.Sprintf("level %d", level), func(t *testing.T) {
			must := require.New(t)
			dir := t.TempDir()
			c, err := NewFileCache(dir, FileCacheOptions{Mode: CacheReadThrough})
			must.NoError(err)

			key := CacheKey("c", "i", 0)
			target := t.TempDir()
			link := filepath.Join(dir, key[:2])
			if level == 2 {
				must.NoError(os.Mkdir(link, 0o700))
				link = filepath.Join(link, key[2:4])
			}
			must.NoError(os.Symlink(target, link))

			must.ErrorContains(c.Put(key, fileCacheVerdict("x")), "symbolic link")

			_, found, err := c.Get(key)
			must.True(found)
			must.Error(err)

			entries, err := os.ReadDir(target)
			must.NoError(err)
			must.Empty(entries)
		})
	}
}
