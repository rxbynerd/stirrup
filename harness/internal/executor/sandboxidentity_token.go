package executor

import (
	"context"
	"fmt"
	"strconv"
)

const (
	// SandboxIdentityTokenDir is the in-sandbox directory backing the
	// sandbox identity token file. It sits outside the workspace, and
	// workspace reads refuse any path that resolves outside it
	// (workspaceReadScript), so read_file cannot reach the token even
	// through a symlink committed into the workspace; it is never swept
	// into a git commit or a workspace export; and each executor backs it
	// with a private memory-backed mount so it never touches the workspace
	// bind mount or node disk.
	SandboxIdentityTokenDir = "/run/stirrup/sandbox-identity"

	// SandboxIdentityTokenPath is the file the composed git credential
	// helper reads the token from; refreshes replace it in place.
	SandboxIdentityTokenPath = SandboxIdentityTokenDir + "/token"

	sandboxIdentityTokenStaging = SandboxIdentityTokenDir + "/.token.tmp"

	// sandboxIdentityTokenMountBytes sizes the memory-backed mount: a token
	// is capped at 16 KiB (sandboxidentity.MaxTokenBytes) and the directory
	// holds at most the live file plus one staging file.
	sandboxIdentityTokenMountBytes = 64 * 1024

	// sandboxIdentityIncompleteExit is the exit status
	// sandboxIdentityWriteScript reserves for a stream that ended short of
	// the announced length.
	sandboxIdentityIncompleteExit = 4
)

// sandboxIdentityWriteScript is the in-sandbox command every executor runs
// to deliver a token ($1 final path, $2 staging path, $3 expected byte
// length). The token arrives on stdin, never in argv, which the Docker
// daemon records on the exec instance and the Kubernetes API server
// records in its audit log; the byte length is not sensitive and rides in
// argv because cat cannot tell a short stream from a complete one, so the
// rename only happens when the staged file matches it. umask 077 yields
// mode 0600, and the rename makes the replacement atomic so a
// credential-helper read racing a refresh observes the old or the new
// token, never a torn one. A short or failed write removes the staging
// file and leaves the previous token in place.
var sandboxIdentityWriteScript = fmt.Sprintf(
	`umask 077 && cat > "$2" && [ "$(wc -c < "$2" | tr -d ' ')" -eq "$3" ] && mv -f -- "$2" "$1" || { rm -f -- "$2"; echo "sandbox identity token delivery was incomplete; the previous token is left in place" >&2; exit %d; }`,
	sandboxIdentityIncompleteExit)

// sandboxIdentityWriteCommand is the argv for delivering a token of
// tokenLen bytes.
func sandboxIdentityWriteCommand(tokenLen int) []string {
	return []string{"sh", "-c", sandboxIdentityWriteScript, "sh", SandboxIdentityTokenPath, sandboxIdentityTokenStaging, strconv.Itoa(tokenLen)}
}

// SandboxIdentityTokenWriter is the optional capability for delivering a
// sandbox identity token into a running sandbox at SandboxIdentityTokenPath.
// The factory requires it whenever executor.sandboxIdentity is configured
// and fails closed when the executor does not provide it. Token bytes must
// travel in a request body or exec stdin, never in a command line.
type SandboxIdentityTokenWriter interface {
	WriteSandboxIdentityToken(ctx context.Context, token string) error
}
