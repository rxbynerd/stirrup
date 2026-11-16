package git

// Identity recorded as author and committer on every harness-made commit.
// Workspaces and sandbox images often carry no git identity, and git
// refuses to commit without one.
const (
	HarnessIdentityName  = "stirrup"
	HarnessIdentityEmail = "stirrup@noreply.local"
)

// HardeningFlags are the configuration overrides every harness-issued git
// command carries, as argv elements. They keep harness git operations
// deterministic against repository-local configuration the agent can
// rewrite (hooks, fsmonitor, signing, auto-maintenance). They are not a
// security boundary: inside the sandbox the agent already holds the same
// authority as these commands.
var HardeningFlags = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "commit.gpgsign=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// sandboxHomeFlags make the deterministic strategy ignore the global ignore
// and attributes files under a sandbox's HOME, which the agent can write
// and which would otherwise change what a commit takes without leaving a
// trace in the workspace. They are not applied under the local executor,
// whose HOME is the operator's and whose global ignores protect files such
// as .env from being committed.
var sandboxHomeFlags = []string{
	"-c", "core.excludesFile=/dev/null",
	"-c", "core.attributesFile=/dev/null",
}

// IdentityFlags returns the argv elements that pin the harness identity for
// one git invocation.
func IdentityFlags() []string {
	return []string{
		"-c", "user.name=" + HarnessIdentityName,
		"-c", "user.email=" + HarnessIdentityEmail,
	}
}
