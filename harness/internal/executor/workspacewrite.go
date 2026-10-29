package executor

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

const (
	// workspaceWriteNoReadlinkExit, workspaceWriteEscapeExit, and
	// workspaceWriteIsDirExit are the exit statuses workspaceWriteGuardScript
	// reserves for its own failures; mkdir exits 1.
	workspaceWriteNoReadlinkExit = 112
	workspaceWriteEscapeExit     = 113
	workspaceWriteIsDirExit      = 114

	// workspaceWriteGuardOutputLimit bounds what the guard's output streams
	// may buffer; it prints one path or one error line.
	workspaceWriteGuardOutputLimit = 64 << 10
)

// workspaceWriteGuardScript creates the parent directories of $2 and prints
// the symlink-free path a write to it must use, refusing anything that
// resolves outside the workspace ($1): archive endpoints, tar -C, and mkdir
// -p all follow symlinks. canon keeps the trailing newlines command
// substitution strips; the final walk refuses any symlink left in the path
// whatever readlink printed. In-workspace symlinks are written through.
var workspaceWriteGuardScript = fmt.Sprintf(`command -v readlink >/dev/null 2>&1 || { echo "readlink is required for workspace writes" >&2; exit %[1]d; }
nl='
'
w=$1 f=$2 d=${2%%/*}
escape() { echo "path escapes workspace: $f" >&2; exit %[2]d; }
canon() {
  c=$(readlink -f -- "$1" 2>/dev/null && echo .)
  c=${c%%.}
  c=${c%%?}
  case "$c" in ""|*"$nl"*) escape ;; esac
}
a=$d
while [ -n "$a" ] && [ ! -e "$a" ] && [ ! -L "$a" ]; do a=${a%%/*}; done
canon "${a:-/}"
case "$c" in "$w"|"$w"/*) ;; *) escape ;; esac
mkdir -p -- "$d" || exit 1
canon "$d"
case "$c" in "$w"|"$w"/*) ;; *) escape ;; esac
t=$c/${f##*/}
if [ -L "$t" ]; then
  canon "$t"
  t=$c
  case "$t" in "$w"/*) ;; *) escape ;; esac
fi
q=$t
while [ -n "$q" ] && [ "$q" != "$w" ]; do
  if [ -L "$q" ]; then escape; fi
  q=${q%%/*}
done
if [ -d "$t" ]; then echo "path is a directory, not a file: $f" >&2; exit %[3]d; fi
printf '%%s\n' "$t"`, workspaceWriteNoReadlinkExit, workspaceWriteEscapeExit, workspaceWriteIsDirExit)

// workspaceWriteGuardCommand is the argv both sandbox executors run before
// a workspace write; workspace and target are absolute in-sandbox paths.
func workspaceWriteGuardCommand(workspace, target string) []string {
	return []string{"sh", "-c", workspaceWriteGuardScript, "sh", workspace, target}
}

// classifyWorkspaceWriteExit maps a non-zero exit of
// workspaceWriteGuardCommand to an error, reporting an escape through the
// security emitter the same way a textual traversal is reported.
func classifyWorkspaceWriteExit(code int, stderr, filePath, workspace string, security SecurityEventEmitter) error {
	switch code {
	case workspaceWriteNoReadlinkExit:
		return fmt.Errorf("write file %s: the sandbox image lacks readlink, which workspace writes require", filePath)
	case workspaceWriteEscapeExit:
		return workspaceWriteEscape(filePath, workspace, security)
	case workspaceWriteIsDirExit:
		return fmt.Errorf("path is a directory, not a file: %s", filePath)
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("write file %s: %s", filePath, msg)
	}
	return fmt.Errorf("write file %s: guard exited %d", filePath, code)
}

// parseWorkspaceWriteTarget validates the path the guard printed, so a
// write only ever goes to a clean path strictly inside workspace.
func parseWorkspaceWriteTarget(stdout, filePath, workspace string, security SecurityEventEmitter) (string, error) {
	target := strings.TrimSuffix(stdout, "\n")
	if target == "" || path.Clean(target) != target || !strings.HasPrefix(target, workspace+"/") ||
		strings.IndexFunc(target, unicode.IsControl) >= 0 {
		return "", workspaceWriteEscape(filePath, workspace, security)
	}
	return target, nil
}

// checkWorkspaceWritePath refuses a sandbox write path carrying a control
// character, which no legitimate workspace file name needs and which shell
// tooling and the engines handle inconsistently.
func checkWorkspaceWritePath(filePath string) error {
	if strings.IndexFunc(filePath, unicode.IsControl) >= 0 {
		return fmt.Errorf("path contains a control character: %q", filePath)
	}
	return nil
}

func workspaceWriteEscape(filePath, workspace string, security SecurityEventEmitter) error {
	if security != nil {
		security.PathTraversalBlocked(filePath, workspace)
	}
	return fmt.Errorf("path escapes workspace: %s", filePath)
}
