package core

import (
	"context"
	"errors"
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
			return httpsPutDestinationError("executor.workspaceExportHttpsPut", err)
		}
	}
	if sink := config.ResultSink; sink != nil && sink.Type == "https-put" {
		if _, err := httpsput.Resolve(ctx, secrets, sink.HTTPSPut); err != nil {
			return httpsPutDestinationError("resultSink.httpsPut", err)
		}
	}
	if config.TraceEmitter.Type == "https-put" {
		if _, err := httpsput.Resolve(ctx, secrets, config.TraceEmitter.HTTPSPut); err != nil {
			return fmt.Errorf("traceEmitter.httpsPut: %w", err)
		}
	}
	return nil
}

// httpsPutDestinationError prefixes a destination's resolve failure with
// field. The failure of a file-backed reference is a HostFileError, so a
// control plane cannot use the reference to learn whether a host file
// exists, is readable, or holds a URL.
func httpsPutDestinationError(field string, err error) error {
	var refErr *httpsput.RefError
	if errors.As(err, &refErr) && security.IsFileSecretRef(refErr.Ref) {
		return &HostFileError{Field: field + "." + refErr.Field, Err: err}
	}
	return fmt.Errorf("%s: %w", field, err)
}
