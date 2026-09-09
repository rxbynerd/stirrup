package executor

import "context"

const (
	// SandboxIdentityTokenDir is the in-sandbox directory backing the
	// sandbox identity token file. It sits outside the workspace so the
	// token is unreachable through ResolvePath (read_file), is never swept
	// into a git commit or a workspace export, and each executor backs it
	// with a private memory-backed mount so it never touches the workspace
	// bind mount or node disk.
	SandboxIdentityTokenDir = "/run/stirrup/sandbox-identity"

	// SandboxIdentityTokenPath is the file the composed git credential
	// helper reads the token from; refreshes replace it in place.
	SandboxIdentityTokenPath = SandboxIdentityTokenDir + "/token"

	// sandboxIdentityTokenMountBytes sizes the memory-backed mount: a token
	// is capped at 16 KiB (sandboxidentity.MaxTokenBytes) and the directory
	// holds at most the live file plus one staging file.
	sandboxIdentityTokenMountBytes = 64 * 1024
)

// sandboxIdentityWriteCommand is the in-sandbox command every executor runs
// to deliver a token: the token arrives on stdin (never in argv, which the
// Docker daemon records on the exec instance and the Kubernetes API server
// records in its audit log), umask 077 yields mode 0600, and the rename
// makes the replacement atomic so a credential-helper read racing a refresh
// observes the old or the new token, never a torn one.
var sandboxIdentityWriteCommand = []string{
	"sh", "-c",
	"umask 077 && cat > " + SandboxIdentityTokenDir + "/.token.tmp && mv -f " + SandboxIdentityTokenDir + "/.token.tmp " + SandboxIdentityTokenPath,
}

// SandboxIdentityTokenWriter is the optional capability for delivering a
// sandbox identity token into a running sandbox at SandboxIdentityTokenPath.
// The factory requires it whenever executor.sandboxIdentity is configured
// and fails closed when the executor does not provide it. Token bytes must
// travel in a request body or exec stdin, never in a command line.
type SandboxIdentityTokenWriter interface {
	WriteSandboxIdentityToken(ctx context.Context, token string) error
}
