package workspaceexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// streamArchiver is a WorkspaceArchiver that writes a fixed stream and then
// returns err, standing in for a sandbox executor's tar output.
type streamArchiver struct {
	stream []byte
	err    error
}

func (s streamArchiver) ArchiveWorkspace(_ context.Context, w io.Writer, _ int64) (executor.ArchiveStats, error) {
	n, err := w.Write(s.stream)
	if err != nil {
		return executor.ArchiveStats{Bytes: int64(n)}, err
	}
	return executor.ArchiveStats{Bytes: int64(n)}, s.err
}

// endlessArchiver writes zero blocks until its writer fails, ignoring the
// byte cap it is given.
type endlessArchiver struct{}

func (endlessArchiver) ArchiveWorkspace(ctx context.Context, w io.Writer, _ int64) (executor.ArchiveStats, error) {
	block := make([]byte, 32<<10)
	var total int64
	for ctx.Err() == nil {
		n, err := w.Write(block)
		total += int64(n)
		if err != nil {
			return executor.ArchiveStats{Bytes: total}, err
		}
	}
	return executor.ArchiveStats{Bytes: total}, ctx.Err()
}

func tarStream(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = "content of " + h.Name
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader %q: %v", h.Name, err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("write %q: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func readUploaded(t *testing.T, body []byte) []*tar.Header {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gzr)
	var out []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		out = append(out, h)
	}
}

// TestGCSExporter_EmptyWorkspaceUploadsEmptyArchive pins that a workspace
// with no files still uploads a readable, entry-free archive and reports
// status empty, which counts as delivered.
func TestGCSExporter_EmptyWorkspaceUploadsEmptyArchive(t *testing.T) {
	cases := map[string]executor.WorkspaceArchiver{
		"local executor":     localArchiver(t, t.TempDir()),
		"sandbox tar stream": streamArchiver{stream: tarStream(t, &tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &captureUploadServer{}
			exp := newTestExporter(t, srv)
			receipt, err := exp.Export(context.Background(), src, "gs://b/o.tar.gz")
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if receipt.Status != types.ExportStatusEmpty || receipt.Files != 0 || !receipt.Delivered() {
				t.Errorf("receipt = %+v, want a delivered empty receipt", receipt)
			}
			if len(srv.requests) != 1 {
				t.Fatalf("uploads = %d, want 1", len(srv.requests))
			}
			if got := readUploaded(t, srv.last().Body); len(got) != 0 {
				t.Errorf("empty archive has %d entries", len(got))
			}
		})
	}
}

// TestGCSExporter_MissingWorkspaceRoot pins that a missing root fails the
// export with nothing uploaded, for the host walker and a sandbox stream.
func TestGCSExporter_MissingWorkspaceRoot(t *testing.T) {
	gone := localArchiver(t, t.TempDir())
	if err := os.Remove(gone.HostWorkspaceRoot()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]executor.WorkspaceArchiver{
		"local executor": gone,
		"sandbox":        streamArchiver{err: fmt.Errorf("archive workspace /workspace: %w", executor.ErrWorkspaceRootMissing)},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &captureUploadServer{}
			exp := newTestExporter(t, srv)
			receipt, err := exp.Export(context.Background(), src, "gs://b/o.tar.gz")
			if !errors.Is(err, executor.ErrWorkspaceRootMissing) {
				t.Fatalf("Export error = %v, want ErrWorkspaceRootMissing", err)
			}
			if receipt.Status != types.ExportStatusFailed || receipt.Delivered() {
				t.Errorf("receipt = %+v, want failed", receipt)
			}
			if len(srv.requests) != 0 {
				t.Errorf("uploads = %d, want 0", len(srv.requests))
			}
		})
	}
}

