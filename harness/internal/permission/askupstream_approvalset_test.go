package permission

import (
	"sort"
	"testing"
	"time"
)

// TestAskUpstream_ApprovalToolNamesContainsExpectedTools is the WP1
// regression test for ask-upstream wiring: the policy must prompt on
// every tool that requires upstream approval — not just workspace-mutating
// tools. The expected set covers writes (write_file), shell execution
// (run_command), network (web_fetch), and sub-agent spawning
// (spawn_agent).
func TestAskUpstream_ApprovalToolNamesContainsExpectedTools(t *testing.T) {
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
	if len(got) != len(want) {
		t.Fatalf("ApprovalToolNames length = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("ApprovalToolNames[%d] = %q, want %q", i, got[i], name)
		}
	}
}
