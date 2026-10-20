package permission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestDenyAll_DeniesEverything: deny-all rejects mutating and
// non-mutating tools alike — the property that distinguishes it from
// deny-side-effects and makes it the right fallback for a permit-based
// allow-list (a non-matching web_fetch must not fall through to allow).
func TestDenyAll_DeniesEverything(t *testing.T) {
	p := NewDenyAll()
	for _, name := range []string{"write_file", "run_command", "read_file", "web_fetch", "spawn_agent"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			result, err := p.Check(context.Background(), types.ToolDefinition{Name: name}, json.RawMessage(`{}`))
			must.NoError(err)
			must.False(result.Allowed, "deny-all allowed the tool")
			must.NotEmpty(result.Reason, "deny-all denial carries no reason")
		})
	}
}