// TestGCSExporter_RepacksSandboxStream pins how an untrusted tar stream is
// rewritten: "./" prefixes dropped, ownership and setuid bits stripped,
// in-root symlinks and hard links kept unfollowed, devices and FIFOs
// skipped.
func TestGCSExporter_RepacksSandboxStream(t *testing.T) {
	mtime := time.Unix(1_700_000_000, 0)
	stream := tarStream(t,
		&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./src/", Typeflag: tar.TypeDir, Mode: 0o750},
		&tar.Header{Name: "./src/main.go", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 1000, Gid: 1000, Uname: "agent", Gname: "agent", ModTime: mtime},
		&tar.Header{Name: "./src/alias.go", Typeflag: tar.TypeSymlink, Linkname: "main.go"},
		&tar.Header{Name: "./src/up", Typeflag: tar.TypeSymlink, Linkname: "../src/main.go"},
		&tar.Header{Name: "./hard", Typeflag: tar.TypeLink, Linkname: "./src/main.go"},
		&tar.Header{Name: "./fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
		&tar.Header{Name: "./null", Typeflag: tar.TypeChar, Mode: 0o666},
	)
	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)
	receipt, err := exp.Export(context.Background(), streamArchiver{stream: stream}, "gs://b/o.tar.gz")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if receipt.Status != types.ExportStatusUploaded || receipt.Files != 4 {
		t.Errorf("receipt = %+v, want uploaded with 4 files", receipt)
	}

	got := map[string]*tar.Header{}
	var names []string
	for _, h := range readUploaded(t, srv.last().Body) {
		got[h.Name] = h
		names = append(names, h.Name)
	}
	want := []string{"src/", "src/main.go", "src/alias.go", "src/up", "hard"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	main := got["src/main.go"]
	if main.Uid != 0 || main.Gid != 0 || main.Uname != "" || main.Gname != "" {
		t.Errorf("ownership kept: uid=%d gid=%d uname=%q gname=%q", main.Uid, main.Gid, main.Uname, main.Gname)
	}
	if main.Mode != 0o755 || !main.ModTime.Equal(mtime) {
		t.Errorf("main.go mode=%o mtime=%v, want 0755 and %v", main.Mode, main.ModTime, mtime)
	}
	if got["src/alias.go"].Typeflag != tar.TypeSymlink || got["src/alias.go"].Linkname != "main.go" {
		t.Errorf("alias.go = %+v, want an unfollowed symlink", got["src/alias.go"])
	}
	if got["hard"].Typeflag != tar.TypeLink || got["hard"].Linkname != "src/main.go" {
		t.Errorf("hard = %+v, want a hard link to src/main.go", got["hard"])
	}
}

// TestGCSExporter_RefusesHostileSandboxStream pins that entries which could
// write outside the extraction directory fail the export with nothing
// uploaded.
func TestGCSExporter_RefusesHostileSandboxStream(t *testing.T) {
	cases := map[string][]*tar.Header{
		"absolute path":             {{Name: "/etc/passwd", Typeflag: tar.TypeReg}},
		"parent component":          {{Name: "../outside.txt", Typeflag: tar.TypeReg}},
		"nested parent component":   {{Name: "./a/../../outside.txt", Typeflag: tar.TypeReg}},
		"absolute symlink target":   {{Name: "./etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"}},
		"escaping symlink target":   {{Name: "./sub/up", Typeflag: tar.TypeSymlink, Linkname: "../../secret"}},
		"empty symlink target":      {{Name: "./empty", Typeflag: tar.TypeSymlink}},
		"entry beneath a symlink":   {{Name: "./dir", Typeflag: tar.TypeSymlink, Linkname: "sub"}, {Name: "./dir/file", Typeflag: tar.TypeReg}},
		"hard link outside root":    {{Name: "./h", Typeflag: tar.TypeLink, Linkname: "../etc/shadow"}},
		"absolute hard link target": {{Name: "./h", Typeflag: tar.TypeLink, Linkname: "/etc/shadow"}},
		"hard link through symlink": {{Name: "./dir", Typeflag: tar.TypeSymlink, Linkname: "sub"}, {Name: "./h", Typeflag: tar.TypeLink, Linkname: "./dir/f"}},
		"entry before its symlink parent": {
			{Name: "./dir/file", Typeflag: tar.TypeReg},
			{Name: "./dir", Typeflag: tar.TypeSymlink, Linkname: "sub"},
		},
		"symlink chain climbs through a self link": {
			{Name: "./s", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "./t", Typeflag: tar.TypeSymlink, Linkname: "s/.."},
		},
		"symlink chain climbs through a later self link": {
			{Name: "./t", Typeflag: tar.TypeSymlink, Linkname: "s/.."},
			{Name: "./s", Typeflag: tar.TypeSymlink, Linkname: "."},
		},
		"symlink chain climbs through a parent link": {
			{Name: "./a/", Typeflag: tar.TypeDir},
			{Name: "./a/b", Typeflag: tar.TypeSymlink, Linkname: ".."},
			{Name: "./x", Typeflag: tar.TypeSymlink, Linkname: "a/b/../secret"},
		},
		"symlink chain climbs through two links": {
			{Name: "./a/", Typeflag: tar.TypeDir},
			{Name: "./a/b", Typeflag: tar.TypeSymlink, Linkname: ".."},
			{Name: "./t", Typeflag: tar.TypeSymlink, Linkname: "a/b"},
			{Name: "./d/", Typeflag: tar.TypeDir},
			{Name: "./d/u", Typeflag: tar.TypeSymlink, Linkname: "../t/.."},
		},
		"symlink loop": {
			{Name: "./a", Typeflag: tar.TypeSymlink, Linkname: "b"},
			{Name: "./b", Typeflag: tar.TypeSymlink, Linkname: "a"},
		},
		"regular file after same-named symlink": {
			{Name: "./f", Typeflag: tar.TypeSymlink, Linkname: "g"},
			{Name: "./f", Typeflag: tar.TypeReg},
		},
		"duplicate regular file": {
			{Name: "./f", Typeflag: tar.TypeReg},
			{Name: "f", Typeflag: tar.TypeReg},
		},
		"hard link to an escaping symlink": {
			{Name: "./up", Typeflag: tar.TypeSymlink, Linkname: "../secret"},
			{Name: "./h", Typeflag: tar.TypeLink, Linkname: "./up"},
		},
		"hard link to an in-root symlink": {
			{Name: "./f", Typeflag: tar.TypeReg},
			{Name: "./l", Typeflag: tar.TypeSymlink, Linkname: "f"},
			{Name: "./h", Typeflag: tar.TypeLink, Linkname: "./l"},
		},
		"hard link to a missing name": {{Name: "./h", Typeflag: tar.TypeLink, Linkname: "./absent"}},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &captureUploadServer{}
			exp := newTestExporter(t, srv)
			receipt, err := exp.Export(context.Background(), streamArchiver{stream: tarStream(t, headers...)}, "gs://b/o.tar.gz")
			if err == nil || !strings.Contains(err.Error(), "refusing archive entry") {
				t.Fatalf("Export error = %v, want a refusal", err)
			}
			if receipt.Status != types.ExportStatusFailed {
				t.Errorf("receipt status = %q, want failed", receipt.Status)
			}
			if len(srv.requests) != 0 {
				t.Errorf("uploads = %d, want 0", len(srv.requests))
			}
		})
	}
}

