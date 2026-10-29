package runner

import (
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// dryRunFixtureURL stands in for a fixture server's URL when a dry run
// validates the merged config without starting the server.
const dryRunFixtureURL = "http://127.0.0.1:0"

// applyFixtureServer points the merged config at a fixture server. An sse
// server becomes the provider's base URL, and an mcp server becomes the URI
// of every tools.mcpServers entry that has none. The target must be unset
// so the suite never names an endpoint the runner would overwrite.
func applyFixtureServer(cfg *types.RunConfig, fixture types.FixtureServerConfig, url string) error {
	switch fixture.Mode {
	case types.FixtureModeSSE:
		if cfg.Provider.BaseURL != "" {
			return fmt.Errorf("fixture_server mode %q supplies provider.base_url; the task must leave it unset, got %q", fixture.Mode, cfg.Provider.BaseURL)
		}
		cfg.Provider.BaseURL = url
		return nil
	case types.FixtureModeMCP:
		filled := 0
		for i := range cfg.Tools.MCPServers {
			if cfg.Tools.MCPServers[i].URI == "" {
				cfg.Tools.MCPServers[i].URI = url
				filled++
			}
		}
		if filled == 0 {
			return fmt.Errorf("fixture_server mode %q supplies a tools.mcp_server uri; the task must declare an mcp_server with no uri", fixture.Mode)
		}
		return nil
	default:
		return fmt.Errorf("unknown fixture_server mode %q", fixture.Mode)
	}
}

// withoutProviderFlags returns cfg minus every flag that selects or
// authenticates a live model. An sse fixture task runs against its own
// server, so these flags, which retarget the other tasks, must not reach it.
func withoutProviderFlags(cfg RunConfig) RunConfig {
	cfg.Model = ""
	cfg.PromptModel = ""
	cfg.Provider = ""
	cfg.BaseURL = ""
	cfg.APIKeyRef = ""
	cfg.AnthropicWIF = AnthropicWIFConfig{}
	return cfg
}
