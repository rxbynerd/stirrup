package core

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/httpsput"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// checkHTTPSPutDestinations resolves every HTTPS PUT destination the
// config names, so a reference that does not resolve to a usable URL
// fails construction instead of the upload at the end of the run. The
// uploads resolve again when they happen. Nothing is contacted.
func checkHTTPSPutDestinations(ctx context.Context, config *types.RunConfig, secrets security.SecretStore) error {
	if target := config.Executor.WorkspaceExportHTTPSPut; target != nil {
		if _, err := httpsput.Resolve(ctx, secrets, target); err != nil {
			return fmt.Errorf("executor.workspaceExportHttpsPut: %w", err)
		}
	}
	return nil
}
