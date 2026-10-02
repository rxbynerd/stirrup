//go:build unix

package judge

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
