package workspaceexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// packedArchive is a validated, gzipped workspace tarball.
type packedArchive struct {
	gz    []byte
	files int
}

// archiveWorkspace streams src's tar archive through repackArchive. The
// producer runs concurrently behind a pipe so neither side buffers the raw
// stream; a repack failure closes the pipe and cancels the producer.
func archiveWorkspace(ctx context.Context, src executor.WorkspaceArchiver, maxBytes int64) (packedArchive, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		_, err := src.ArchiveWorkspace(ctx, pw, maxBytes)
		_ = pw.CloseWithError(err)
		produced <- err
	}()

	in := &cappedReader{r: pr, max: maxBytes}
	packed, repackErr := repackArchive(in, maxBytes)
	if repackErr == nil {
		// tar pads its output past the end-of-archive marker; drain it so
		// the producer can finish writing and report its own status.
		_, repackErr = io.Copy(io.Discard, in)
	}
	if repackErr != nil {
		_ = pr.CloseWithError(repackErr)
		cancel()
	}
	produceErr := <-produced

	// A producer failure reaches repackArchive through the pipe, so a
	// repack error that wraps it is a symptom; report the cause.
	if produceErr != nil && (repackErr == nil || errors.Is(repackErr, produceErr)) {
		return packedArchive{}, produceErr
	}
	if repackErr != nil {
		return packedArchive{}, repackErr
	}
	return packed, nil
}

// repackArchive re-reads an untrusted tar stream and writes a gzipped copy
// holding only directories, regular files, symlinks and hard links, with
// permission bits and modification times but no ownership. It refuses
// absolute names, ".." components, symlinks whose target is absolute or
// leaves the root, hard links to names outside the archive root or beneath
// a symlink, and any entry beneath an earlier symlink entry, so the archive cannot write
// outside the extraction directory. Symlinks are never followed.
func repackArchive(r io.Reader, maxBytes int64) (packedArchive, error) {
	var buf bytes.Buffer
	out := &cappedWriter{w: &buf, max: maxBytes}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	tr := tar.NewReader(r)

	symlinks := map[string]bool{}
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return packedArchive{}, fmt.Errorf("read workspace archive: %w", err)
		}
		name, err := cleanEntryName(hdr.Name)
		if err != nil {
			return packedArchive{}, err
		}
		if name == "" {
			continue
		}
		if parent := symlinkAncestor(name, symlinks); parent != "" {
			return packedArchive{}, fmt.Errorf("refusing archive entry %q: beneath symlink %q", hdr.Name, parent)
		}

		h := &tar.Header{
			Name:    name,
			Mode:    hdr.Mode & 0o777,
			ModTime: hdr.ModTime,
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			h.Typeflag = tar.TypeDir
			h.Name = name + "/"
		case tar.TypeReg:
			h.Typeflag = tar.TypeReg
			h.Size = hdr.Size
			files++
		case tar.TypeSymlink:
			if err := checkSymlinkTarget(name, hdr.Linkname); err != nil {
				return packedArchive{}, fmt.Errorf("refusing archive entry %q: %w", hdr.Name, err)
			}
			h.Typeflag = tar.TypeSymlink
			h.Linkname = hdr.Linkname
			symlinks[name] = true
			files++
		case tar.TypeLink:
			target, err := cleanEntryName(hdr.Linkname)
			if err != nil || target == "" || symlinkAncestor(target, symlinks) != "" {
				return packedArchive{}, fmt.Errorf("refusing archive entry %q: hard link target %q is outside the workspace", hdr.Name, hdr.Linkname)
			}
			h.Typeflag = tar.TypeLink
			h.Linkname = target
			files++
		default:
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return packedArchive{}, out.wrap(fmt.Errorf("write archive entry %q: %w", name, err))
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return packedArchive{}, out.wrap(fmt.Errorf("copy archive entry %q: %w", name, err))
			}
		}
	}
	if err := tw.Close(); err != nil {
		return packedArchive{}, out.wrap(fmt.Errorf("close tar: %w", err))
	}
	if err := gz.Close(); err != nil {
		return packedArchive{}, out.wrap(fmt.Errorf("close gzip: %w", err))
	}
	return packedArchive{gz: buf.Bytes(), files: files}, nil
}

// cleanEntryName normalises a tar entry name to a slash-separated path
// relative to the archive root, returning "" for the root itself. It
// rejects absolute names and ".." components.
func cleanEntryName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refusing archive entry %q: absolute path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("refusing archive entry %q: parent directory component", name)
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}

// checkSymlinkTarget rejects a symlink target that is absolute or that,
// resolved lexically from the link's directory, leaves the archive root.
func checkSymlinkTarget(name, target string) error {
	if target == "" {
		return errors.New("empty symlink target")
	}
	if strings.HasPrefix(target, "/") {
		return fmt.Errorf("symlink target %q is absolute", target)
	}
	resolved := path.Join(path.Dir(name), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink target %q resolves outside the workspace", target)
	}
	return nil
}

// symlinkAncestor returns the first proper ancestor of name recorded as a
// symlink entry, or "".
func symlinkAncestor(name string, symlinks map[string]bool) string {
	for i := strings.IndexByte(name, '/'); i >= 0; {
		if symlinks[name[:i]] {
			return name[:i]
		}
		next := strings.IndexByte(name[i+1:], '/')
		if next < 0 {
			break
		}
		i += next + 1
	}
	return ""
}

// cappedReader fails with executor.ErrArchiveTooLarge once more than max
// bytes have been read.
type cappedReader struct {
	r    io.Reader
	max  int64
	read int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.max {
		return n, tooLarge(c.max)
	}
	return n, err
}

// cappedWriter refuses writes that would take the total past max.
type cappedWriter struct {
	w        io.Writer
	max      int64
	written  int64
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.exceeded || c.written+int64(len(p)) > c.max {
		c.exceeded = true
		return 0, tooLarge(c.max)
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// wrap reports a cap breach in place of err when one occurred, since the
// tar and gzip writers do not always surface the underlying write error.
func (c *cappedWriter) wrap(err error) error {
	if c.exceeded && !errors.Is(err, executor.ErrArchiveTooLarge) {
		return tooLarge(c.max)
	}
	return err
}

func tooLarge(limit int64) error {
	return fmt.Errorf("%w (%d bytes)", executor.ErrArchiveTooLarge, limit)
}