// TestRepackArchive_KeepsInRootLinkChains pins that symlinks resolving
// through other symlinks are kept when every resolution stays in the root.
func TestRepackArchive_KeepsInRootLinkChains(t *testing.T) {
	stream := tarStream(t,
		&tar.Header{Name: "./t", Typeflag: tar.TypeSymlink, Linkname: "s/a/c/file"},
		&tar.Header{Name: "./s", Typeflag: tar.TypeSymlink, Linkname: "."},
		&tar.Header{Name: "./a/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./a/b", Typeflag: tar.TypeSymlink, Linkname: ".."},
		&tar.Header{Name: "./a/c/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./a/c/file", Typeflag: tar.TypeReg, Mode: 0o644},
		&tar.Header{Name: "./a/c/up", Typeflag: tar.TypeSymlink, Linkname: "../b/a"},
		&tar.Header{Name: "./x", Typeflag: tar.TypeSymlink, Linkname: "a/b/a/c/../c/file"},
		&tar.Header{Name: "./dangling", Typeflag: tar.TypeSymlink, Linkname: "a/missing"},
		&tar.Header{Name: "./h", Typeflag: tar.TypeLink, Linkname: "a/c/file"},
	)
	packed, err := repackArchive(bytes.NewReader(stream), 1<<20)
	if err != nil {
		t.Fatalf("repackArchive: %v", err)
	}
	if packed.files != 8 {
		t.Errorf("files = %d, want 8", packed.files)
	}
}

// TestArchiveWorkspace_EnforcesCap pins that a stream past the cap fails
// even when the producer ignores the cap it was given.
func TestArchiveWorkspace_EnforcesCap(t *testing.T) {
	big := tarStream(t, &tar.Header{Name: "big", Typeflag: tar.TypeReg})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	payload := bytes.Repeat([]byte("z"), 256<<10)
	_ = tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: int64(len(payload)), Mode: 0o644})
	_, _ = tw.Write(payload)
	_ = tw.Close()

	cases := map[string]executor.WorkspaceArchiver{
		"oversized entry":   streamArchiver{stream: buf.Bytes()},
		"endless producer":  endlessArchiver{},
		"padding after EOF": streamArchiver{stream: append(big, make([]byte, 128<<10)...)},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := archiveWorkspace(context.Background(), src, 64<<10)
			if !errors.Is(err, executor.ErrArchiveTooLarge) {
				t.Fatalf("archiveWorkspace error = %v, want ErrArchiveTooLarge", err)
			}
		})
	}
}

// TestArchiveWorkspace_ProducerErrorAfterStream pins that a producer which
// writes a complete archive but then fails (tar exiting non-zero) fails
// the export.
func TestArchiveWorkspace_ProducerErrorAfterStream(t *testing.T) {
	tarFailed := errors.New("archive workspace /workspace: tar exited with status 2")
	src := streamArchiver{stream: tarStream(t, &tar.Header{Name: "./a", Typeflag: tar.TypeReg}), err: tarFailed}
	if _, err := archiveWorkspace(context.Background(), src, 1<<20); !errors.Is(err, tarFailed) {
		t.Fatalf("archiveWorkspace error = %v, want the producer's error", err)
	}
}

// TestGCSExporter_LocalArchiveMatchesHostTree exports a host tree through
// the local executor and checks the upload against it.
func TestGCSExporter_LocalArchiveMatchesHostTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "deep/b.txt"), "b")
	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)
	receipt, err := exp.Export(context.Background(), localArchiver(t, dir), "gs://b/o.tar.gz")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if receipt.Files != 2 {
		t.Errorf("receipt.Files = %d, want 2", receipt.Files)
	}
	var names []string
	for _, h := range readUploaded(t, srv.last().Body) {
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "a.txt,deep/,deep/b.txt" {
		t.Errorf("entries = %v", names)
	}
}
