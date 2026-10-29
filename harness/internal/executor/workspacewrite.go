package executor

import (
	"fmt"
	"path"
	"strings"
)

const (
	// workspaceWriteNoReadlinkExit and workspaceWriteEscapeExit are the exit
	// statuses workspaceWriteGuardScript reserves for its own failures;
	// mkdir exits 1.
	workspaceWriteNoReadlinkExit = 112
	workspaceWriteEscapeExit     = 113
)

// workspaceWriteGuardScript resolves where a write to $2 would land inside
// the sandbox, creates its parent directories, and prints the resolved
// file path, refusing anything that leaves the workspace ($1). Engine
// archive endpoints, tar -C, and mkdir -p all follow a symlink committed
// into the workspace, so without the guard a write could land anywhere
// the writing uid can reach. The deepest existing ancestor is checked
// before mkdir -p so a symlinked intermediate directory cannot make mkdir
// create directories elsewhere. A destination that is itself a symlink is
// written through to its target only when that target stays inside the
// workspace, matching the local executor. An image without readlink fails
// closed rather than skipping the check.
var workspaceWriteGuardScript = fmt.Sprintf(`command -v readlink >/dev/null 2>&1 || { echo "readlink is required for workspace writes" >&2; exit %[1]d; }
w=$1 f=$2 d=${2%%/*}
a=$d
while [ -n "$a" ] && [ ! -e "$a" ] && [ ! -L "$a" ]; do a=${a%%/*}; done
r=$(readlink -f -- "${a:-/}" 2>/dev/null)
case "$r" in
  "$w"|"$w"/*) ;;
  *) echo "path escapes workspace: $f" >&2; exit %[2]d ;;
esac
mkdir -p -- "$d" || exit 1
p=$(readlink -f -- "$d" 2>/dev/null)
case "$p" in
  "$w"|"$w"/*) ;;
  *) echo "path escapes workspace: $f" >&2; exit %[2]d ;;
esac
t=$p/${f##*/}
if [ -L "$t" ]; then
  t=$(readlink -f -- "$t" 2>/dev/null)
  case "$t" in
    "$w"/*) ;;
    *) echo "path escapes workspace: $f" >&2; exit %[2]d ;;
  esac
fi
printf '%%s\n' "$t"`, workspaceWriteNoReadlinkExit, workspaceWriteEscapeExit)

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
	if target == "" || path.Clean(target) != target || !strings.HasPrefix(target, workspace+"/") {
		return "", workspaceWriteEscape(filePath, workspace, security)
	}
	return target, nil
}

func workspaceWriteEscape(filePath, workspace string, security SecurityEventEmitter) error {
	if security != nil {
		security.PathTraversalBlocked(filePath, workspace)
	}
	return fmt.Errorf("path escapes workspace: %s", filePath)
}
