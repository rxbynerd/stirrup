package git

// Identity recorded as author and committer on every harness-made commit.
// Workspaces and sandbox images often carry no git identity, and git
// refuses to commit without one.
const (
	HarnessIdentityName  = "stirrup"
	HarnessIdentityEmail = "stirrup@noreply.local"
)

// IdentityFlags returns the argv elements that pin the harness identity for
// one git invocation.
func IdentityFlags() []string {
	return []string{
		"-c", "user.name=" + HarnessIdentityName,
		"-c", "user.email=" + HarnessIdentityEmail,
	}
}
