package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestDenyAll_DeniesEverything: deny-all rejects mutating and
// non-mutating tools alike — the property that distinguishes it from
// deny-side-effects and makes it the right fallback for a permit-based
// allow-list (a non-matching web_fetch must not fall through to allow).
func TestDenyAll_DeniesEverything(t *testing.T) {
	p := NewDenyAll()
	for _, name := range []string{"write_file", "run_command", "read_file", "web_fetch", "spawn_agent"} {
		result, err := p.Check(context.Background(), types.ToolDefinition{Name: name}, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Check(%s): %v", name, err)
		}
		if result.Allowed {
			t.Errorf("deny-all allowed %s", name)
		}
		if result.Reason == "" {
			t.Errorf("deny-all denial of %s carries no reason", name)
		}
	}
}
