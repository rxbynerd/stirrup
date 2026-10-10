package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// maxDegradedErrorBytes bounds the error text recorded on a
// DegradedCapability, which is copied to the trace, the RunResult and the
// transport.
const maxDegradedErrorBytes = 512

// connectMCPServers connects every configured server. A required server
// that fails to connect, or that does not advertise every allowlisted tool,
// aborts construction with an error naming it. An optional server that
// fails is skipped (or, for a partial tool list, kept with the tools it did
// advertise); it is logged, announced as a transport "warning" event, and
// returned as a DegradedCapability.
func connectMCPServers(
	ctx context.Context,
	client *mcp.Client,
	servers []types.MCPServerConfig,
	secrets security.SecretStore,
	tp transport.Transport,
	logger *slog.Logger,
) ([]types.DegradedCapability, error) {
	var degraded []types.DegradedCapability
	for _, srv := range servers {
		err := client.Connect(ctx, srv, secrets)
		if err == nil {
			continue
		}
		if !srv.Optional {
			return nil, fmt.Errorf("required MCP server %q unavailable: %w", srv.Name, err)
		}

		reason := boundDegradedError(err)
		message := fmt.Sprintf("optional MCP server %q unavailable; continuing without its tools: %s", srv.Name, reason)
		var missing *mcp.MissingToolsError
		if errors.As(err, &missing) {
			message = fmt.Sprintf("optional MCP server %q is missing allowed tools; continuing with the rest: %s", srv.Name, reason)
		}
		logger.Warn("optional MCP server degraded", "server", srv.Name, "error", reason)
		if emitErr := tp.Emit(types.HarnessEvent{Type: "warning", Message: message}); emitErr != nil {
			logger.Warn("transport emit failed", "event", "warning", "error", emitErr)
		}
		degraded = append(degraded, types.DegradedCapability{
			Kind:  types.DegradedCapabilityMCP,
			Name:  srv.Name,
			Error: reason,
		})
	}
	return degraded, nil
}

// boundDegradedError scrubs err's text and truncates it to
// maxDegradedErrorBytes on a rune boundary.
func boundDegradedError(err error) string {
	text, _ := types.CapFinalAssistantText(security.Scrub(err.Error()), maxDegradedErrorBytes)
	return text
}
