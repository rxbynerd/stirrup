package executor

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

const (
	// workspaceReadNoReadlinkExit and workspaceReadEscapeExit are the exit
	// statuses workspaceReadScript reserves for its own failures; tar
	// itself exits 0, 1, 2, or 64.
	workspaceReadNoReadlinkExit = 112
	workspaceReadEscapeExit     = 113

	// tarSingleEntryOverhead bounds the bytes a single-entry archive adds
	// around the file content: one 512-byte header, padding to the next
	// 512-byte boundary, and the two zero blocks that end the archive.
	tarSingleEntryOverhead = 512 + 511 + 1024
)

// workspaceReadScript streams one workspace file out as a single-entry tar
// archive after resolving its real path inside the sandbox and refusing
// anything that leaves the workspace ($1). Engine archive endpoints and
// tar both follow a symlink committed into the workspace (tar on a named
// path resolves symlinked intermediate directories), which would let
// read_file reach any file the sandbox uid can read, the sandbox identity
// token included. tar then runs on the resolved path so the bytes come
// from the file the check saw. An image without readlink fails closed
// rather than skipping the check; a path readlink cannot resolve (a
// missing file) falls through to tar, which reports the ordinary
// not-found error.
var workspaceReadScript = fmt.Sprintf(`command -v readlink >/dev/null 2>&1 || { echo "readlink is required for workspace reads" >&2; exit %d; }
r=$(readlink -f -- "$2" 2>/dev/null)
case "$r" in
  "") ;;
  "$1"/*) ;;
  *) echo "path escapes workspace: $2" >&2; exit %d ;;
esac
t=${r:-$2}
exec tar -C / -cf - -- "${t#/}"`, workspaceReadNoReadlinkExit, workspaceReadEscapeExit)

// workspaceReadCommand is the argv both sandbox executors run for a
// workspace read; workspace and resolved are absolute in-sandbox paths.
func workspaceReadCommand(workspace, resolved string) []string {
	return []string{"sh", "-c", workspaceReadScript, "sh", workspace, resolved}
}

// classifyWorkspaceReadExit maps a non-zero exit of workspaceReadCommand to
// an error, reporting an escape through the security emitter the same way
// a textual traversal is reported.
func classifyWorkspaceReadExit(code int, stderr, filePath, workspace string, security SecurityEventEmitter) error {
	switch code {
	case workspaceReadNoReadlinkExit:
		return fmt.Errorf("read file %s: the sandbox image lacks readlink, which workspace reads require", filePath)
	case workspaceReadEscapeExit:
		if security != nil {
			security.PathTraversalBlocked(filePath, workspace)
		}
		return fmt.Errorf("path escapes workspace: %s", filePath)
	default:
		return classifyTarError(filePath, stderr)
	}
}

var (
	errArchiveEntryIsDir    = errors.New("is a directory")
	errArchiveEntryTooLarge = errors.New("exceeds the file size cap")
)

// decodeSingleFileArchive extracts the one regular file a workspace read
// streams back. A symlink entry can only mean readlink could not resolve
// it, so its target is missing. size reports the entry's declared or
// observed size alongside errArchiveEntryTooLarge.
func decodeSingleFileArchive(archive []byte, limit int64) (content string, size int64, err error) {
	tr := tar.NewReader(bytes.NewReader(archive))
	header, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return "", 0, fs.ErrNotExist
	}
	if err != nil {
		return "", 0, fmt.Errorf("read tar header: %w", err)
	}
	switch header.Typeflag {
	case tar.TypeDir:
		return "", 0, errArchiveEntryIsDir
	case tar.TypeSymlink:
		return "", 0, fs.ErrNotExist
	}
	if header.Size > limit {
		return "", header.Size, errArchiveEntryTooLarge
	}
	// One byte past the cap makes an over-cap payload detectable by length
	// even when the header under-reported it (that read may surface as
	// io.ErrUnexpectedEOF).
	data, err := io.ReadAll(io.LimitReader(tr, limit+1))
	if int64(len(data)) > limit || (errors.Is(err, io.ErrUnexpectedEOF) && int64(len(data)) >= limit) {
		return "", int64(len(data)), errArchiveEntryTooLarge
	}
	if err != nil {
		return "", 0, fmt.Errorf("read file from tar: %w", err)
	}
	return string(data), int64(len(data)), nil
}
