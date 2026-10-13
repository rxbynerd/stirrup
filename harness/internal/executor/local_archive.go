package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ArchiveWorkspace walks the host workspace directory and writes a tar of
// every regular file, directory and symlink beneath it. Symlinks are
// recorded unfollowed, but one that is dangling or resolves outside the
// workspace fails the archive (see
// docs/security.md#path-traversal-prevention); devices, FIFOs and sockets
// are skipped.
func (e *LocalExecutor) ArchiveWorkspace(ctx context.Context, w io.Writer, maxBytes int64) (ArchiveStats, error) {
	info, err := os.Stat(e.workspace)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ArchiveStats{}, fmt.Errorf("archive workspace %s: %w", e.workspace, ErrWorkspaceRootMissing)
		}
		return ArchiveStats{}, fmt.Errorf("archive workspace %s: %w", e.workspace, err)
	}
	if !info.IsDir() {
		return ArchiveStats{}, fmt.Errorf("archive workspace %s: not a directory", e.workspace)
	}

	out := &archiveCapWriter{w: w, max: maxBytes}
	tw := tar.NewWriter(out)
	walkErr := filepath.WalkDir(e.workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeHostTarEntry(tw, e.workspace, path, d)
	})
	if walkErr == nil {
		walkErr = tw.Close()
	}
	stats := ArchiveStats{Bytes: out.written}
	if out.exceeded {
		return stats, fmt.Errorf("archive workspace %s: %w (%d bytes)", e.workspace, ErrArchiveTooLarge, maxBytes)
	}
	if walkErr != nil {
		return stats, fmt.Errorf("archive workspace %s: %w", e.workspace, walkErr)
	}
	return stats, nil
}

// writeHostTarEntry writes the tar entry for one walked path under root,
// which must already be absolute and symlink-resolved.
func writeHostTarEntry(tw *tar.Writer, root, path string, d fs.DirEntry) error {
	archiveName, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("relative path %q: %w", path, err)
	}
	if archiveName == "." {
		return nil
	}
	if relEscapes(archiveName) {
		return fmt.Errorf("refusing path %q: escapes workspace root", path)
	}

	// A dangling symlink is refused rather than skipped: it could otherwise
	// carry a symlink-then-overwrite target past a downstream untar that
	// dereferences links.
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("refusing path %q: cannot resolve symlink target: %w", path, err)
	}
	if rel, err := filepath.Rel(root, resolvedPath); err != nil || relEscapes(rel) {
		return fmt.Errorf("refusing path %q: resolves outside workspace root", path)
	}

	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	name := filepath.ToSlash(archiveName)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("readlink %q: %w", path, err)
		}
		return tw.WriteHeader(&tar.Header{
			Name:     name,
			Linkname: target,
			Mode:     int64(info.Mode().Perm()),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeSymlink,
		})
	case info.IsDir():
		return tw.WriteHeader(&tar.Header{
			Name:     name + "/",
			Mode:     int64(info.Mode().Perm()),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeDir,
		})
	case info.Mode().IsRegular():
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %q: %w", path, copyErr)
		}
	}
	return nil
}

// relEscapes reports whether a filepath.Rel result leaves its base. Unlike
// a plain ".." prefix test it accepts names such as "..cache".
func relEscapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
