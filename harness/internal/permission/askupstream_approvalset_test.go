package permission

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAskUpstream_ApprovalToolNamesContainsExpectedTools: the policy
// must prompt on every tool that requires upstream approval — not just
// workspace-mutating tools.
func TestAskUpstream_ApprovalToolNamesContainsExpectedTools(t *testing.T) {
	must := require.New(t)
	approval := map[string]bool{
		"write_file":  true,
		"run_command": true,
		"web_fetch":   true,
		"spawn_agent": true,
	}
	policy := NewAskUpstreamPolicy(&mockTransport{}, approval, 100*time.Millisecond)

	got := policy.ApprovalToolNames()
	sort.Strings(got)

	want := []string{"run_command", "spawn_agent", "web_fetch", "write_file"}
	must.Equal(want, got)
}
